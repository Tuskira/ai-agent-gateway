// Package llmplane is the :8082 LLM plane: it authenticates each call with a
// gateway key, tags it with gateway-assigned tenant/session, forwards it to the
// first matching Provider byte-for-byte (so prompt caching survives; the one
// exception is OpenAI stream usage, off unless the operator opts in — see
// openaiProvider.InjectStreamUsage, and a model registry target of another
// wire format, which is translated — see translated.go),
// streams the answer back, and captures one row per call. Providers are an additive seam —
// a new provider is one file plus one line in buildProviders; a new model is zero
// code (the model name is forwarded verbatim).
package llmplane

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Usage is the normalized token accounting a provider extracts from a response.
// It maps onto the token columns of pkg/sink.LLMCall (int64), keeping the
// provider files decoupled from the sink package.
type Usage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	// CacheCreation1hTokens is the subset of CacheCreationTokens written with a
	// 1-hour TTL (Anthropic bills it at 2x base input vs 1.25x for the 5-min
	// default). Zero unless the provider reports the ephemeral_1h split.
	CacheCreation1hTokens int64
	// WebSearchRequests is the server-tool web-search count, billed as a flat
	// per-request fee on top of tokens.
	WebSearchRequests int64
	// Pricing context the provider reports: ServiceTier (OpenAI service_tier,
	// Gemini serviceTier: "priority", "flex", ...), and Anthropic's Speed ("fast")
	// and InferenceGeo ("us").
	ServiceTier       string
	Speed             string
	InferenceGeo      string
	StopReason        string
	ProviderRequestID string
}

// Provider adapts one upstream API format. Selection is by the /{ID}/ route
// prefix; no registry/DI — buildProviders returns a compiled slice, Handler
// builds an ID map from it, and that is the whole seam.
type Provider interface {
	Name() string // "anthropic" | "bedrock" | "openai" | "gemini"
	ID() string   // route-prefix segment: a request to /{ID}/<native path> selects this provider
	// Model extracts the model id for capture — from the request body (OpenAI,
	// Anthropic) or the upstream path (Bedrock, Gemini). Forwarded verbatim, so
	// new models need no code.
	Model(body []byte, upstreamPath string) string
	// BuildUpstream builds the native upstream request targeting upstreamPath
	// (the path with the /{ID}/ prefix stripped). Passthrough vs SigV4 re-sign.
	BuildUpstream(ctx context.Context, r *http.Request, body []byte, upstreamPath string) (*http.Request, error)
	ParseUsage(respBody []byte) Usage // dialect-specific token parse
}

// pickProvider resolves the request to a provider and the native upstream path.
// A /{provider-id}/… prefix selects that provider explicitly (upstream path =
// the remainder). Failing that, bare native paths are routed for SDK clients
// that append to a base URL and can't add a prefix — but each bare match is
// scoped to that provider's OWN endpoints, not a broad namespace, so providers
// that share /v1 (OpenAI's /v1/chat/completions, …) are never misrouted; those
// use their explicit prefix. Returns (nil, "") → 404.
//
// Bare matches (upstream path = the full path, not stripped):
//
//	/v1/messages…, /v1/complete → Anthropic (Claude Code sets ANTHROPIC_BASE_URL
//	                              and appends /v1/messages)
//	/model/…                    → Bedrock
func pickProvider(byID map[string]Provider, r *http.Request) (Provider, string) {
	path := r.URL.Path
	if seg, rest := firstSegment(path); seg != "" {
		if p, ok := byID[seg]; ok {
			return p, rest
		}
	}
	if p, ok := byID["anthropic"]; ok && isAnthropicNativePath(path) {
		return p, path
	}
	if p, ok := byID["bedrock"]; ok && strings.HasPrefix(path, "/model/") {
		return p, path
	}
	return nil, ""
}

// isAnthropicNativePath matches Anthropic's own bare endpoints only (the
// Messages API and the legacy Text Completions API), deliberately NOT all of
// /v1/ — so a future provider sharing the /v1 namespace collides with none of it.
func isAnthropicNativePath(path string) bool {
	return strings.HasPrefix(path, "/v1/messages") || strings.HasPrefix(path, "/v1/complete")
}

// firstSegment splits "/openai/v1/chat" into ("openai", "/v1/chat"); a single
// segment "/openai" yields ("openai", "/"); "/" yields ("", "").
func firstSegment(path string) (seg, rest string) {
	p := strings.TrimPrefix(path, "/")
	if p == "" {
		return "", ""
	}
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], "/" + p[i+1:]
	}
	return p, "/"
}

// headerAllowlist is the ONLY set of header names whose values are stored. Being
// an allowlist (not a denylist), a newly added credential header (Authorization,
// x-api-key, x-amz-*, cookie, ...) can never silently leak because it is simply
// not on this list. Everything else is recorded as present-but-masked.
var headerAllowlist = map[string]bool{
	"content-type":                  true,
	"content-length":                true,
	"content-encoding":              true,
	"accept":                        true,
	"accept-encoding":               true,
	"anthropic-version":             true,
	"anthropic-beta":                true,
	"user-agent":                    true,
	"x-amzn-requestid":              true,
	"x-request-id":                  true,
	"request-id":                    true,
	"date":                          true,
	"connection":                    true,
	"x-bedrock-region":              true,
	"retry-after":                   true,
	"server":                        true,
	"via":                           true,
	"vary":                          true,
	"alt-svc":                       true,
	"cf-ray":                        true,
	"cf-cache-status":               true,
	"strict-transport-security":     true,
	"x-content-type-options":        true,
	"access-control-expose-headers": true,
	"openai-version":                true,
	"openai-processing-ms":          true,
	"openai-organization":           true,
	"openai-project":                true,
	"x-openai-proxy-wasm":           true,
}

