package llmplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Registry resolution: how a hit in the model registry turns into one or
// more upstream attempts.
//
// Same wire format: a client speaking the Anthropic dialect can be sent to
// an Anthropic endpoint (any host) or to Bedrock for an Anthropic model
// (whose invoke body and response ARE the Anthropic Messages shapes, only
// the framing of a stream differs -- see eventstream.go); an OpenAI-dialect
// client to any OpenAI-compatible endpoint; Bedrock to Bedrock and Gemini
// to Gemini. In every such case the gateway rewrites ONLY the upstream
// host, the credential, the region and the model id; everything else in
// the request is the client's own bytes.
//
// Different wire format (requiresTranslation): the target is served
// through the translation engine when pkg/llm has an adapter for both
// sides (translatorFor, translated.go) -- today an Anthropic-dialect
// client on an openai_compat target. A pair with no adapter is refused
// with the "requires translation" 400 below.

// resolvedRoute is a registry hit: the row and the targets the client's
// dialect can be sent to, in the row's order.
type resolvedRoute struct {
	model    *store.Model
	attempts []resolvedTarget
	// noCredential lists the indexes of reachable targets that were
	// skipped because no credential may reach them (see callerKeyAllowed).
	noCredential []int
}

// resolvedTarget is one target of a route, with its index in the row's
// Targets (what fallback_index records).
type resolvedTarget struct {
	store.ModelTarget
	index int
	// tr is set when the target's wire format differs from the client's:
	// the attempt is built by the translation engine (buildTranslated).
	tr *translation
}

// upstreamAttempt is a built upstream request plus what the router needs
// to relay and account for its response.
type upstreamAttempt struct {
	req *http.Request
	// adapt, when set, is applied to the selected response before it is
	// relayed (Bedrock event-stream -> SSE re-framing).
	adapt func(*http.Response)
	// pricingProvider is the pkg/pricing provider key the response is
	// costed under ("anthropic", "bedrock", "openai", "gemini").
	pricingProvider string
	// region is the Bedrock region signed for, "" otherwise.
	region string
	// translated reports an attempt built by the translation engine.
	translated bool
	// parseUsage, when set, replaces the client dialect's usage parser
	// for this attempt's relayed body.
	parseUsage func([]byte) Usage
	// estimate, when set, is the answer itself (a translated target's
	// token count): nothing is sent upstream and req is nil.
	estimate *int64
}

// resolve looks the requested model up for the caller's tenant. It returns
// (nil, nil) on a miss (or with no registry, or for a disabled row, which
// behaves as unregistered), a route with zero attempts when the row exists
// but no target is usable (none can be reached from the client's wire
// format, or none may be given a credential -- route.noCredential), and an
// error only when the store failed.
func (rt *router) resolve(ctx context.Context, p Provider, model, upstreamPath, tenantID string) (*resolvedRoute, error) {
	m, err := rt.registry.Resolve(ctx, tenantID, model)
	if err != nil || m == nil || !m.Enabled {
		return nil, err
	}
	route := &resolvedRoute{model: m}
	for i, t := range m.Targets {
		var tr *translation
		if requiresTranslation(p.Name(), upstreamPath, t) {
			if tr = translatorFor(p.Name(), upstreamPath, t); tr == nil {
				continue // no adapter for the pair
			}
		}
		if t.Credential == "" && !callerKeyAllowed(p, t, tr != nil) {
			// No credential of its own, and the caller's key may not be
			// sent there: the target is unusable, whatever the caller sent.
			route.noCredential = append(route.noCredential, i)
			slog.Warn("llmplane: model registry target skipped: no credential may reach it (set a credential or allow_caller_key)",
				"model", m.Name, "target", i, "vendor", t.Vendor)
			continue
		}
		route.attempts = append(route.attempts, resolvedTarget{ModelTarget: t, index: i, tr: tr})
	}
	return route, nil
}

// defaultBaseOf is the base URL this gateway is configured to reach the
// client's dialect at (llm_proxy.upstream_base_url for anthropic,
// llm_proxy.providers.openai.base_url, llm_proxy.providers.gemini.base_url):
// where the passthrough sends the caller's key today, and where a target
// without base_url goes. "" for a dialect with no base URL (bedrock, whose
// host is computed from the region).
func defaultBaseOf(p Provider) string {
	switch v := p.(type) {
	case anthropicProvider:
		return v.baseURL
	case openaiProvider:
		return v.baseURL
	case geminiProvider:
		return v.baseURL
	}
	return ""
}

