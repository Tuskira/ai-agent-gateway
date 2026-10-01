package llmplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/llmplane/translate"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	pkgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Translated targets: how a model registry target whose wire format differs
// from the client's is served through the translation engine
// (internal/llmplane/translate) instead of being refused. The registry is
// the only way in: resolve() attaches a *translation to such a target, and
// the router builds, tries, falls back and captures it exactly like a
// same-wire one.

// translation is how one target is reached through the engine.
type translation struct {
	d llm.Dialect
	p llm.Provider
}

// translatedOp is what a client path means to the translation engine.
type translatedOp int

const (
	opNone  translatedOp = iota // not translatable
	opChat                      // the dialect's chat call
	opCount                     // the dialect's token count, answered by estimate
)

// opFor maps a client dialect's path onto the engine's operations. Only
// the Anthropic Messages dialect has an adapter today.
func opFor(dialect, upstreamPath string) translatedOp {
	if dialect != "anthropic" {
		return opNone
	}
	switch strings.TrimSuffix(upstreamPath, "/") {
	case "/v1/messages":
		return opChat
	case "/v1/messages/count_tokens":
		return opCount
	}
	return opNone
}

// vendorOf is what capture records as a target's resolved_vendor: its
// label when it has one ("groq"), else its vendor type ("openai_compat").
func vendorOf(t store.ModelTarget) string {
	if t.Label != "" {
		return t.Label
	}
	return t.Vendor
}

// pricingProviderOf is the pkg/pricing provider a target's calls are costed
// as: its label when it has one, else "openai" for an openai_compat target
// and the vendor type for the others. pkg/pricing reads the provider for
// its token convention (foldsCache) and for Bedrock's regional rows; the
// rate itself is looked up by model id.
func pricingProviderOf(t store.ModelTarget) string {
	switch {
	case t.Label != "":
		return t.Label
	case t.Vendor == "openai_compat":
		return "openai"
	}
	return t.Vendor
}

// foldsCache reports pkg/pricing's token convention for a provider: for
// "openai" and "gemini" the input count INCLUDES the cached tokens (and
// pricing subtracts them back); for every other provider input and
// cache_read are separate counts, Anthropic's split. Capture stores, and
// pricing is given, the convention of the provider the call is costed as.
func foldsCache(pricingProvider string) bool {
	return pricingProvider == "openai" || pricingProvider == "gemini"
}

// translatorFor returns the translation serving a call in the client's
// dialect on target t, or nil when no adapter pair exists for it (the
// remaining "requires translation" 400): the client's dialect and the
// target's vendor must both be registered in pkg/llm, and the path must be
// one the engine serves.
func translatorFor(dialect, upstreamPath string, t store.ModelTarget) *translation {
	if opFor(dialect, upstreamPath) == opNone {
		return nil
	}
	d, err := llm.DialectByName(dialect)
	if err != nil {
		return nil
	}
	p, err := llm.ProviderByName(t.Vendor)
	if err != nil {
		return nil
	}
	return &translation{d: d, p: p}
}

// translatedKey picks the vendor key for a translated target: its own
// credential (creds; managed, i.e. the operator's: a vendor 401/403 on it
// is answered without the vendor's message), else -- only when the target
// sets allow_caller_key -- the caller's (translate.CallerKey: its
// X-Provider-Key[-<label>] header, else its dialect's own credential
// unless that is the gateway's or an Anthropic one). ok is false when no
// credential may reach the target at all; key "" with ok means the caller
// sent none, and the call goes out without one (a keyless local server).
func translatedKey(r *http.Request, t store.ModelTarget, creds map[string]string) (key string, managed, ok bool, err error) {
	if creds != nil {
		k, err := apiKeyOf(creds)
		if err != nil {
			return "", false, false, clientError{"model registry: " + err.Error()}
		}
		return k, true, true, nil
	}
	if !t.AllowCallerKey {
		return "", false, false, nil
	}
	return translate.CallerKey(r, t.Label), false, true, nil
}

