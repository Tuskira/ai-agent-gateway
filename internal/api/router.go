// Package api implements the gateway's control-plane REST API: the chi
// router mounted on the API plane, its open health/service-info/docs
// endpoints, and the authenticated endpoints layered on top of them
// (internal/api/handlers).
package api

import (
	"github.com/google/uuid"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/handlers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	dataplaneheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/ops"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Deps are the dependencies NewRouter needs to build the API plane's
// handler.
type Deps struct {
	// ServiceVersion is reported on /api/v1/ and /api/v1/health, and as
	// the OpenAPI document's info.version.
	ServiceVersion string

	// Authenticator is the (already-chained) Authenticator every
	// authenticated route runs behind.
	Authenticator pkgauth.Authenticator
	// Authorizer backs every permission-gated route (internalauth.
	// RequirePermission).
	Authorizer pkgauth.Authorizer
	// Logger is used for the auth middleware's failure logs and any
	// route-level logging.
	Logger *slog.Logger

	// RateLimiter, when non-nil, is mounted as the outermost middleware
	// on every route this router serves (internalauth.RateLimitMiddleware):
	// it enforces the general per-IP request cap on every request and the
	// auth-failure lockout on every non-exempt one (see
	// isRateLimitExempt). Nil disables rate limiting entirely. Its
	// "locked_ips" counter is also reported by GET /api/v1/health.
	RateLimiter *internalauth.RateLimiter

	// Console configures the console login/session routes (/auth/config,
	// /auth/login, ...): cookie security, session lifetimes, the API-key
	// fallback flag, the default tenant. The zero value works (default
	// lifetimes, "auto" cookies, no API-key login, no default tenant).
	Console handlers.ConsoleConfig

	// Store is the control-plane persistence layer backing tenants,
	// API keys, credentials, connectors, agent profiles, console users
	// and sessions, and the tool cache. Required for every authenticated
	// route except /auth/me called with an API key.
	Store store.Store
	// Secrets is the encrypted credential store backing /credentials.
	Secrets *secrets.Service
	// Headers is the header-resolver registry backing
	// /headers/providers.
	Headers *dataplaneheaders.Registry
	// DefaultTimeoutMS is connectors.default_timeout_ms (see handlers.Deps).
	DefaultTimeoutMS int

	// ConnectorOps backs GET /connectors/{id}/health and POST
	// /connectors/{id}/discover. cmd/gateway builds the data plane it
	// comes from whenever the API plane is enabled -- mcp.enabled or not
	// -- so a running gateway always supplies it; left nil, those routes
	// still mount (so the OpenAPI surface stays complete) but answer 503.
	ConnectorOps ops.ConnectorOps
	// CacheOps backs every /cache/* route. Same nil contract as
	// ConnectorOps.
	CacheOps ops.CacheOps
	// ProfileOps, when non-nil, lets Profiles' and Skills' write routes
	// invalidate a running MCP plane's agent-profile enforcer cache
	// immediately instead of waiting out its 30s TTL. Same nil contract
	// as ConnectorOps. See handlers.Deps.ProfileOps.
	ProfileOps ops.ProfileOps
	// Analytics backs every /analytics/* route. Nil when no ClickHouse
	// sink is configured -- those routes still mount (so the OpenAPI
	// surface stays complete) but answer 404.
	Analytics analytics.Reader
	// LLMCalls backs /analytics/llm-logs without ClickHouse (see
	// handlers.Deps.LLMCalls).
	LLMCalls analytics.LLMCallReader
	// AnalyticsMeta carries request-independent facts (today: the
	// configured pricing source) that GET /analytics/overview stamps onto
	// its response. See handlers.Deps.AnalyticsMeta.
	AnalyticsMeta analytics.Meta
	// BodyStore resolves offloaded LLM-call bodies for the LLM-log detail
	// route. Nil = no body store on this instance. See
	// handlers.Deps.BodyStore.
	BodyStore sink.BodyStore

	// IngestConfig configures POST /api/v1/ingest (handlers.Ingest): off
	// by default (Enabled: false), so the route always 404s. See
	// handlers.Deps.IngestConfig.
	IngestConfig config.Ingest
	// Capture and LLMCapture supply the body-capture rules (store_bodies +
	// byte caps) POST /api/v1/ingest applies to access-log and LLM-call
	// records respectively -- the SAME rules and byte caps the gateway's
	// own async proxy paths apply (config.Capture backs the MCP plane's
	// access log; config.LLMProxy.Capture backs the LLM plane), so an
	// ingested row is bounded identically to a gateway-proxied one. See
	// handlers.Deps.Capture / LLMCapture.
	Capture    config.Capture
	LLMCapture config.LLMCapture
	// IngestSink backs the durable write POST /api/v1/ingest performs. Nil
	// when no ClickHouse sink is configured on this instance -- the route
	// still mounts (so the OpenAPI surface stays complete) but answers 503.
	// See handlers.Deps.IngestSink.
	IngestSink sink.IngestSink
	// IngestRateLimiter enforces POST /api/v1/ingest's per-API-key
	// requests-per-minute cap (see handlers.Deps.IngestRateLimiter and
	// internal/auth.KeyRateLimiter's doc comment for why this route uses a
	// per-key limiter instead of the control plane's per-IP one). Nil
	// disables per-key limiting entirely (the route is still gated by
	// IngestConfig.Enabled and the ordinary auth/permission checks).
	IngestRateLimiter *internalauth.KeyRateLimiter

	// SinksStatus, when non-nil, is called on every GET /health request
	// to report each configured log sink's state (enabled, dropped
	// count) under the response's "sinks" key. Left nil, /health omits
	// "sinks" entirely.
	SinksStatus func() map[string]any

	// LimitsStatus, when non-nil, is called on every GET /health request
	// to report the LLM plane's per-key limit denials (budget_denials,
	// rpm_denials) under "limits". Nil (no LLM plane in this process)
	// omits the key.
	LimitsStatus func() map[string]any

	// ServeUI mounts the embedded React admin console at "/" when true
	// (config: api.serve_ui / GATEWAY_API_SERVE_UI). When false, "/" is
	// not registered at all -- only /api/v1/* is served.
	ServeUI bool

	// ModelInvalidator, when non-nil, is called by every successful
	// /models write so a co-hosted LLM plane's registry cache drops the
	// tenant's entries at once (see handlers.ModelInvalidator;
	// internal/llmplane.Registry implements it). Nil is a valid no-op.
	ModelInvalidator handlers.ModelInvalidator
	// KeyInvalidator, when non-nil, is called by POST /api-keys revoke
	// and rotate so a revoked/rotated-out key's cached lookup is evicted
	// from the running apikey.Authenticator immediately, instead of
	// waiting out its CacheTTL. Nil is a valid no-op (e.g. in tests, or
	// when the api-key Authenticator isn't wired into this process at
	// all) -- see handlers.KeyInvalidator.
	KeyInvalidator handlers.KeyInvalidator
	// UIHandler serves the embedded admin console. Required when ServeUI
	// is true; ignored otherwise. Kept as an injected http.Handler
	// (rather than NewRouter importing internal/api/ui directly) so
	// router tests don't need a real UI build.
	UIHandler http.Handler
}

// routeSpec is one API route: enough to both mount it on the chi router
// and describe it in the generated OpenAPI document, from a single
// source of truth (see buildRoutes and openapi.go) so the mounted routes
// and the served spec can never drift out of sync --
// TestOpenAPI_CoversEveryRoute (router_test.go) asserts exactly that by
// walking the live chi tree.
type routeSpec struct {
	Method  string // http.MethodGet, etc.
	Pattern string // chi pattern relative to /api/v1, e.g. "/connectors/{id}"
	Tag     string
	Summary string
	// Public routes are mounted outside the auth middleware group
	// (health, service info, docs, openapi.json).
	Public bool
	// Permission is checked with internalauth.RequirePermission, in
	// addition to authentication, when non-empty. Every non-Public route
	// without a Permission is still behind the auth middleware -- it
	// just requires no permission beyond "authenticated" (e.g.
	// /auth/me).
	Permission string
	Handler    http.HandlerFunc
}

// NewRouter returns the API plane's http.Handler. Every route is declared
// once in buildRoutes and mounted from that table; GET /api/v1/openapi.json
// is generated from the same table (see openapi.go).
//
//   - GET /                       the embedded admin console (only when
//     deps.ServeUI is true), with an SPA fallback for client-side routes
//   - GET /api/v1/                service info, no auth
//   - GET /api/v1/health          liveness probe, no auth
//   - GET /api/v1/openapi.json    the generated OpenAPI document, no auth
//   - GET /api/v1/docs            an API reference page, no auth
//   - everything else under /api/v1/* is authenticated (see buildRoutes)
//
// /api/v1/* is registered before "/" so API routes always take precedence
// over the UI's catch-all.
func NewRouter(deps Deps) http.Handler {
	r := chi.NewRouter()
	routes := buildRoutes(deps)

	// Mounted first so it wraps every response this router serves,
	// including the UI mount below and error responses (404s, rate-limit
	// rejections) from the middleware mounted after it -- see
	// securityHeaders' doc comment.
	r.Use(securityHeaders)

	// Mounted before any route so it wraps everything this router serves,
	// including the UI mount below: the general per-IP cap counts every
	// request, and the failure lockout is skipped only for the specific
	// paths isRateLimitExempt names.
	if deps.RateLimiter != nil {
		r.Use(internalauth.RateLimitMiddleware(deps.RateLimiter, isRateLimitExempt, isIngestPath))
	}

	r.Route("/api/v1", func(r chi.Router) {
		for _, rt := range routes {
			if !rt.Public {
				continue
			}
			r.MethodFunc(rt.Method, rt.Pattern, rt.Handler)
		}

		r.Group(func(r chi.Router) {
			r.Use(internalauth.Middleware(deps.Authenticator, deps.Logger))
			// Both only ever act on a console-user (session) principal;
			// an API key passes through untouched. CSRF first, so a
			// forged request is refused before anything else is said.
			r.Use(session.CSRF)
			r.Use(session.MustChangeGate(mustChangeAllowed))
			for _, rt := range routes {
				if rt.Public {
					continue
				}
				// Permission is checked before the id is parsed so an
				// unauthorised caller learns nothing about which ids exist.
				h := requireUUIDParam("id")(rt.Handler)
				if rt.Permission != "" {
					r.With(internalauth.RequirePermission(deps.Authorizer, rt.Permission)).Method(rt.Method, rt.Pattern, h)
				} else {
					r.Method(rt.Method, rt.Pattern, h)
				}
			}
		})

		// Any other /api/v1/* path is an unknown API route: fall through
		// to a JSON 404 rather than the UI's SPA/index.html fallback.
		r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			httpx.WriteError(w, http.StatusNotFound, httpx.TypeNotFound, "not found")
		})
	})

	if deps.ServeUI {
		r.Mount("/", deps.UIHandler)
	}

	return r
}

