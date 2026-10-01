// Package handlers implements the gateway control-plane's REST handlers:
// tenants, API keys, credentials, connectors, agent profiles, header
// providers, and the embedded OpenAPI/docs page. Every handler here reads
// its tenant exclusively from pkgauth.PrincipalFrom(ctx) -- never from the
// request -- and replies through internal/api/httpx's shared JSON/error
// helpers.
package handlers

import (
	"net/http"
	"time"

	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	dataplaneheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/ops"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Deps are the dependencies every handler group needs: the persistence
// layer, the encrypted secret store, and the header-resolver registry.
type Deps struct {
	Store   store.Store
	Secrets *secrets.Service
	Headers *dataplaneheaders.Registry

	// Authorizer backs a route that needs a SECOND permission check beyond
	// the one router.go's routeSpec.Permission already enforces --
	// POST /model-catalog/providers/{id}/connect additionally requires
	// credential.create (and credential.read when reusing an existing
	// credential), on top of the model.create the route table gates it on
	// (see ModelCatalog.Connect). Nil in a test Deps that never exercises
	// that route is fine -- every call site checks for nil and denies
	// rather than panicking. It also backs the MCP catalog's own split
	// (a tenant admin writes tenant entries, only platform.admin writes
	// platform ones).
	Authorizer pkgauth.Authorizer

	// ConnectorOps backs GET .../health and POST .../discover (Connectors
	// handler). cmd/gateway always supplies it when the API plane is
	// enabled, mcp.enabled or not, but it stays a nillable dependency --
	// every route that needs it must check for nil and answer 503 via
	// writeOpsUnavailable rather than panic.
	ConnectorOps ops.ConnectorOps
	// CacheOps backs every /cache/* route (Cache handler). Same nil
	// contract as ConnectorOps.
	CacheOps ops.CacheOps
	// ProfileOps, when non-nil, is called by Profiles' write routes
	// (Update, SetTools, SetSkills, Delete) and by Skills' write routes
	// (Update, Delete, AddVersion) so a running MCP plane's profile
	// enforcer cache (internal/dataplane/profile.Enforcer) drops the
	// affected resolution(s) immediately instead of serving stale
	// tools/instructions/skills for up to its 30s TTL. Nil is a
	// documented no-op -- e.g. an api pod without the MCP plane wired in
	// -- every call site checks for nil and logs rather than panics.
	ProfileOps ops.ProfileOps
	// DefaultTimeoutMS is connectors.default_timeout_ms, the
	// timeout_ms a connector gets when its create request sets none. 0
	// falls back to the column default (defaultConnectorTimeoutMS).
	DefaultTimeoutMS int
	// Analytics backs every /analytics/* route (Analytics handler). Nil
	// when no ClickHouse sink is configured -- every route that needs it
	// must check for nil and answer 404 (see analyticsUnavailableMessage)
	// rather than panic.
	Analytics analytics.Reader
	// AnalyticsMeta carries request-independent facts about this running
	// instance (today: which pricing source is configured) that the
	// Overview handler stamps onto every response alongside the Reader's
	// own fields. Zero value is fine -- PricingSource empty is treated as
	// "embedded" (see Analytics.Overview).
	AnalyticsMeta analytics.Meta
	// BodyStore resolves the body_ref of an LLM call whose bodies were
	// offloaded (llm_proxy.capture.body_store) for GET
	// /analytics/llm-logs/{request_id}. Nil when this instance has no body
	// store configured: the detail then carries the ref and a
	// bodies_unavailable reason instead of the bodies.
	BodyStore sink.BodyStore
	// LLMCalls serves /analytics/llm-logs when Analytics is nil: the
	// Postgres capture store, which records every LLM call. Nil when the
	// capture store is not Postgres.
	LLMCalls analytics.LLMCallReader

	// ModelInvalidator, when non-nil, is called by every successful
	// /models write so the LLM plane's registry cache drops the tenant's
	// entries immediately (see ModelInvalidator). Nil is a documented
	// no-op, e.g. an api pod that does not also serve the LLM plane.
	ModelInvalidator ModelInvalidator

	// AllowedRoles is the set of role names POST /api-keys and POST
	// /api-keys/{id}/rotate accept for the Role field: every role
	// currently configured on the process's Authorizer (built-in plus
	// any config.Auth.Roles overrides/additions), supplied by
	// router.go's buildRoutes. A role not in this set is rejected with a
	// 400 listing the known ones (see validateRole). Nil/empty means no
	// role is accepted -- fail closed rather than silently allowing
	// anything.
	AllowedRoles map[string]bool

	// KeyInvalidator, when non-nil, is notified by APIKeys.Revoke and
	// APIKeys.Rotate so a revoked/rotated-out key's cached lookup is
	// evicted from the running apikey.Authenticator immediately instead
	// of waiting out its CacheTTL. Nil is a valid no-op: handler tests,
	// and any deployment that doesn't wire the api-key Authenticator
	// through, simply skip the call.
	KeyInvalidator KeyInvalidator

	// Console configures the console login/session routes (Auth handler).
	// The zero value is usable: default lifetimes, "auto" cookies.
	Console ConsoleConfig
	// LoginThrottle lets POST /auth/login report failures and successes
	// to the control plane's per-IP lockout (internal/auth.RateLimiter
	// implements it). Nil skips the reporting (the per-user lockout still
	// applies).
	LoginThrottle LoginThrottle

	// IngestConfig configures POST /api/v1/ingest (Ingest handler): off by
	// default, so the route always answers the same 404 an unmounted route
	// would (see Ingest.Ingest's doc comment).
	IngestConfig config.Ingest
	// Capture and LLMCapture are the SAME body-capture config the
	// gateway's own async proxy paths use (config.Capture backs the MCP
	// plane's access log, config.LLMProxy.Capture the LLM plane): Ingest
	// applies their store_bodies flag and byte caps to ingested records too,
	// via the shared sink.TruncateBody helper, so an ingested row is capped
	// identically to a gateway-proxied one.
	Capture    config.Capture
	LLMCapture config.LLMCapture
	// IngestSink backs POST /api/v1/ingest's durable write. Nil when no
	// ClickHouse sink is configured on this instance -- Ingest answers 503
	// rather than calling through a nil.
	IngestSink sink.IngestSink
	// IngestRateLimiter enforces POST /api/v1/ingest's per-API-key
	// requests-per-minute cap (internal/auth.KeyRateLimiter). Nil disables
	// per-key limiting (the route is still gated by IngestConfig.Enabled
	// and the ordinary auth/permission middleware).
	IngestRateLimiter *internalauth.KeyRateLimiter
}

// KeyInvalidator evicts one API key's cached lookup by its content hash
// (store.APIKey.KeyHash) from an Authenticator's in-memory cache. It is
// implemented by *apikey.Authenticator (see its InvalidateKey method);
// the interface lives here, not in internal/auth/apikey, so this package
// doesn't need to import a concrete Authenticator type just to accept
// one.
type KeyInvalidator interface {
	InvalidateKey(keyHash string)
}

// ConsoleConfig is the slice of config.Auth the console login routes need.
type ConsoleConfig struct {
	// APIKeyLogin is reported by GET /auth/config as api_key_login.
	APIKeyLogin bool
	// DefaultTenant is the tenant slug the login form is prefilled with
	// when more than one tenant exists.
	DefaultTenant string
	// CookieSecure is "auto" (default), "true" or "false".
	CookieSecure string
	// SessionIdle and SessionMax are the idle and absolute session
	// lifetimes (defaults 8h / 24h).
	SessionIdle time.Duration
	SessionMax  time.Duration
}

// LoginThrottle is the subset of *internal/auth.RateLimiter the login
// handler uses, so a login failure counts toward the same per-IP lockout
// that API-key failures do.
type LoginThrottle interface {
	ClientIP(r *http.Request) string
	ReportFailure(ip string)
	ReportSuccess(ip string)
}

// connectorTimeoutMS is the timeout_ms for a connector created without one.
func (d Deps) connectorTimeoutMS() int {
	if d.DefaultTimeoutMS > 0 {
		return d.DefaultTimeoutMS
	}
	return defaultConnectorTimeoutMS
}