// targetBaseOf is the base URL a target is reached at: its own base_url,
// else the gateway's configured default for the dialect.
func targetBaseOf(p Provider, t store.ModelTarget) string {
	if t.BaseURL != "" {
		return t.BaseURL
	}
	return defaultBaseOf(p)
}

// callerKeyAllowed reports whether the caller's own vendor key (BYOK) may
// be sent to target t, which names no credential. It may when the operator
// opted the target in (allow_caller_key), or when the target is on the
// very host the gateway already sends that vendor's keys to on the
// passthrough path. Bedrock always qualifies: its host is computed
// (bedrock-runtime.<region>.amazonaws.com), and the caller's X-Bedrock-*
// credentials are consumed by the gateway to sign, never forwarded.
//
// A translated target has no such default host -- the client's dialect is
// not the target's vendor, so the gateway sends that dialect's keys
// nowhere near it -- and takes a caller's key by opt-in only.
//
// This is what stops a registered name from harvesting keys: without it,
// anyone allowed to register a model could point a name callers already
// use at a host of their choosing and collect every caller's vendor key.
func callerKeyAllowed(p Provider, t store.ModelTarget, translated bool) bool {
	if t.AllowCallerKey {
		return true
	}
	if translated {
		return false
	}
	return t.Vendor == "bedrock" || sameHost(targetBaseOf(p, t), defaultBaseOf(p))
}

// sameHost compares the host[:port] of two base URLs, case-insensitively.
// An unparsable or empty URL never matches.
func sameHost(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil || ua.Host == "" || ub.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Host, ub.Host)
}

// refuseNoCredential answers a registered model whose reachable targets
// were all skipped because no credential may reach them, in the client
// dialect's own error envelope. It names the first such target.
func (rt *router) refuseNoCredential(w http.ResponseWriter, r *http.Request, info callInfo, body []byte, start time.Time, target int) {
	info.fallbackIndex = -1 // no target could be tried
	rt.emitError(r, info, body, start, http.StatusBadRequest, "no_credential")
	writeDialectError(w, info.provider, http.StatusBadRequest, requestIDOf(r.Context()),
		fmt.Sprintf("model %s: no credential for target %d (set a credential or allow_caller_key)", info.requestedModel, target))
}

// requiresTranslation reports whether sending a request in the client's
// dialect to target t would need its body translated between wire
// formats. When it does, translatorFor decides whether an adapter pair
// exists to do so.
func requiresTranslation(dialect, upstreamPath string, t store.ModelTarget) bool {
	switch dialect {
	case "anthropic":
		switch t.Vendor {
		case "anthropic":
			return false
		case "bedrock":
			// Bedrock's InvokeModel for an Anthropic model takes the
			// Messages body and returns the Messages response; only
			// /v1/messages itself maps (count_tokens, batches and the
			// legacy /v1/complete have different Bedrock shapes).
			return !isAnthropicBedrockModel(t.Model) || strings.TrimSuffix(upstreamPath, "/") != "/v1/messages"
		}
	case "openai":
		return t.Vendor != "openai_compat"
	case "bedrock":
		return t.Vendor != "bedrock"
	case "gemini":
		return t.Vendor != "gemini"
	}
	return true
}

// reAnthropicBedrockModel matches a Bedrock model id of an Anthropic model:
// the bare "anthropic.<model>" or an inference profile
// "<region-prefix>.anthropic.<model>" (us., eu., apac., global., ...).
var reAnthropicBedrockModel = regexp.MustCompile(`^(?:[a-z0-9-]+\.)?anthropic\.[A-Za-z0-9._:-]+$`)

func isAnthropicBedrockModel(id string) bool { return reAnthropicBedrockModel.MatchString(id) }

// refuseUntranslatable answers a registered model none of whose targets
// can be reached from the client's dialect, natively or translated:
//
//	400 {"type":"error","error":{"type":"invalid_request_error",
//	     "message":"model <name> requires translation (not available)"}}
//
// It only answers for pairs no pkg/llm adapter covers (an OpenAI-dialect
// client on an Anthropic target: there is no OpenAI dialect yet) and for
// paths the engine does not serve (batches, /v1/complete).
func (rt *router) refuseUntranslatable(w http.ResponseWriter, r *http.Request, info callInfo, body []byte, start time.Time) {
	info.fallbackIndex = -1 // no target could be tried
	rt.emitError(r, info, body, start, http.StatusBadRequest, "requires_translation")
	writeAnthropicError(w, http.StatusBadRequest, requestIDOf(r.Context()),
		"model "+info.requestedModel+" requires translation (not available)")
}

