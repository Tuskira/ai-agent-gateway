package llmplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/discovery"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	pkgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Recorder durably captures one record per call. Implemented by
// internal/capture.Recorder; the plane depends only on this method, so it
// imports neither the store nor a concrete sink.
type Recorder interface {
	Record(ctx context.Context, c *pkgsink.LLMCall) error
}

// router is the shared round-trip for every provider: read the bounded body, pick
// a provider, build the upstream request, relay the response frame-by-frame while
// teeing a bounded copy aside, parse usage, and durably record one captured row.
type router struct {
	// clientIP derives the caller's address for capture; never nil.
	clientIP       func(*http.Request) string
	byID           map[string]Provider // provider ID → provider, for /{id}/ prefix routing
	recorder       Recorder
	client         *http.Client
	storeBodies    bool
	maxCaptureReq  int // bounded storage cap for the request body
	maxCaptureResp int // bounded storage cap for the response body
	pricing        *pricing.Card
	// registry resolves the requested model against the tenant's model
	// registry; nil resolves nothing (pure passthrough, as before).
	registry *Registry
	// bedrock signs registry targets of vendor "bedrock" whatever the
	// client's dialect. It is the same instance routed for /model/* when
	// Bedrock is enabled, and exists (unrouted) even when it is not, so an
	// Anthropic-dialect alias can still land on Bedrock.
	bedrock *bedrockProvider
	limiter *Limiter // per-key limits; nil = none enforced
}

// callInfo is what the router learned about a call for capture: the
// client's dialect, what it asked for, and where the call actually went.
type callInfo struct {
	provider        string // client dialect (Provider.Name)
	pricingProvider string // pkg/pricing key the response is costed under
	requestedModel  string // the "model" the client sent, verbatim
	resolvedVendor  string // registry target's label, else its vendor type; "" when unregistered
	resolvedModel   string // registry target model id, "" when unregistered
	translated      bool   // the body was translated between wire formats
	fallbackIndex   int    // index of the target that answered; 0 unregistered; -1 none
	region          string // Bedrock region the call was signed for
	price           *store.ModelPrice
}

// model is what the capture row's "model" column carries: the vendor model
// actually called when the registry resolved it, else the requested name.
func (ci callInfo) model() string {
	if ci.resolvedModel != "" {
		return ci.resolvedModel
	}
	return ci.requestedModel
}

// retryableStatus reports whether an upstream response means "try the next
// target": the upstream is unavailable or throttled (nothing about the
// request itself is wrong). A 4xx other than 408/429 is the request's
// fault and is relayed from the first target that says so.
func retryableStatus(code int) bool {
	return code >= 500 || code == http.StatusTooManyRequests || code == http.StatusRequestTimeout
}