// headerAllowPrefixes extends headerAllowlist to header families whose members
// vary per provider (x-ratelimit-limit-requests, anthropic-ratelimit-tokens-…).
var headerAllowPrefixes = []string{"x-ratelimit-", "anthropic-ratelimit-"}

func headerAllowed(lname string) bool {
	if headerAllowlist[lname] {
		return true
	}
	for _, p := range headerAllowPrefixes {
		if strings.HasPrefix(lname, p) {
			return true
		}
	}
	return false
}

// hopByHop headers and gateway-internal headers are never forwarded upstream.
var skipForward = map[string]bool{
	"connection":               true,
	"keep-alive":               true,
	"proxy-authenticate":       true,
	"proxy-authorization":      true,
	"te":                       true,
	"trailer":                  true,
	"transfer-encoding":        true,
	"upgrade":                  true,
	"host":                     true,
	"content-length":           true,
	"accept-encoding":          true, // let Go negotiate; avoids capturing gzipped bodies
	"x-gateway-key":            true,
	"x-tenant-id":              true,
	"x-session-id":             true,
	"x-claude-code-session-id": true,
	"x-bedrock-region":         true,
	// Flow-B credential headers: consumed by the gateway to sign, never forwarded.
	"x-bedrock-access-key-id":     true,
	"x-bedrock-secret-access-key": true,
	"x-bedrock-session-token":     true,
	"x-bedrock-role-arn":          true,
	"x-bedrock-external-id":       true,
}

// providerKeyHeader is where a caller puts its own vendor key for a
// translated registry target (X-Provider-Key, or X-Provider-Key-<label>:
// -groq, -zhipu, ...); read by translate.CallerKey, never forwarded.
const providerKeyHeader = "X-Provider-Key"

// copyHeaders copies headers from src to dst, dropping hop-by-hop and
// gateway-internal ones (skipForward, providerKeyHeader). Used both ways: client→upstream (the
// client's own provider credential — x-api-key / BYOK Authorization — is
// preserved, so it rides along) and upstream→client. A gateway key presented
// via Authorization (Bearer gk_…) is OUR credential, not the provider's, so it
// is never forwarded upstream (it would otherwise leak the gateway key); a BYOK
// Authorization (sk-…, OAuth, AWS4-…) is kept.
func copyHeaders(dst, src http.Header) {
	for name, vals := range src {
		lname := strings.ToLower(name)
		if skipForward[lname] || isProviderKeyHeader(lname) {
			continue
		}
		if lname == "authorization" && isGatewayBearer(vals) {
			continue
		}
		for _, v := range vals {
			dst.Add(name, v)
		}
	}
}

// isProviderKeyHeader reports whether a lowercased header name is
// providerKeyHeader or one of its per-label forms.
func isProviderKeyHeader(lname string) bool {
	base := strings.ToLower(providerKeyHeader)
	return lname == base || strings.HasPrefix(lname, base+"-")
}

// isGatewayBearer reports whether an Authorization value carries the gateway's
// own key (Bearer gk_…) rather than a provider credential.
func isGatewayBearer(vals []string) bool {
	for _, v := range vals {
		if strings.HasPrefix(v, "Bearer gk_") {
			return true
		}
	}
	return false
}

// maskInto adds h's headers to out, keeping values only for allowlisted names and
// replacing every other value with "[masked]" (name kept for triage). Request and
// response headers merge into the single pkg/sink.LLMCall.Headers map.
func maskInto(out map[string]string, h http.Header) {
	for name, vals := range h {
		if headerAllowed(strings.ToLower(name)) {
			out[name] = strings.Join(vals, ", ")
		} else {
			out[name] = "[masked]"
		}
	}
}

// empty reports whether no billable usage was parsed.
func (u Usage) empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadTokens == 0 &&
		u.CacheCreationTokens == 0 && u.WebSearchRequests == 0
}

// headerUsager is implemented by providers that also report token counts in
// response headers (Bedrock InvokeModel, for every model). The router uses it
// only when the body yielded no usage, so a body-parsed count always wins.
type headerUsager interface {
	HeaderUsage(h http.Header) Usage
}

// streamUsageInjector is implemented by providers that rewrite a streaming
// request so the upstream reports usage it otherwise omits (OpenAI Chat
// Completions). When it reports true, the router strips the extra usage-only
// chunk from the client's copy of the stream.
type streamUsageInjector interface {
	InjectStreamUsage(body []byte, upstreamPath string) ([]byte, bool)
}

// requestHints reads the pricing context a request declares (OpenAI
// service_tier, Gemini serviceTier, Anthropic speed and inference_geo); the
// router uses it only for fields the response left empty.
func requestHints(body []byte) (tier, speed, geo string) {
	var r struct {
		ServiceTier  string `json:"service_tier"`
		Tier         string `json:"serviceTier"`
		Speed        string `json:"speed"`
		InferenceGeo string `json:"inference_geo"`
	}
	_ = json.Unmarshal(body, &r)
	return firstNonEmpty(r.Tier, r.ServiceTier), r.Speed, r.InferenceGeo
}