// buildTarget builds the upstream request for one resolved target, in the
// client's dialect. creds is the decrypted credential payload the target
// names, nil when it names none.
func (rt *router) buildTarget(ctx context.Context, p Provider, r *http.Request, body []byte, upstreamPath string, t resolvedTarget, creds map[string]string) (*upstreamAttempt, error) {
	switch {
	case t.tr != nil:
		return rt.buildTranslated(ctx, p, r, body, upstreamPath, t, creds)
	case p.Name() == "anthropic" && t.Vendor == "anthropic":
		return buildAnthropicTarget(ctx, p, r, body, upstreamPath, t, creds)
	case p.Name() == "anthropic" && t.Vendor == "bedrock":
		return rt.bedrock.buildAnthropicOnBedrock(ctx, r, body, t, creds)
	case p.Name() == "openai" && t.Vendor == "openai_compat":
		return buildOpenAICompatTarget(ctx, p, r, body, upstreamPath, t, creds)
	case p.Name() == "bedrock" && t.Vendor == "bedrock":
		return rt.bedrock.buildBedrockTarget(ctx, r, body, upstreamPath, t, creds)
	case p.Name() == "gemini" && t.Vendor == "gemini":
		return buildGeminiTarget(ctx, p, r, body, upstreamPath, t, creds)
	}
	return nil, fmt.Errorf("model registry: no builder for %s -> %s", p.Name(), t.Vendor)
}