// record durably captures the call OFF the request's lifecycle: it uses a fresh
// context (not r.Context(), which may already be cancelled by a client
// disconnect or the stream deadline) so capture still commits. A failure is
// logged loudly — the record is never silently dropped.
func (rt *router) record(call *pkgsink.LLMCall) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.recorder.Record(ctx, call); err != nil {
		slog.Error("llmplane: capture failed (possible data loss)", "request_id", call.RequestID, "error", err)
	}
	// After the durable write: a limits refresh racing this call can then
	// only count it twice (conservative), never miss it.
	if rt.limiter != nil {
		rt.limiter.observe(call.TenantID, call.KeyID, call.RequestedModel, call.Timestamp, call.CostUSD)
	}
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := requestIDOf(r.Context())
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, reqID, "request body too large")
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, reqID, "cannot read request body")
		return
	}

	p, upstreamPath := pickProvider(rt.byID, r)
	if p == nil {
		writeAnthropicError(w, http.StatusNotFound, reqID, "no provider for route "+r.URL.Path)
		return
	}
	model := p.Model(body, upstreamPath)
	info := callInfo{provider: p.Name(), pricingProvider: p.Name(), requestedModel: model}

	// Per-key limits, before anything is forwarded. A denial is still
	// captured (status + error) so it shows in the LLM logs.
	if rt.limiter != nil {
		if d := rt.limiter.check(r.Context(), tenantOf(r.Context()), keyIDOf(r.Context()), body); d != nil {
			info.fallbackIndex = -1 // no target was tried
			rt.emitError(r, info, body, start, d.status, d.capture)
			d.write(w, reqID)
			return
		}
	}

	// Capture keeps the client's own body; only the upstream copy may carry the
	// injected stream_options (see streamUsageInjector).
	upBody, injected := body, false
	if si, ok := p.(streamUsageInjector); ok {
		upBody, injected = si.InjectStreamUsage(body, upstreamPath)
	}

	// Model registry: a hit swaps the upstream (host, credential, region,
	// model id) for each of the row's targets, in order -- translating the
	// call for a target of another wire format; a miss is the byte-for-byte
	// passthrough this plane has always done.
	route, err := rt.resolve(r.Context(), p, model, upstreamPath, tenantOf(r.Context()))
	if err != nil {
		slog.Error("llmplane: model registry lookup failed", "request_id", reqID, "model", model, "error", err)
		rt.emitError(r, info, body, start, http.StatusBadGateway, "registry_error: "+errorText(err))
		writeAnthropicError(w, http.StatusBadGateway, reqID, "model registry unavailable")
		return
	}
	// Model-level limits, once the name is known to be a registered row:
	// the row's own budgets and caps, per tenant, on top of the key's.
	if route != nil && rt.limiter != nil {
		if d := rt.limiter.checkModel(r.Context(), tenantOf(r.Context()), route.model, body); d != nil {
			info.fallbackIndex = -1 // no target was tried
			rt.emitError(r, info, body, start, d.status, d.capture)
			d.write(w, reqID)
			return
		}
	}
	if route != nil && len(route.attempts) == 0 {
		if len(route.noCredential) > 0 {
			rt.refuseNoCredential(w, r, info, body, start, route.noCredential[0])
			return
		}
		rt.refuseUntranslatable(w, r, info, body, start)
		return
	}

	// One builder per attempt: the passthrough alone on a miss, else the
	// route's targets. Building is deferred so a credential is fetched only
	// for a target that is actually tried.
	type attemptBuilder func() (*upstreamAttempt, error)
	var builders []attemptBuilder
	var indexes []int
	if route == nil {
		builders = append(builders, func() (*upstreamAttempt, error) {
			up, err := p.BuildUpstream(r.Context(), r, upBody, upstreamPath)
			if err != nil {
				return nil, err
			}
			return &upstreamAttempt{req: up, pricingProvider: p.Name()}, nil
		})
		indexes = append(indexes, 0)
	} else {
		info.price = route.model.Price
		for _, t := range route.attempts {
			t := t
			builders = append(builders, func() (*upstreamAttempt, error) {
				var creds map[string]string
				if t.Credential != "" {
					var err error
					if creds, err = rt.registry.credential(r.Context(), tenantOf(r.Context()), t.Credential); err != nil {
						return nil, err
					}
				}
				if t.tr != nil {
					// The engine parses the client's own body; the
					// stream-usage injection is the native path's.
					return rt.buildTarget(r.Context(), p, r, body, upstreamPath, t, creds)
				}
				return rt.buildTarget(r.Context(), p, r, upBody, upstreamPath, t, creds)
			})
			indexes = append(indexes, t.index)
		}
	}

	// Fallback: try each target in order; move on only while nothing has
	// reached the client -- a build failure, a dial/TLS error, or a
	// retryable status (429/408/5xx) BEFORE its headers are relayed. The
	// last target's outcome is what the client gets, whatever it is.
	var (
		resp    *http.Response
		att     *upstreamAttempt
		lastErr error
	)
	for i, build := range builders {
		last := i == len(builders)-1
		if route != nil {
			t := route.attempts[i]
			info.resolvedVendor, info.resolvedModel = vendorOf(t.ModelTarget), t.Model
			info.fallbackIndex, info.translated = indexes[i], t.tr != nil
		}
		a, err := build()
		if err != nil {
			lastErr = err
			if !last {
				slog.Warn("llmplane: model registry target failed to build; trying next", "request_id", reqID, "model", model, "target", indexes[i], "error", errorText(err))
				continue
			}
			var ce clientError
			if errors.As(err, &ce) {
				rt.emitError(r, info, body, start, http.StatusBadRequest, errorText(err))
				writeAnthropicError(w, http.StatusBadRequest, reqID, ce.msg)
				return
			}
			rt.emitError(r, info, body, start, http.StatusBadGateway, errorText(err))
			writeAnthropicError(w, http.StatusBadGateway, reqID, "upstream request build failed")
			return
		}
		info.pricingProvider, info.region = a.pricingProvider, a.region
		if a.estimate != nil {
			rt.serveEstimate(w, r, info, *a.estimate, start)
			return
		}

		got, err := rt.client.Do(a.req)
		if err != nil {
			// Before any response header: the stream deadline, the client leaving
			// (499, nothing reaches it), or the upstream failing.
			switch ctxErr := r.Context().Err(); {
			case errors.Is(ctxErr, context.DeadlineExceeded):
				rt.emitError(r, info, body, start, http.StatusGatewayTimeout, "stream_deadline")
				writeAnthropicError(w, http.StatusGatewayTimeout, reqID, "request deadline exceeded")
				return
			case errors.Is(ctxErr, context.Canceled):
				rt.emitError(r, info, body, start, 499, "client_closed")
				return
			}
			lastErr = err
			if !last {
				slog.Warn("llmplane: model registry target unreachable; trying next", "request_id", reqID, "model", model, "target", indexes[i], "error", errorText(err))
				continue
			}
			rt.emitError(r, info, body, start, http.StatusBadGateway, "upstream_error: "+errorText(lastErr))
			writeAnthropicError(w, http.StatusBadGateway, reqID, "upstream error")
			return
		}
		if !last && retryableStatus(got.StatusCode) {
			slog.Warn("llmplane: model registry target answered retryable status; trying next", "request_id", reqID, "model", model, "target", indexes[i], "status", got.StatusCode)
			_, _ = io.Copy(io.Discard, io.LimitReader(got.Body, 64<<10))
			got.Body.Close()
			continue
		}
		resp, att = got, a
		break
	}
	// Before the deferred Close, which must close the adapted body (a
	// translated one stops the translating goroutine), not the upstream one
	// it wraps.
	if a := att; a != nil && a.adapt != nil {
		a.adapt(resp)
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// Tee: the client gets the full stream (flushed per frame). A small head+tail
	// pair feeds usage parsing independently of body storage — input tokens live
	// in the first frame (message_start), output in the last (message_delta) — so
	// token/cost accounting is correct even when the response far exceeds
	// max_response_bytes. The storage copy exists only when bodies are stored.
	// The usage-only chunk is removed from the client's copy only for a streamed
	// 2xx that declares no Content-Length. A Content-Length means the upstream
	// counted bytes we are about to drop, so stripping would relay a body
	// shorter than the length the client was promised — safer to pass the
	// stream through intact (the chunk is harmless; the capture parses it
	// either way).
	client := newFlushWriter(w)
	var toClient io.Writer = client
	var strip *usageChunkStripper
	if injected && !att.translated && resp.StatusCode/100 == 2 && resp.Header.Get("Content-Length") == "" &&
		strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		strip = &usageChunkStripper{w: client}
		toClient = strip
	}
	uHead, uTail := newBoundedCap(usageScanBytes), newTailCap(usageScanBytes)
	dst, cap := captureTargets(rt.storeBodies, rt.maxCaptureResp, toClient, uHead, uTail)
	// Skill/MCP discovery reads the same bytes the client gets (the
	// client-dialect body, for a translated target), independent of body
	// storage. Only a 2xx can carry a tool call; the scanner cannot fail or
	// block the relay (see internal/discovery).
	var disc *discovery.Scanner
	if resp.StatusCode/100 == 2 {
		disc = discovery.NewScanner()
		dst = append(dst, disc)
	}
	_, copyErr := io.Copy(io.MultiWriter(dst...), resp.Body)
	if strip != nil && copyErr == nil {
		copyErr = strip.Flush()
	}

	parseUsage := p.ParseUsage
	if att.parseUsage != nil {
		parseUsage = att.parseUsage
	}
	usage := mergeUsage(parseUsage(uHead.Bytes()), parseUsage(uTail.Bytes()))
	if hu, ok := p.(headerUsager); ok && !att.translated && usage.empty() {
		usage = mergeUsage(usage, hu.HeaderUsage(resp.Header))
	}
	tier, speed, geo := requestHints(body)
	usage.ServiceTier, usage.Speed = firstNonEmpty(usage.ServiceTier, tier), firstNonEmpty(usage.Speed, speed)
	usage.InferenceGeo = firstNonEmpty(usage.InferenceGeo, geo)
	var found discovery.Result
	if disc != nil {
		found = disc.Result()
	}
	rt.emit(r, info, body, start, resp, cap, usage, found, relayError(r.Context(), copyErr, client.err))
}

