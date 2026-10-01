package llmplane

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
)

// PermissionLLMAccess is the permission a caller must hold to use the LLM plane
// (checked in chain against Config.Authorizer). The built-in admin ("*") and
// agent ("llm.*") roles hold it; a custom role must grant it.
const PermissionLLMAccess = "llm.access"

// Config is the resolved LLM-plane config: the caller maps config.LLMProxy onto
// it, so the plane never reads the gateway's config types (the one thing it
// borrows from internal/config is the shared AWS-region regex, see
// bedrock.go's reAWSRegion). Authentication is NOT the plane's concern — the
// caller wraps Handler with the gateway's auth middleware and supplies the
// caller identity via the Principal on the request context.
type Config struct {
	UpstreamBaseURL        string
	MaxRequestBytes        int64 // 413 threshold on the inbound request
	MaxStreamDuration      time.Duration
	MaxConcurrentPerTenant int
	BedrockEnabled         bool
	BedrockRegion          string
	BedrockCredentialMode  string
	// BedrockRoleAccounts is the comma-separated allowlist of accounts whose
	// roles X-Bedrock-Role-Arn may name: a bare account id for the "aws"
	// partition, "<partition>:<account>" (aws-us-gov:…, aws-cn:…) outside it,
	// or "*" for every account in every partition.
	BedrockRoleAccounts     string
	OpenAIEnabled           bool
	OpenAIBaseURL           string
	OpenAIStreamUsage       bool // opt-in: inject stream_options.include_usage (see openaiProvider.InjectStreamUsage)
	GeminiEnabled           bool
	GeminiBaseURL           string
	StoreBodies             bool
	MaxCaptureRequestBytes  int // bounded storage cap for the request body
	MaxCaptureResponseBytes int // bounded storage cap for the response body (the stream tee)

	// Authorizer decides PermissionLLMAccess for the Principal the caller's auth
	// middleware put on the context. It is REQUIRED: Handler returns an error
	// when it is nil, because a plane with no authorizer would serve every
	// authenticated key regardless of role.
	Authorizer pkgauth.Authorizer

	// ClientIP derives the caller's address for capture (the process-wide
	// clientip.Resolver). Nil falls back to the TCP peer address;
	// X-Forwarded-For is never trusted on its own.
	ClientIP func(*http.Request) string

	// Pricing is the rate card used to cost each captured call
	// (pricing.Card.Cost). Nil means the package-level embedded default
	// (pricing.Default) — the caller (cmd/gateway) supplies a card merged
	// with an operator override file when llm_proxy.pricing_file is set.
	Pricing *pricing.Card

	// Registry resolves each request's "model" against the tenant's model
	// registry (see NewRegistry and docs/llm-plane.md "Model registry").
	// Nil resolves nothing: every call is the byte-for-byte passthrough.
	Registry *Registry

	// Limiter enforces per-API-key limits (budgets, rpm, max_tokens; see
	// limits.go). Nil = no per-key limits are enforced. The caller builds
	// it (NewLimiter) so it can also read Limiter.Status for /health.
	Limiter *Limiter
}

// Handler builds the provider slice, wires the router and middleware chain, and
// returns the LLM-plane handler. rec durably captures one record per call. The
// caller mounts this behind its auth middleware (and adds an unauthenticated
// /health ahead of it), so no auth or /health lives here.
//
// cfg.Authorizer is the one required dependency (same contract as
// dataplane.New's): without it the plane could not enforce PermissionLLMAccess,
// so it is refused at construction rather than discovered at request time.
func Handler(cfg Config, rec Recorder) (http.Handler, error) {
	if cfg.Authorizer == nil {
		return nil, fmt.Errorf("llmplane: Authorizer is required")
	}
	// The Bedrock provider is built unconditionally: routed for /model/*
	// only when enabled, but always available to sign a registry target
	// of vendor "bedrock" reached from another dialect.
	bedrock := newBedrockProvider(cfg.BedrockRegion, cfg.BedrockCredentialMode, cfg.BedrockRoleAccounts)
	providers := buildProviders(cfg, bedrock)
	byID := make(map[string]Provider, len(providers))
	for _, p := range providers {
		byID[p.ID()] = p
	}
	pc := cfg.Pricing
	if pc == nil {
		pc = pricing.Default
	}
	clientIP := cfg.ClientIP
	if clientIP == nil {
		clientIP = peerIP
	}
	rt := &router{
		clientIP: clientIP,
		byID:     byID,
		recorder: rec,
		// Default transport (TLS verify on), no client timeout (SSE). Redirects are
		// relayed to the client, never followed: following one would re-send the
		// body and the caller's provider key (x-api-key, x-goog-api-key, ...) to
		// whatever host the Location names.
		client: &http.Client{Transport: upstreamTransport(), CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		storeBodies:    cfg.StoreBodies,
		maxCaptureReq:  cfg.MaxCaptureRequestBytes,
		maxCaptureResp: cfg.MaxCaptureResponseBytes,
		pricing:        pc,
		registry:       cfg.Registry,
		bedrock:        bedrock,
		limiter:        cfg.Limiter,
	}
	return chain(rt, cfg), nil
}

// buildProviders is the whole provider seam: a compiled slice (+ an ID map built
// by Handler for /{id}/ prefix routing). A new provider is one constructor + one
// line here. Anthropic + Bedrock keep their legacy bare routes; OpenAI/Gemini are
// prefix-only and mounted when enabled.
func buildProviders(cfg Config, bedrock *bedrockProvider) []Provider {
	providers := []Provider{newAnthropicProvider(cfg.UpstreamBaseURL)}
	if cfg.BedrockEnabled {
		if bedrock == nil {
			bedrock = newBedrockProvider(cfg.BedrockRegion, cfg.BedrockCredentialMode, cfg.BedrockRoleAccounts)
		}
		providers = append([]Provider{bedrock}, providers...)
	}
	if cfg.OpenAIEnabled {
		providers = append(providers, newOpenAIProvider(cfg.OpenAIBaseURL, cfg.OpenAIStreamUsage))
	}
	if cfg.GeminiEnabled {
		providers = append(providers, newGeminiProvider(cfg.GeminiBaseURL))
	}
	return providers
}

// chain wraps the handler outermost-first: tags → permission → body limit →
// per-tenant concurrency → stream deadline. Authentication is applied by the
// caller AHEAD of this plane; the Principal it put on the context feeds the
// permission check and the limiter's tenant.
func chain(h http.Handler, cfg Config) http.Handler {
	tl := newTenantLimiter(cfg.MaxConcurrentPerTenant)
	h = streamDeadline(cfg.MaxStreamDuration, h)
	h = tl.middleware(h)
	h = limitBody(cfg.MaxRequestBytes, h)
	h = requirePermission(cfg.Authorizer, h)
	h = withTags(h)
	return h
}

// transportOverride replaces the guarded upstream transport. Tests only
// (see export_test.go); always nil in the shipped binary.
var transportOverride http.RoundTripper

// upstreamTransport is the transport for every upstream call: it dials
// through the SSRF guard (internal/netguard).
func upstreamTransport() http.RoundTripper {
	if transportOverride != nil {
		return transportOverride
	}
	return netguard.Transport()
}