// buildRoutes returns every /api/v1 route, relative to that prefix. It is
// the single source of truth NewRouter mounts from and buildOpenAPI
// documents from. The two docs.* routes' handler is primed with the
// generated spec after the full table (including those two routes) is
// assembled, via Docs's pointer receiver -- see handlers.Docs.
func buildRoutes(deps Deps) []routeSpec {
	hdeps := handlers.Deps{
		Store:             deps.Store,
		Secrets:           deps.Secrets,
		Headers:           deps.Headers,
		Authorizer:        deps.Authorizer,
		ConnectorOps:      deps.ConnectorOps,
		CacheOps:          deps.CacheOps,
		ProfileOps:        deps.ProfileOps,
		DefaultTimeoutMS:  deps.DefaultTimeoutMS,
		Analytics:         deps.Analytics,
		AnalyticsMeta:     deps.AnalyticsMeta,
		BodyStore:         deps.BodyStore,
		LLMCalls:          deps.LLMCalls,
		AllowedRoles:      allowedRoles(deps.Authorizer),
		KeyInvalidator:    deps.KeyInvalidator,
		ModelInvalidator:  deps.ModelInvalidator,
		IngestConfig:      deps.IngestConfig,
		Capture:           deps.Capture,
		LLMCapture:        deps.LLMCapture,
		IngestSink:        deps.IngestSink,
		IngestRateLimiter: deps.IngestRateLimiter,
		Console:           deps.Console,
	}
	if deps.RateLimiter != nil {
		// Only when set: a nil *RateLimiter in the interface would be a
		// non-nil LoginThrottle.
		hdeps.LoginThrottle = deps.RateLimiter
	}

	tenants := handlers.Tenants{Deps: hdeps}
	apiKeys := handlers.APIKeys{Deps: hdeps}
	credentials := handlers.Credentials{Deps: hdeps}
	connectors := handlers.Connectors{Deps: hdeps}
	profiles := handlers.Profiles{Deps: hdeps}
	headerProviders := handlers.Headers{Deps: hdeps}
	cacheOps := handlers.Cache{Deps: hdeps}
	analyticsH := handlers.Analytics{Deps: hdeps}
	models := handlers.Models{Deps: hdeps}
	modelCatalog := handlers.ModelCatalog{Deps: hdeps}
	skillsH := handlers.Skills{Deps: hdeps}
	mcpCatalog := handlers.MCPCatalog{Deps: hdeps}
	ingestH := handlers.Ingest{Deps: hdeps}
	authH := handlers.Auth{Deps: hdeps}
	usersH := handlers.Users{Deps: hdeps}
	docs := &handlers.Docs{}

	routes := []routeSpec{
		{Method: http.MethodGet, Pattern: "/", Tag: "service", Summary: "Service info", Public: true,
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				httpx.WriteJSON(w, http.StatusOK, map[string]string{
					"service": "tusk-ai-secured-gateway",
					"plane":   "api",
					"version": deps.ServiceVersion,
				})
			}},
		{Method: http.MethodGet, Pattern: "/health", Tag: "service", Summary: "Liveness probe", Public: true,
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				body := map[string]any{
					"status":  "ok",
					"plane":   "api",
					"version": deps.ServiceVersion,
				}
				if deps.SinksStatus != nil {
					body["sinks"] = deps.SinksStatus()
				}
				if deps.LimitsStatus != nil {
					body["limits"] = deps.LimitsStatus()
				}
				if deps.RateLimiter != nil {
					body["rate_limit"] = map[string]any{"locked_ips": deps.RateLimiter.LockedIPCount()}
				}
				httpx.WriteJSON(w, http.StatusOK, body)
			}},
		{Method: http.MethodGet, Pattern: "/docs", Tag: "service", Summary: "API reference page", Public: true, Handler: docs.Page},
		{Method: http.MethodGet, Pattern: "/openapi.json", Tag: "service", Summary: "OpenAPI document", Public: true, Handler: docs.OpenAPI},

		// Console login. config and login are public (there is no session
		// yet); login sits behind the same per-IP lockout as every route
		// and adds a per-user one (handlers.Auth).
		{Method: http.MethodGet, Pattern: "/auth/config", Tag: "auth", Summary: "What the console login page should offer (password login, API-key fallback, tenant prefill)", Public: true, Handler: authH.Config},
		{Method: http.MethodPost, Pattern: "/auth/login", Tag: "auth", Summary: "Log in with tenant, username and password; sets the session cookie", Public: true, Handler: authH.Login},
		{Method: http.MethodPost, Pattern: "/auth/logout", Tag: "auth", Summary: "Revoke the current session and clear the cookie", Handler: authH.Logout},
		{Method: http.MethodGet, Pattern: "/auth/me", Tag: "auth", Summary: "The caller's Principal (and, for a console user, the account and CSRF token)", Handler: authH.Me},
		{Method: http.MethodPost, Pattern: "/auth/password", Tag: "auth", Summary: "Change your own password (console user session only)", Handler: authH.ChangePassword},
		// users.manage, not user.read/...: a read-only user's or agent's
		// "*.read" grant would satisfy a "user.read" permission (see
		// handlers.Users and pkgauth.PermUsersManage).
		{Method: http.MethodGet, Pattern: "/auth/audit", Tag: "auth", Summary: "The tenant's authentication audit trail", Permission: pkgauth.PermUsersManage, Handler: usersH.Audit},

		{Method: http.MethodPost, Pattern: "/users", Tag: "users", Summary: "Create a console user; returns a one-time temporary password when none is given", Permission: pkgauth.PermUsersManage, Handler: usersH.Create},
		{Method: http.MethodGet, Pattern: "/users", Tag: "users", Summary: "List the tenant's console users", Permission: pkgauth.PermUsersManage, Handler: usersH.List},
		{Method: http.MethodGet, Pattern: "/users/{id}", Tag: "users", Summary: "Get a console user", Permission: pkgauth.PermUsersManage, Handler: usersH.Get},
		{Method: http.MethodPatch, Pattern: "/users/{id}", Tag: "users", Summary: "Update a user's display name, role or disabled flag (last-admin and self guards apply)", Permission: pkgauth.PermUsersManage, Handler: usersH.Update},
		{Method: http.MethodDelete, Pattern: "/users/{id}", Tag: "users", Summary: "Delete a console user (soft; last-admin and self guards apply)", Permission: pkgauth.PermUsersManage, Handler: usersH.Delete},
		{Method: http.MethodPost, Pattern: "/users/{id}/reset-password", Tag: "users", Summary: "Reset a user's password to a one-time temporary one and revoke their sessions", Permission: pkgauth.PermUsersManage, Handler: usersH.ResetPassword},
		{Method: http.MethodPost, Pattern: "/users/{id}/revoke-sessions", Tag: "users", Summary: "Revoke all of a user's sessions", Permission: pkgauth.PermUsersManage, Handler: usersH.RevokeSessions},

		// Gated on platform.admin, not tenant.create/tenant.read: see
		// handlers.Tenants' doc comment and pkgauth.matchPermission for
		// why an ordinary "*.read"/"*" grant must not satisfy this.
		{Method: http.MethodPost, Pattern: "/tenants", Tag: "tenants", Summary: "Create a tenant", Permission: "platform.admin", Handler: tenants.Create},
		{Method: http.MethodGet, Pattern: "/tenants", Tag: "tenants", Summary: "List tenants", Permission: "platform.admin", Handler: tenants.List},

		{Method: http.MethodPost, Pattern: "/api-keys", Tag: "api-keys", Summary: "Create an API key", Permission: "admin.manage", Handler: apiKeys.Create},
		{Method: http.MethodGet, Pattern: "/api-keys", Tag: "api-keys", Summary: "List API keys", Permission: "admin.manage", Handler: apiKeys.List},
		{Method: http.MethodPatch, Pattern: "/api-keys/{id}", Tag: "api-keys", Summary: "Update an API key's limits or profile binding", Permission: "admin.manage", Handler: apiKeys.Update},
		{Method: http.MethodDelete, Pattern: "/api-keys/{id}", Tag: "api-keys", Summary: "Revoke an API key", Permission: "admin.manage", Handler: apiKeys.Revoke},
		{Method: http.MethodPost, Pattern: "/api-keys/{id}/rotate", Tag: "api-keys", Summary: "Rotate an API key", Permission: "admin.manage", Handler: apiKeys.Rotate},

		{Method: http.MethodPost, Pattern: "/credentials", Tag: "credentials", Summary: "Create a credential", Permission: "admin.manage", Handler: credentials.Create},
		{Method: http.MethodGet, Pattern: "/credentials", Tag: "credentials", Summary: "List credentials", Permission: "admin.manage", Handler: credentials.List},
		{Method: http.MethodGet, Pattern: "/credentials/{name}", Tag: "credentials", Summary: "Get a credential's metadata", Permission: "admin.manage", Handler: credentials.Get},
		{Method: http.MethodPut, Pattern: "/credentials/{name}", Tag: "credentials", Summary: "Rotate a credential's payload", Permission: "admin.manage", Handler: credentials.Rotate},
		{Method: http.MethodDelete, Pattern: "/credentials/{name}", Tag: "credentials", Summary: "Delete a credential", Permission: "admin.manage", Handler: credentials.Delete},

		{Method: http.MethodPost, Pattern: "/connectors", Tag: "connectors", Summary: "Create a connector", Permission: "connector.create", Handler: connectors.Create},
		{Method: http.MethodGet, Pattern: "/connectors", Tag: "connectors", Summary: "List connectors", Permission: "connector.read", Handler: connectors.List},
		{Method: http.MethodGet, Pattern: "/connectors/{id}", Tag: "connectors", Summary: "Get a connector", Permission: "connector.read", Handler: connectors.Get},
		{Method: http.MethodPut, Pattern: "/connectors/{id}", Tag: "connectors", Summary: "Update a connector", Permission: "connector.update", Handler: connectors.Update},
		{Method: http.MethodDelete, Pattern: "/connectors/{id}", Tag: "connectors", Summary: "Delete a connector", Permission: "connector.delete", Handler: connectors.Delete},
		{Method: http.MethodGet, Pattern: "/connectors/{id}/tools", Tag: "connectors", Summary: "List a connector's cached tools", Permission: "connector.read", Handler: connectors.Tools},
		{Method: http.MethodGet, Pattern: "/connectors/{id}/health", Tag: "connectors", Summary: "Probe a connector's health", Permission: "connector.read", Handler: connectors.Health},
		{Method: http.MethodPost, Pattern: "/connectors/{id}/discover", Tag: "connectors", Summary: "Trigger tool discovery", Permission: "connector.update", Handler: connectors.Discover},
		{Method: http.MethodGet, Pattern: "/mcp-catalog", Tag: "mcp-catalog", Summary: "List the MCP catalog with this tenant's added flags", Permission: "connector.read", Handler: mcpCatalog.List},
		{Method: http.MethodGet, Pattern: "/mcp-catalog/{slug}", Tag: "mcp-catalog", Summary: "Get one MCP catalog entry", Permission: "connector.read", Handler: mcpCatalog.Get},
		{Method: http.MethodPost, Pattern: "/mcp-catalog", Tag: "mcp-catalog", Summary: "Create a catalog entry (tenant, or platform with platform.admin)", Permission: "connector.create", Handler: mcpCatalog.Create},
		{Method: http.MethodPut, Pattern: "/mcp-catalog/{slug}", Tag: "mcp-catalog", Summary: "Update a catalog entry", Permission: "connector.update", Handler: mcpCatalog.Update},
		{Method: http.MethodDelete, Pattern: "/mcp-catalog/{slug}", Tag: "mcp-catalog", Summary: "Delete a catalog entry", Permission: "connector.delete", Handler: mcpCatalog.Delete},
		{Method: http.MethodPost, Pattern: "/mcp-catalog/{slug}/add", Tag: "mcp-catalog", Summary: "Add a catalog entry to the tenant as a connector", Permission: "connector.create", Handler: mcpCatalog.Add},

		{Method: http.MethodPost, Pattern: "/profiles", Tag: "profiles", Summary: "Create an agent profile", Permission: "profile.create", Handler: profiles.Create},
		{Method: http.MethodGet, Pattern: "/profiles", Tag: "profiles", Summary: "List agent profiles", Permission: "profile.read", Handler: profiles.List},
		{Method: http.MethodGet, Pattern: "/profiles/{id}", Tag: "profiles", Summary: "Get an agent profile", Permission: "profile.read", Handler: profiles.Get},
		{Method: http.MethodPut, Pattern: "/profiles/{id}", Tag: "profiles", Summary: "Update an agent profile", Permission: "profile.update", Handler: profiles.Update},
		{Method: http.MethodDelete, Pattern: "/profiles/{id}", Tag: "profiles", Summary: "Delete an agent profile", Permission: "profile.delete", Handler: profiles.Delete},
		{Method: http.MethodPut, Pattern: "/profiles/{id}/tools", Tag: "profiles", Summary: "Replace a profile's tool allow-list", Permission: "profile.update", Handler: profiles.SetTools},
		{Method: http.MethodGet, Pattern: "/profiles/{id}/tools", Tag: "profiles", Summary: "Get a profile's tool allow-list", Permission: "profile.read", Handler: profiles.GetTools},
		{Method: http.MethodPut, Pattern: "/profiles/{id}/skills", Tag: "profiles", Summary: "Replace a profile's attached skills/commands (see the skills & commands registry)", Permission: "profile.update", Handler: profiles.SetSkills},
		{Method: http.MethodGet, Pattern: "/profiles/{id}/skills", Tag: "profiles", Summary: "Get a profile's attached skills/commands", Permission: "profile.read", Handler: profiles.GetSkills},

		{Method: http.MethodPost, Pattern: "/models", Tag: "models", Summary: "Register a model (LLM-plane model registry)", Permission: "model.create", Handler: models.Create},
		{Method: http.MethodGet, Pattern: "/models", Tag: "models", Summary: "List registered models (tenant rows + platform defaults)", Permission: "model.read", Handler: models.List},
		{Method: http.MethodGet, Pattern: "/models/{id}", Tag: "models", Summary: "Get a registered model", Permission: "model.read", Handler: models.Get},
		{Method: http.MethodPut, Pattern: "/models/{id}", Tag: "models", Summary: "Update a registered model (tenant rows only)", Permission: "model.update", Handler: models.Update},
		{Method: http.MethodDelete, Pattern: "/models/{id}", Tag: "models", Summary: "Delete a registered model (soft; tenant rows only)", Permission: "model.delete", Handler: models.Delete},

		{Method: http.MethodGet, Pattern: "/model-catalog", Tag: "model-catalog", Summary: "List the platform model catalog (providers and their models), merged with this tenant's registered models", Permission: "model.read", Handler: modelCatalog.List},
		{Method: http.MethodPost, Pattern: "/model-catalog/providers", Tag: "model-catalog", Summary: "Create a catalog provider", Permission: pkgauth.PermCatalogManage, Handler: modelCatalog.CreateProvider},
		{Method: http.MethodPut, Pattern: "/model-catalog/providers/{id}", Tag: "model-catalog", Summary: "Update a catalog provider", Permission: pkgauth.PermCatalogManage, Handler: modelCatalog.UpdateProvider},
		{Method: http.MethodDelete, Pattern: "/model-catalog/providers/{id}", Tag: "model-catalog", Summary: "Delete a catalog provider (409 with a usage count if any tenant model was connected from one of its models, unless ?force=true)", Permission: pkgauth.PermCatalogManage, Handler: modelCatalog.DeleteProvider},
		{Method: http.MethodPost, Pattern: "/model-catalog/providers/{id}/models", Tag: "model-catalog", Summary: "Add a model to a catalog provider", Permission: pkgauth.PermCatalogManage, Handler: modelCatalog.CreateModel},
		{Method: http.MethodPut, Pattern: "/model-catalog/models/{id}", Tag: "model-catalog", Summary: "Update a catalog model", Permission: pkgauth.PermCatalogManage, Handler: modelCatalog.UpdateModel},
		{Method: http.MethodDelete, Pattern: "/model-catalog/models/{id}", Tag: "model-catalog", Summary: "Delete a catalog model (same 409 rule as deleting a provider)", Permission: pkgauth.PermCatalogManage, Handler: modelCatalog.DeleteModel},
		{Method: http.MethodGet, Pattern: "/model-catalog/models/{id}/usage", Tag: "model-catalog", Summary: "How many tenant models (across every tenant) were connected from this catalog model", Permission: pkgauth.PermCatalogManage, Handler: modelCatalog.ModelUsage},
		{Method: http.MethodPost, Pattern: "/model-catalog/providers/{id}/test", Tag: "model-catalog", Summary: "Test a provider's base_url and an API key (raw or an existing credential) by calling its models endpoint", Permission: "model.create", Handler: modelCatalog.Test},
		{Method: http.MethodPost, Pattern: "/model-catalog/providers/{id}/connect", Tag: "model-catalog", Summary: "Connect a provider: create or reuse a credential and register the selected catalog models as tenant models (requires credential.create, or credential.read too when reusing an existing credential)", Permission: "model.create", Handler: modelCatalog.Connect},
		{Method: http.MethodPost, Pattern: "/model-catalog/providers/{id}/prices/preview", Tag: "model-catalog", Summary: "Preview a price refresh for a provider: fetch its current prices from its own pricing API and diff them against the catalog (read-only; nebius and together only)", Permission: pkgauth.PermCatalogManage, Handler: modelCatalog.PreviewPrices},
		{Method: http.MethodPost, Pattern: "/model-catalog/providers/{id}/prices/apply", Tag: "model-catalog", Summary: "Apply a price refresh: write the given prices onto the catalog, optionally also onto every tenant model still using the catalog's old price", Permission: pkgauth.PermCatalogManage, Handler: modelCatalog.ApplyPrices},

		{Method: http.MethodPost, Pattern: "/skills", Tag: "skills", Summary: "Register a skill or command in the skills & commands registry", Permission: "skill.create", Handler: skillsH.Create},
		{Method: http.MethodGet, Pattern: "/skills", Tag: "skills", Summary: "List registered skills/commands (tenant rows + platform defaults; ?kind=skill|command)", Permission: "skill.read", Handler: skillsH.List},
		{Method: http.MethodGet, Pattern: "/skills/{id}", Tag: "skills", Summary: "Get a registered skill/command, including its latest version's files", Permission: "skill.read", Handler: skillsH.Get},
		{Method: http.MethodPut, Pattern: "/skills/{id}", Tag: "skills", Summary: "Partially update a skill/command (tenant rows only)", Permission: "skill.update", Handler: skillsH.Update},
		{Method: http.MethodDelete, Pattern: "/skills/{id}", Tag: "skills", Summary: "Delete a skill/command (soft; tenant rows only)", Permission: "skill.delete", Handler: skillsH.Delete},
		{Method: http.MethodPost, Pattern: "/skills/{id}/versions", Tag: "skills", Summary: "Add a new version's files to a skill/command (tenant rows only)", Permission: "skill.update", Handler: skillsH.AddVersion},
		{Method: http.MethodGet, Pattern: "/skills/{id}/versions", Tag: "skills", Summary: "List a skill/command's versions (no file bodies)", Permission: "skill.read", Handler: skillsH.ListVersions},
		{Method: http.MethodGet, Pattern: "/skills/{id}/versions/{v}", Tag: "skills", Summary: "Get one version of a skill/command, with files", Permission: "skill.read", Handler: skillsH.GetVersion},

		{Method: http.MethodGet, Pattern: "/headers/providers", Tag: "headers", Summary: "List header resolver types and external providers", Permission: "headers.read", Handler: headerProviders.List},

		{Method: http.MethodGet, Pattern: "/cache/stats", Tag: "cache", Summary: "Tool cache statistics", Permission: "cache.read", Handler: cacheOps.Stats},
		{Method: http.MethodGet, Pattern: "/cache/search", Tag: "cache", Summary: "Search cached tools (?q=&limit=&include_stale=, excludes stale rows unless include_stale=true)", Permission: "cache.read", Handler: cacheOps.Search},
		{Method: http.MethodPost, Pattern: "/cache/refresh", Tag: "cache", Summary: "Refresh the tool cache for every connector", Permission: "cache.manage", Handler: cacheOps.RefreshAll},
		{Method: http.MethodPost, Pattern: "/cache/refresh/connectors/{id}", Tag: "cache", Summary: "Refresh the tool cache for one connector", Permission: "cache.manage", Handler: cacheOps.RefreshConnector},
		{Method: http.MethodDelete, Pattern: "/cache", Tag: "cache", Summary: "Invalidate the tool cache for every connector", Permission: "cache.manage", Handler: cacheOps.InvalidateAll},
		{Method: http.MethodDelete, Pattern: "/cache/connectors/{id}", Tag: "cache", Summary: "Invalidate the tool cache for one connector", Permission: "cache.manage", Handler: cacheOps.InvalidateConnector},

		{Method: http.MethodGet, Pattern: "/analytics/overview", Tag: "analytics", Summary: "Overview metrics (KPIs, usage, traffic) for a time range", Permission: "analytics.read", Handler: analyticsH.Overview},
		{Method: http.MethodGet, Pattern: "/analytics/models", Tag: "analytics", Summary: "Per-model usage summary (calls, tokens, cost, used by) for a time range, default 7d", Permission: "analytics.read", Handler: analyticsH.Models},
		{Method: http.MethodGet, Pattern: "/analytics/skills", Tag: "analytics", Summary: "Per skill/command usage summary (calls, used by, last seen) for a time range, default 7d", Permission: "analytics.read", Handler: analyticsH.Skills},
		{Method: http.MethodGet, Pattern: "/analytics/skills/usage", Tag: "analytics", Summary: "Skills the model was observed using in LLM traffic, with registered state (discovered = used but not registered), default 7d", Permission: "analytics.read", Handler: analyticsH.SkillsUsage},
		{Method: http.MethodGet, Pattern: "/analytics/mcps/usage", Tag: "analytics", Summary: "MCP servers/tools the model was observed using in LLM traffic, with connector registration and via-gateway attribution, default 7d", Permission: "analytics.read", Handler: analyticsH.MCPsUsage},
		{Method: http.MethodGet, Pattern: "/analytics/client-models", Tag: "analytics", Summary: "Client -> model -> provider usage Sankey graph for a time range, default 7d", Permission: "analytics.read", Handler: analyticsH.ClientModelSankey},
		{Method: http.MethodGet, Pattern: "/analytics/traffic-flow", Tag: "analytics", Summary: "Client -> path -> model | connector traffic-flow Sankey graph across both planes for a time range, default 7d", Permission: "analytics.read", Handler: analyticsH.TrafficFlow},
		{Method: http.MethodGet, Pattern: "/analytics/logs", Tag: "analytics", Summary: "List MCP access-log rows (no bodies)", Permission: "analytics.read", Handler: analyticsH.Logs},
		{Method: http.MethodGet, Pattern: "/analytics/logs/{request_id}", Tag: "analytics", Summary: "Get one MCP access-log row, including bodies", Permission: "admin.manage", Handler: analyticsH.LogsGet},
		{Method: http.MethodGet, Pattern: "/analytics/llm-logs", Tag: "analytics", Summary: "List LLM-call rows (no bodies)", Permission: "analytics.read", Handler: analyticsH.LLMLogs},
		{Method: http.MethodGet, Pattern: "/analytics/llm-logs/{request_id}", Tag: "analytics", Summary: "Get one LLM-call row, including bodies", Permission: "admin.manage", Handler: analyticsH.LLMLogsGet},
		{Method: http.MethodGet, Pattern: "/analytics/sessions/{session_id}/timeline", Tag: "analytics", Summary: "Merged, time-ordered MCP+LLM event timeline for one gateway session, under the ownership rule", Permission: "analytics.read", Handler: analyticsH.SessionTimeline},

		// Fed by a companion capture component, not by a normal
		// gateway-proxied caller -- see
		// docs/api.md and handlers.Ingest's doc comment. Gated on
		// "ingest.write", which only the built-in "interceptor" role (and
		// admin's blanket "*") grants (pkg/auth.NewRoleAuthorizer).
		{Method: http.MethodPost, Pattern: "/ingest", Tag: "ingest", Summary: "Ingest externally captured LLM/access-log records", Permission: "ingest.write", Handler: ingestH.Ingest},
	}

	docs.Spec = buildOpenAPI(routes, deps.ServiceVersion)
	return routes
}