// captureTargets returns the tee writers: base, plus a storage copy (bounded by
// limit) only when bodies are stored — otherwise no response buffer exists.
func captureTargets(store bool, limit int, base ...io.Writer) ([]io.Writer, *boundedCap) {
	if !store {
		return base, nil
	}
	cap := newBoundedCap(limit)
	return append(base, cap), cap
}

// relayError classifies a failed relay: the stream deadline, the client going
// away, or the upstream failing mid-body. "" when the relay completed.
func relayError(ctx context.Context, copyErr, clientErr error) string {
	switch {
	case copyErr == nil:
		return ""
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "stream_deadline"
	case clientErr != nil || errors.Is(ctx.Err(), context.Canceled):
		return "client_closed"
	default:
		return "upstream_error: " + errorText(copyErr)
	}
}

// maxUserAgentLen bounds sink.LLMCall.UserAgent (see pkgsink.CapUserAgent):
// only the classification prefix matters for analytics, so a caller cannot
// bloat a capture row by sending an oversized header.
const maxUserAgentLen = pkgsink.MaxUserAgentLen

// clientInfo reads r's raw User-Agent (capped at maxUserAgentLen) and
// classifies it into the gateway's named client family via the same
// substring rule the MCP plane's requestsByClient grouping uses
// (pkgsink.ClientFamily), so a caller classifies identically on both
// planes. clientName/userAgent are both "" when the caller sent no
// User-Agent header at all.
func clientInfo(r *http.Request) (clientName, userAgent string) {
	ua := pkgsink.CapUserAgent(r.Header.Get("User-Agent"))
	return pkgsink.ClientFamily(ua), ua
}