// newUpstream is the shared skeleton: URL = base + path (+ the client's
// query), the client's headers copied minus hop-by-hop/gateway ones.
func newUpstream(ctx context.Context, r *http.Request, body []byte, base, upstreamPath string) (*http.Request, error) {
	u := strings.TrimRight(base, "/") + upstreamPath
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	up, err := http.NewRequestWithContext(ctx, r.Method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(up.Header, r.Header)
	return up, nil
}

// credentialHeaders are the request headers vendors read an API key from.
// On the registry path every one of them is removed from the upstream
// request unless it is the target dialect's own (dialectCredentialHeaders),
// so a key meant for one vendor is never delivered to another.
var credentialHeaders = []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Api-Key"}

// dialectCredentialHeaders are the headers a dialect's own key travels in.
var dialectCredentialHeaders = map[string][]string{
	"anthropic": {"X-Api-Key", "Authorization"}, // API key, or an OAuth bearer
	"openai":    {"Authorization"},
	"gemini":    {"X-Goog-Api-Key", "Authorization"}, // API key, or an OAuth bearer
}

// setCredential decides which credential reaches a target of the given
// dialect. In order:
//
//  1. The target's own credential, when it names one. The caller's
//     credential headers are removed, so exactly one credential is sent.
//  2. The caller's own key for this dialect (BYOK), only when
//     callerKeyOK -- the target opted in with allow_caller_key, or it is
//     on the gateway's configured default host for the vendor (see
//     callerKeyAllowed).
//  3. Nothing. resolve() does not even build such a target, so this
//     branch is defence in depth: every credential header is removed.
//
// In every branch the gateway's own key and any other vendor's key header
// are removed. headerName/format say where and how the target's credential
// is written.
func setCredential(up *http.Request, dialect string, creds map[string]string, callerKeyOK bool, headerName string, format func(string) string) error {
	own := map[string]bool{}
	for _, h := range dialectCredentialHeaders[dialect] {
		own[http.CanonicalHeaderKey(h)] = true
	}
	for _, h := range credentialHeaders {
		h = http.CanonicalHeaderKey(h)
		if !own[h] || creds != nil || !callerKeyOK || carriesGatewayKey(up.Header.Values(h)) {
			up.Header.Del(h)
		}
	}
	if creds == nil {
		return nil
	}
	key, err := apiKeyOf(creds)
	if err != nil {
		return clientError{"model registry: " + err.Error()}
	}
	up.Header.Set(headerName, format(key))
	return nil
}

// carriesGatewayKey reports whether a credential header value is the
// gateway's own key (gk_..., bare or as a bearer). copyHeaders already
// drops the two headers the gateway reads its key from; this covers a
// client that also repeated it somewhere else.
func carriesGatewayKey(vals []string) bool {
	for _, v := range vals {
		if strings.HasPrefix(strings.TrimPrefix(v, "Bearer "), "gk_") {
			return true
		}
	}
	return false
}

func buildAnthropicTarget(ctx context.Context, p Provider, r *http.Request, body []byte, upstreamPath string, t resolvedTarget, creds map[string]string) (*upstreamAttempt, error) {
	base := targetBaseOf(p, t.ModelTarget)
	body, err := rewriteTopLevel(body, map[string]any{"model": t.Model}, nil)
	if err != nil {
		return nil, clientError{"request body is not a JSON object"}
	}
	up, err := newUpstream(ctx, r, body, base, upstreamPath)
	if err != nil {
		return nil, err
	}
	if err := setCredential(up, "anthropic", creds, callerKeyAllowed(p, t.ModelTarget, false), "X-Api-Key", func(k string) string { return k }); err != nil {
		return nil, err
	}
	return &upstreamAttempt{req: up, pricingProvider: "anthropic"}, nil
}

func buildOpenAICompatTarget(ctx context.Context, p Provider, r *http.Request, body []byte, upstreamPath string, t resolvedTarget, creds map[string]string) (*upstreamAttempt, error) {
	body, err := rewriteTopLevel(body, map[string]any{"model": t.Model}, nil)
	if err != nil {
		return nil, clientError{"request body is not a JSON object"}
	}
	// A registry openai_compat target's base_url INCLUDES the version
	// segment (like the OpenAI SDKs: https://api.openai.com/v1 -- see
	// docs/llm-plane.md's base_url convention). This is the same-dialect
	// passthrough (client and target both speak OpenAI), so upstreamPath
	// is already "/v1/..." (the /openai/ prefix stripped by pickProvider);
	// joining it onto t.BaseURL verbatim would double the version segment
	// ({base}/v1/v1/chat/completions). Strip it here so the result is
	// {base}/chat/completions, matching the translated path's join
	// (buildTranslated, which never had this bug -- it always appends
	// "/chat/completions" directly).
	up, err := newUpstream(ctx, r, body, t.BaseURL, strings.TrimPrefix(upstreamPath, "/v1"))
	if err != nil {
		return nil, err
	}
	if err := setCredential(up, "openai", creds, callerKeyAllowed(p, t.ModelTarget, false), "Authorization", func(k string) string { return "Bearer " + k }); err != nil {
		return nil, err
	}
	pricingProvider := pricingProviderOf(t.ModelTarget)
	return &upstreamAttempt{req: up, pricingProvider: pricingProvider, parseUsage: openAIUsageAs(pricingProvider, p.ParseUsage)}, nil
}

func buildGeminiTarget(ctx context.Context, p Provider, r *http.Request, body []byte, upstreamPath string, t resolvedTarget, creds map[string]string) (*upstreamAttempt, error) {
	base := targetBaseOf(p, t.ModelTarget)
	callerKeyOK := callerKeyAllowed(p, t.ModelTarget, false)
	// The model is in the path: /v1beta/models/{model}:generateContent.
	path := reGeminiModel.ReplaceAllString(upstreamPath, "/models/"+t.Model)
	up, err := newUpstream(ctx, r, body, base, path)
	if err != nil {
		return nil, err
	}
	if creds != nil || !callerKeyOK {
		// Gemini also accepts the key as ?key=; the caller's must not
		// travel beside the target's own credential, nor to a target it
		// may not reach.
		q := up.URL.Query()
		q.Del("key")
		up.URL.RawQuery = q.Encode()
	}
	if err := setCredential(up, "gemini", creds, callerKeyOK, "X-Goog-Api-Key", func(k string) string { return k }); err != nil {
		return nil, err
	}
	return &upstreamAttempt{req: up, pricingProvider: "gemini"}, nil
}

// rewriteTopLevel returns body with the top-level members in set replaced
// or added and those in del removed. Only the top level is touched: every
// other member's bytes are carried over verbatim (json.RawMessage), so
// nested content -- messages, cache_control markers, tool schemas -- is
// byte-identical to what the client sent. Members are emitted in sorted
// key order, which is the one visible difference from the client's bytes.
func rewriteTopLevel(body []byte, set map[string]any, del []string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	for _, k := range del {
		delete(obj, k)
	}
	for k, v := range set {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		obj[k] = raw
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(obj[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