// buildTranslated builds the attempt for a translated target. The chat
// call is parsed, checked against the vendor's capabilities and built by
// the engine with a fresh header set: nothing the client sent
// (anthropic-beta, cookies, session ids, its x-api-key, its X-Provider-Key
// headers) and no query string reaches the vendor. The token count is not
// sent anywhere: the attempt carries the adapter's estimate.
func (rt *router) buildTranslated(ctx context.Context, p Provider, r *http.Request, body []byte, upstreamPath string, t resolvedTarget, creds map[string]string) (*upstreamAttempt, error) {
	tr := t.tr
	pricingProvider := pricingProviderOf(t.ModelTarget)
	att := &upstreamAttempt{pricingProvider: pricingProvider, translated: true, parseUsage: translatedUsage(pricingProvider)}
	if opFor(p.Name(), upstreamPath) == opCount {
		n, err := translate.EstimateTokens(withBody(r, body), tr.d, tr.p)
		if err != nil {
			return nil, clientError{fmt.Sprintf("cannot translate request: %v", err)}
		}
		att.estimate = &n
		return att, nil
	}
	key, managed, ok, err := translatedKey(r, t.ModelTarget, creds)
	if err != nil {
		return nil, err
	}
	if !ok {
		// resolve() does not keep such a target; defence in depth.
		return nil, clientError{"no credential may reach this target (set a credential or allow_caller_key)"}
	}
	target := llm.Target{Vendor: pricingProvider, BaseURL: t.BaseURL, Model: t.Model, Auth: llm.Auth{APIKey: key}}
	call, err := translate.Prepare(ctx, withBody(r, body), tr.d, tr.p, target, translate.Options{RequestID: requestIDOf(ctx), ManagedKey: managed})
	if err != nil {
		var se *translate.StatusError
		if errors.As(err, &se) && se.Status == http.StatusBadRequest {
			return nil, clientError{se.Message}
		}
		return nil, err
	}
	att.req, att.adapt = call.Upstream, translateResponse
	return att, nil
}

// withBody is r with its (already read) body restored.
func withBody(r *http.Request, body []byte) *http.Request {
	in := r.Clone(r.Context())
	in.Body, in.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	return in
}

// translatedUsage parses the relayed body of a translated call, which is
// already in the client's (Anthropic) shape -- input excludes the cached
// tokens -- into the convention of the provider it is costed as.
func translatedUsage(pricingProvider string) func([]byte) Usage {
	return func(body []byte) Usage {
		u := anthropicProvider{}.ParseUsage(body)
		if foldsCache(pricingProvider) {
			u.InputTokens += u.CacheReadTokens
		}
		return u
	}
}

// openAIUsageAs parses an OpenAI-shaped body -- input includes the cached
// tokens -- into the convention of the provider it is costed as: what a
// labelled openai_compat target reached by an OpenAI-dialect client needs,
// so a label's rows carry one convention whichever dialect called it.
func openAIUsageAs(pricingProvider string, parse func([]byte) Usage) func([]byte) Usage {
	if foldsCache(pricingProvider) {
		return nil // already the provider's convention
	}
	return func(body []byte) Usage {
		u := parse(body)
		u.InputTokens = max(0, u.InputTokens-u.CacheReadTokens)
		return u
	}
}

// translateResponse replaces resp with its translation into the client's
// format (see translate.Call.Response). A response whose request did not
// come from buildTranslated is never relayed as the vendor sent it.
func translateResponse(resp *http.Response) {
	if c := translate.CallOf(resp.Request); c != nil {
		c.Response(resp)
		return
	}
	_ = resp.Body.Close()
	resp.StatusCode, resp.Header, resp.ContentLength = http.StatusBadGateway, http.Header{"Content-Type": {"application/json"}}, -1
	resp.Body = io.NopCloser(bytes.NewReader(anthropicErrorBody(http.StatusBadGateway, "", "untranslated upstream response")))
}

// serveEstimate answers a token count for a translated target locally:
// another vendor's tokenizer is not reachable, and the client's own vendor
// does not know the model. The answer is the adapter's estimate, marked
// "estimated", and it is captured (status 200, stop_reason "estimated",
// unpriced).
func (rt *router) serveEstimate(w http.ResponseWriter, r *http.Request, info callInfo, n int64, start time.Time) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(translate.EstimateBody(n))
	ctx := r.Context()
	headers := make(map[string]string, len(r.Header))
	maskInto(headers, r.Header)
	clientName, userAgent := clientInfo(r)
	rt.record(&pkgsink.LLMCall{
		Timestamp:      start,
		RequestID:      requestIDOf(ctx),
		TenantID:       tenantOf(ctx),
		Principal:      principalOf(ctx),
		KeyID:          keyIDOf(ctx),
		SessionID:      sessionOf(ctx),
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
		StatusCode:     http.StatusOK,
		DurationMS:     time.Since(start).Milliseconds(),
		InputTokens:    n,
		StopReason:     "estimated",
		Headers:        headers,
	})
}