func (rt *router) emit(r *http.Request, info callInfo, reqBody []byte, start time.Time, resp *http.Response, cap *boundedCap, usage Usage, found discovery.Result, relayErr string) {
	ctx := r.Context()
	// Prefer the provider's own request id from the response headers (Anthropic
	// `request-id`, Bedrock `x-amzn-requestid`, OpenAI `x-request-id`); fall back
	// to the parsed body id.
	if h := providerRequestID(resp.Header); h != "" {
		usage.ProviderRequestID = h
	}
	reqBodyCapped, reqBodyCut := pkgsink.TruncateBody(reqBody, rt.maxCaptureReq)
	reqTruncated := rt.storeBodies && reqBodyCut
	headers := make(map[string]string, len(r.Header)+len(resp.Header))
	maskInto(headers, r.Header)
	maskInto(headers, resp.Header)
	clientName, userAgent := clientInfo(r)
	call := &pkgsink.LLMCall{
		Timestamp:           start,
		RequestID:           requestIDOf(ctx),
		TenantID:            tenantOf(ctx),
		Principal:           principalOf(ctx),
		KeyID:               keyIDOf(ctx),
		SessionID:           sessionOf(ctx),
		ClientIP:            rt.callerIP(r),
		ClientName:          clientName,
		UserAgent:           userAgent,
		Provider:            info.provider,
		Model:               info.model(),
		RequestedModel:      info.requestedModel,
		ResolvedVendor:      info.resolvedVendor,
		ResolvedModel:       info.resolvedModel,
		Translated:          info.translated,
		FallbackIndex:       info.fallbackIndex,
		Path:                r.URL.Path,
		StatusCode:          resp.StatusCode,
		DurationMS:          time.Since(start).Milliseconds(),
		Stream:              streamFromRequest(reqBody, r.URL.Path, resp),
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		CacheReadTokens:     usage.CacheReadTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		StopReason:          usage.StopReason,
		ProviderRequestID:   usage.ProviderRequestID,
		UpstreamHost:        resp.Request.URL.Host,
		Headers:             headers,
		Truncated:           reqTruncated || (cap != nil && cap.Truncated),
		SkillsUsed:          found.Skills,
		MCPToolsUsed:        found.MCPTools,
		Error:               relayErr,
	}
	if cap != nil {
		call.RequestBody = reqBodyCapped
		call.ResponseBody = cap.Bytes()
		call.Messages, call.System, call.Tools = parseRequestParts(reqBody)
		// The parsed parts are slices of the request body, so they obey the
		// same per-body cap (the console renders a cut part as truncated).
		var cut1, cut2, cut3 bool
		call.Messages, cut1 = pkgsink.TruncateBody(call.Messages, rt.maxCaptureReq)
		call.System, cut2 = pkgsink.TruncateBody(call.System, rt.maxCaptureReq)
		call.Tools, cut3 = pkgsink.TruncateBody(call.Tools, rt.maxCaptureReq)
		call.Truncated = call.Truncated || (rt.storeBodies && (cut1 || cut2 || cut3))
	}
	// Freeze the dollar cost from the parsed tokens + the per-model rate card.
	// Free utility endpoints are not priced, and neither is a successful call
	// whose usage could not be read (e.g. an OpenAI stream without
	// stream_options.include_usage): NULL means unknown, $0 would claim it was
	// free. An error response with no usage is correctly $0. Pure map lookup,
	// can never fail.
	// A registry row's own price wins over the rate card; the vendor model
	// actually called (not the alias) is what the card is looked up by.
	usageUnknown := resp.StatusCode/100 == 2 && usage.empty()
	if !unpricedPath(r.URL.Path) && !usageUnknown {
		u := pricing.Usage{
			Input:             usage.InputTokens,
			Output:            usage.OutputTokens,
			CacheRead:         usage.CacheReadTokens,
			CacheCreation:     usage.CacheCreationTokens,
			CacheCreation1h:   usage.CacheCreation1hTokens,
			WebSearchRequests: usage.WebSearchRequests,
			Region:            firstNonEmpty(info.region, bedrockRegionOf(info.pricingProvider, resp)),
			Tier:              pricingTier(usage.ServiceTier),
			Fast:              usage.Speed == "fast",
			GeoUS:             usage.InferenceGeo == "us",
		}
		if info.price != nil {
			call.CostUSD = pricing.CostOverride(info.pricingProvider, pricing.Override{
				Input: info.price.Input, Output: info.price.Output, CacheRead: info.price.CacheRead, CacheWrite: info.price.CacheWrite,
			}, u)
		} else {
			call.CostUSD, _ = rt.pricing.Cost(info.pricingProvider, info.model(), u)
		}
	}
	rt.record(call)
}