// allowedRoles returns the set of role names an API key may be created
// with: every role currently configured on z. Roles are hardcoded no
// longer (see internal/config's Auth.Roles) -- POST /api-keys validates
// req.Role against exactly this set (handlers.APIKeys.Create/Rotate)
// instead of a hardcoded {admin, agent} pair, so a custom role defined
// via config.Auth.Roles is automatically assignable.
//
// z is typed as the pkgauth.Authorizer interface (so router.go
// doesn't force every caller to use pkgauth.RoleAuthorizer), so this
// type-asserts to the concrete type that actually holds a role table.
// Any other Authorizer implementation yields an empty set, which makes
// APIKeys.Create/Rotate reject every role -- fail closed, not open.
func allowedRoles(z pkgauth.Authorizer) map[string]bool {
	ra, ok := z.(*pkgauth.RoleAuthorizer)
	if !ok {
		return nil
	}
	out := make(map[string]bool, len(ra.Rules))
	for name := range ra.Rules {
		out[name] = true
	}
	return out
}

// mustChangeAllowed reports whether a console user who must change their
// password may make r: read their own identity, change the password, or
// log out. (GET /auth/config is public, so never gated.) Anything else is
// refused with 403 password_change_required.
func mustChangeAllowed(r *http.Request) bool {
	switch r.Method + " " + r.URL.Path {
	case "GET /api/v1/auth/me", "POST /api/v1/auth/password", "POST /api/v1/auth/logout":
		return true
	}
	return false
}