// emitError records failed upstream attempts so they are not invisible.
func (rt *router) emitError(r *http.Request, info callInfo, reqBody []byte, start time.Time, status int, errText string) {
	ctx := r.Context()
	clientName, userAgent := clientInfo(r)
	call := &pkgsink.LLMCall{
		Timestamp:      start,
		RequestID:      requestIDOf(ctx),
		TenantID:       tenantOf(ctx),
		Principal:      principalOf(ctx),
		KeyID:          keyIDOf(ctx),
		SessionID:      sessionOf(ctx),
		ClientIP:       rt.callerIP(r),
		ClientName:     clientName,
		UserAgent:      userAgent,
		Provider:       info.provider,
		Model:          info.model(),
		RequestedModel: info.requestedModel,
		ResolvedVendor: info.resolvedVendor,
		ResolvedModel:  info.resolvedModel,
		Translated:     info.translated,
		FallbackIndex:  info.fallbackIndex,
		Path:           r.URL.Path,
		StatusCode:     status,
		DurationMS:     time.Since(start).Milliseconds(),
		Error:          errText,
	}
	rt.record(call)
}

// usageScanBytes bounds the head and tail usage buffers (each keeps this many
// bytes) — enough to hold the first frame (message_start / model id) and the
// final usage frame (message_delta / trailing usage object), independent of the
// body-storage cap.
const usageScanBytes = 64 * 1024

// mergeUsage combines usage parsed from the response head and tail. Count fields
// are cumulative and non-negative, so max() takes each from whichever side
// carries it (input/cache from the head's message_start, output from the tail's
// message_delta); stop reason and provider id take the first non-empty. Every
// Usage field must appear here, or it is lost before pricing.
func mergeUsage(a, b Usage) Usage {
	return Usage{
		InputTokens:           max(a.InputTokens, b.InputTokens),
		OutputTokens:          max(a.OutputTokens, b.OutputTokens),
		CacheReadTokens:       max(a.CacheReadTokens, b.CacheReadTokens),
		CacheCreationTokens:   max(a.CacheCreationTokens, b.CacheCreationTokens),
		CacheCreation1hTokens: max(a.CacheCreation1hTokens, b.CacheCreation1hTokens),
		WebSearchRequests:     max(a.WebSearchRequests, b.WebSearchRequests),
		ServiceTier:           firstNonEmpty(a.ServiceTier, b.ServiceTier),
		Speed:                 firstNonEmpty(a.Speed, b.Speed),
		InferenceGeo:          firstNonEmpty(a.InferenceGeo, b.InferenceGeo),
		StopReason:            firstNonEmpty(a.StopReason, b.StopReason),
		ProviderRequestID:     firstNonEmpty(a.ProviderRequestID, b.ProviderRequestID),
	}
}

// unpricedPath reports free utility endpoints — token counting on each provider
// and Anthropic's Message Batches management/results API (batch work is billed
// by the batch, not by these calls; results JSONL would otherwise price its
// last line as a full-rate call). The batches match is on whole path SEGMENTS:
// a bare substring test also swallowed anything merely PREFIXED by it
// (/v1/messages/batches-export), silently zeroing a billable call.
func unpricedPath(path string) bool {
	return strings.HasSuffix(path, "/count_tokens") || // Anthropic
		strings.HasSuffix(path, "/count-tokens") || // Bedrock CountTokens
		strings.HasSuffix(path, ":countTokens") || // Gemini
		strings.HasSuffix(path, "/input_tokens") || // OpenAI Responses input-token count
		strings.HasSuffix(path, "/messages/batches") || // Anthropic Message Batches: the collection
		strings.Contains(path, "/messages/batches/") // ... and anything under one batch
}

// pricingTier maps a provider service tier onto the rate card's tier rows; the
// default/standard/auto/scale tiers bill the standard rate.
func pricingTier(t string) string {
	switch t = strings.ToLower(t); t {
	case "priority", "flex":
		return t
	}
	return ""
}

// bedrockRegionOf returns the AWS region a Bedrock call was served from (the
// upstream host bedrock-runtime.<region>.amazonaws.com), "" for other providers.
func bedrockRegionOf(provider string, resp *http.Response) string {
	if provider != "bedrock" || resp.Request == nil {
		return ""
	}
	h := strings.TrimSuffix(strings.TrimPrefix(resp.Request.URL.Hostname(), "bedrock-runtime."), ".amazonaws.com")
	if !reAWSRegion.MatchString(h) {
		return ""
	}
	return h
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// modelFromBody reads the top-level "model" field from a JSON request body
// (used by body-model providers: Anthropic, OpenAI). Stored verbatim.
func modelFromBody(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

// streamFromRequest reads the `stream` flag from the request body (the authoritative
// signal), then the path-based stream endpoints (Gemini :streamGenerateContent,
// whose non-SSE form is a plain JSON array; Bedrock converse-stream and
// invoke-with-response-stream, so a failed stream call still records stream),
// falling back to an event-stream response content-type. The
// hyphen is normalized so one check matches both Anthropic (text/event-stream) and
// Bedrock (application/vnd.amazon.eventstream, path-based /converse-stream).
func streamFromRequest(reqBody []byte, path string, resp *http.Response) bool {
	var m struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(reqBody, &m) == nil && m.Stream {
		return true
	}
	if strings.HasSuffix(path, ":streamGenerateContent") || strings.HasSuffix(path, "/converse-stream") ||
		strings.HasSuffix(path, "/invoke-with-response-stream") {
		return true
	}
	ct := strings.ReplaceAll(resp.Header.Get("Content-Type"), "-", "")
	return strings.Contains(ct, "eventstream")
}

// providerRequestID pulls the upstream's own request id from response headers.
func providerRequestID(h http.Header) string {
	for _, k := range []string{"request-id", "x-amzn-requestid", "x-request-id"} { // Anthropic, Bedrock, OpenAI
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}

// parseRequestParts extracts messages/system/tools as raw JSON. Handles the
// Anthropic shape and the Bedrock converse `toolConfig` variant.
func parseRequestParts(body []byte) (messages, system, tools []byte) {
	var p struct {
		Messages   json.RawMessage `json:"messages"`
		System     json.RawMessage `json:"system"`
		Tools      json.RawMessage `json:"tools"`
		ToolConfig json.RawMessage `json:"toolConfig"`
	}
	if json.Unmarshal(body, &p) != nil {
		return
	}
	tools = p.Tools
	if len(tools) == 0 {
		tools = p.ToolConfig
	}
	return p.Messages, p.System, tools
}

// callerIP is the caller address for capture: the configured resolver, or
// the TCP peer when none was set.
func (rt *router) callerIP(r *http.Request) string {
	if rt.clientIP != nil {
		return rt.clientIP(r)
	}
	return peerIP(r)
}