// isRateLimitExempt reports whether r is exempt from the rate limiter's
// auth-failure lockout -- but NOT from its general per-IP request cap,
// which every request counts toward regardless (see
// internalauth.RateLimitMiddleware). Exempt: the control plane's own
// unauthenticated service routes (health, docs, the OpenAPI document), and
// everything outside /api/v1/* -- i.e. the embedded UI's static files and
// SPA routes mounted at "/", none of which ever call the authenticator.
func isRateLimitExempt(r *http.Request) bool {
	switch r.URL.Path {
	case "/api/v1/health", "/api/v1/openapi.json", "/api/v1/docs", "/api/v1/", "/api/v1":
		return true
	}
	return !strings.HasPrefix(r.URL.Path, "/api/v1/")
}

// isIngestPath matches POST /api/v1/ingest: the one route exempt from the
// control plane's general per-IP requests-per-minute cap (see
// internalauth.RateLimitMiddleware's generalCapExempt parameter and
// handlers.Ingest's doc comment for why -- office NAT + a separate
// per-key limiter applied inside the handler).
func isIngestPath(r *http.Request) bool {
	return r.URL.Path == "/api/v1/ingest"
}

// requireUUIDParam rejects requests whose {name} path parameter is not a
// UUID with a 404 before any handler runs. Every id column is a Postgres
// uuid; without this the driver fails the query first and the caller sees
// a 500 instead of "not found".
func requireUUIDParam(name string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if v := chi.URLParam(r, name); v != "" {
				if _, err := uuid.Parse(v); err != nil {
					httpx.NotFound(w, "not found")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
