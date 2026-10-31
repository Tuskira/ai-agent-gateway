// Package auth defines the gateway's identity contract: the Principal every
// authenticated request carries, the Authenticator chain that produces one,
// and the Authorizer that turns roles into permission grants.
//
// This package defines interfaces and a default role-based Authorizer only;
// concrete Authenticators (API key, OIDC, dev-mode) are built in a later
// commit and registered against this contract.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Principal is the authenticated identity attached to every request that
// reaches a plane handler. It is the one type every Authenticator produces
// and every Authorizer consumes.
type Principal struct {
	Subject string // user id / key id
	// TenantID is the tenant every tenant-scoped handler acts in. It is
	// the principal's own tenant unless a platform-admin selected another
	// one for this request (see HomeTenantID).
	TenantID string
	// HomeTenantID is set only while a platform-admin is acting in a
	// tenant other than its own: it then holds the principal's real
	// tenant and TenantID holds the acting one. Empty otherwise. Read it
	// through Home().
	HomeTenantID string
	Email        string
	Roles        []string // "admin" | "platform-admin" | "agent" | "viewer" (+ custom)

	AuthMethod string // "apikey" | "session" | "oidc" | "dev"
	KeyID      string // api_keys.id when AuthMethod == "apikey"
	// ProfileID is the agent profile the API key is bound to (empty when
	// unbound). The MCP plane enforces it and ignores/rejects the
	// X-Agent-Profile-Name header accordingly.
	ProfileID string
	// ToolsRestricted is set when the API key is restricted (the middle
	// rung of the containment ladder): it may call models, but the MCP
	// plane refuses every tool call. Tools stay listed, so each attempt
	// is refused visibly rather than silently impossible.
	ToolsRestricted bool

	// The fields below are set only when AuthMethod == AuthMethodSession,
	// i.e. for a console user logged in with a session cookie. Subject is
	// then the user id.
	Username           string
	SessionID          string // user_sessions.id (the token hash)
	CSRFToken          string // the session's double-submit CSRF token
	MustChangePassword bool   // the user must change their password before anything else

	// RawCredential is the original bearer credential, kept so header
	// resolvers configured with type "token_field" (e.g. bearer_token
	// forwarding) can recover it without re-parsing the request.
	RawCredential string
}

// Home returns the tenant the principal really belongs to: HomeTenantID
// while a platform-admin is acting in another tenant, TenantID otherwise.
// Self-service routes (own profile, password change, logout) and audit
// actor attribution use it so they never follow an acting-tenant
// override. A nil Principal has no home and returns "".
func (p *Principal) Home() string {
	if p == nil {
		return ""
	}
	if p.HomeTenantID != "" {
		return p.HomeTenantID
	}
	return p.TenantID
}

// HasRole reports whether role is among p.Roles. A nil Principal holds
// no role.
func (p *Principal) HasRole(role string) bool {
	if p == nil {
		return false
	}
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// Authenticator resolves an inbound HTTP request to a Principal. Chains of
// Authenticators are tried in order; Authenticate returns ErrNoCredential
// when the request simply doesn't carry a credential this Authenticator
// understands (so the chain should try the next one), and ErrInvalid when
// it does but the credential is malformed, expired, or unknown.
type Authenticator interface {
	Name() string
	Authenticate(ctx context.Context, r *http.Request) (*Principal, error)
}

// AuthMethodSession is Principal.AuthMethod for a console user
// authenticated by a session cookie (internal/auth/session).
const AuthMethodSession = "session"

// IsUser reports whether p is a console user (session) rather than an API
// key or another machine identity.
func (p *Principal) IsUser() bool {
	return p != nil && p.AuthMethod == AuthMethodSession
}

// Authorizer decides whether a Principal holds a given permission.
// Permissions are dot-separated strings, e.g. "connector.create",
// "mcp.tools.call", "llm.messages.create".
type Authorizer interface {
	Allow(ctx context.Context, p *Principal, permission string) bool
}

// Sentinel errors returned by Authenticator.Authenticate.
var (
	// ErrNoCredential means the request carries no credential this
	// Authenticator recognizes (e.g. no Authorization header at all).
	// Callers should try the next Authenticator in the chain, if any.
	ErrNoCredential = errors.New("auth: no credential present")

	// ErrInvalid means the request carries a credential this
	// Authenticator recognizes, but it is malformed, expired, revoked,
	// or otherwise unusable. Callers should stop the chain and reject
	// the request rather than trying further Authenticators.
	ErrInvalid = errors.New("auth: invalid credential")

	// ErrRateLimited means the caller is temporarily locked out by an
	// auth-failure limiter. Authenticate wraps it in *RateLimitedError so
	// a plane can answer 429 + Retry-After in its own error format rather
	// than 401.
	ErrRateLimited = errors.New("auth: rate limited")
)

// RateLimitedError is the concrete error behind ErrRateLimited.
type RateLimitedError struct {
	// RetryAfter is how long until the lockout expires.
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return "auth: rate limited (retry after " + e.RetryAfter.Round(time.Second).String() + ")"
}

// Is makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitedError) Is(target error) bool { return target == ErrRateLimited }

type principalCtxKey struct{}

// WithPrincipal returns a context carrying p, retrievable via PrincipalFrom.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFrom returns the Principal stashed in ctx by WithPrincipal, if
// any. The second return value is false when no Principal is present.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(*Principal)
	if !ok || p == nil {
		return nil, false
	}
	return p, true
}

// RoleAuthorizer is the default Authorizer: a static map from role name to
// the permission-glob patterns that role grants, matched with Allow.
//
// The zero value is not usable; construct with NewRoleAuthorizer, which
// wires up the two built-in roles:
//
//	admin:  "*"                          -- everything
//	agent:  "mcp.*", "llm.*", "*.read"   -- MCP + LLM planes, and any read
//	viewer: "*.read"                     -- read-only console user
type RoleAuthorizer struct {
	// Rules maps a role name to the permission patterns it grants. A
	// pattern is a "."-separated list of segments; a segment of "*" in
	// the final position matches that point and everything after it
	// (e.g. "mcp.*" matches "mcp", "mcp.tools", "mcp.tools.call"), while
	// a segment of "*" elsewhere matches exactly one permission segment
	// (e.g. "*.read" matches "connector.read" but not
	// "connector.tools.read"). The single pattern "*" matches any
	// permission whatsoever.
	Rules map[string][]string

	// SingleTenant, when set, reports whether this deployment currently
	// has exactly one tenant -- the same test GET /auth/config's
	// single_tenant field uses. It backs the one dynamic carve-out Allow
	// applies on top of Rules: a console user's admin role grants
	// PermCatalogManage (managing the model catalog) only while this
	// returns true; in multi-tenant mode catalog writes need a
	// platform-admin key or user (see docs/security-model.md). A nil
	// SingleTenant treats every deployment as multi-tenant -- the grant
	// fails closed until main.go wires this up -- and never affects an
	// API-key principal or a platform-admin user, whose grants come from
	// their roles alone.
	SingleTenant func(ctx context.Context) bool
}

// NewRoleAuthorizer returns a RoleAuthorizer preloaded with the gateway's
// built-in roles (admin, platform-admin, agent, viewer, interceptor). Callers may add custom roles by
// mutating the returned Rules map before first use (see also
// config.Auth.Roles, which internal/config merges over these at startup).
//
// admin is a TENANT admin: "*" covers every tenant-scoped permission but,
// by matchPermission's carve-out, nothing in the "platform." namespace
// (tenant enumeration/creation, platform catalog writes, the model
// catalog). Those need the separate platform-admin role ("platform.*"),
// which an API key -- or a console user with store.UserRolePlatformAdmin
// -- holds in addition to admin (see RolesForKey). The first key `gateway
// bootstrap-key` ever creates, and keys made with `bootstrap-key
// --platform`, carry it; the HTTP API only lets a caller who already holds
// platform.admin mint such a key or create such a user, and `gateway
// create-user -role platform-admin` is the break-glass path.
//
// viewer is the read-only role for console users: every "*.read"
// permission and nothing else -- deliberately NOT "mcp.*"/"llm.*" (a
// human never calls the data planes) and not users.manage (user
// management and the auth audit are admin-only). It is also a valid role
// for an API key.
//
// A console user with role "admin" never holds platform.admin: only a
// console user whose roles include platform-admin does (the session
// authenticator expands store.UserRolePlatformAdmin with RolesForKey).
// Allow enforces that for session principals (see Allow).
//
// interceptor holds only "ingest.write": the one permission POST
// /api/v1/ingest is gated on (see internal/api/router.go). It
// deliberately grants nothing else -- not even "*.read" -- so a key
// minted for the capture component that feeds ingest (pkg/sink/sink.go's
// AccessLog/LLMCall.Source == "interceptor") can write ingest records
// and nothing more. agent's own "*.read" grant does not satisfy
// "ingest.write" either (a read grant never matches a write permission),
// so agent keys stay unable to call this route; admin's blanket "*"
// still covers it like everything else.
func NewRoleAuthorizer() *RoleAuthorizer {
	return &RoleAuthorizer{
		Rules: map[string][]string{
			"admin":           {"*"},
			RolePlatformAdmin: {"platform.*"},
			"agent":           {"mcp.*", "llm.*", "*.read"},
			"viewer":          {"*.read"},
			"interceptor":     {"ingest.write"},
		},
	}
}

// RoleGrantsPlatform reports whether role holds any permission in the
// "platform." namespace (the built-in platform-admin role, or a custom
// role configured with a "platform..." pattern). Creating an API key with
// such a role needs the caller to hold platform.admin itself, or a tenant
// admin could mint a platform key for themselves.
func (a *RoleAuthorizer) RoleGrantsPlatform(role string) bool {
	if a == nil || role == RolePlatformAdmin {
		return true // nil: fail closed
	}
	for _, pattern := range a.Rules[role] {
		if strings.SplitN(pattern, ".", 2)[0] == platformNamespace {
			return true
		}
	}
	return false
}

// Allow reports whether any role held by p grants permission, per Rules.
// A nil Principal, or a Principal with no matching role, is denied.
//
// Two carve-outs apply to console users (session principals) ahead of
// the role table:
//
//   - platform.admin is denied unless the user's roles include
//     platform-admin, whatever its other roles' patterns say. An "admin"
//     user gets admin's grants minus this one; a platform-admin user
//     falls through to its "platform.*" rule.
//   - platform.catalog.manage: a user WITHOUT platform-admin holds it
//     only while the deployment is single-tenant and the user is an
//     admin (see SingleTenant). A platform-admin user skips the carve-out
//     and holds it in every deployment through "platform.*".
func (a *RoleAuthorizer) Allow(ctx context.Context, p *Principal, permission string) bool {
	if p == nil || a == nil {
		return false
	}
	if p.IsUser() && !p.HasRole(RolePlatformAdmin) {
		switch permission {
		case PermPlatformAdmin:
			return false
		case PermCatalogManage:
			if a.SingleTenant == nil || !a.SingleTenant(ctx) {
				return false
			}
			return p.HasRole("admin")
		}
	}
	for _, role := range p.Roles {
		for _, pattern := range a.Rules[role] {
			if matchPermission(pattern, permission) {
				return true
			}
		}
	}
	return false
}

// RolePlatformAdmin is the built-in role that holds the "platform."
// permissions. Held by API keys with that stored role and by console
// users with store.UserRolePlatformAdmin (both expanded by RolesForKey);
// a console user with the plain "admin" role never gets it.
const RolePlatformAdmin = "platform-admin"

// RolesForKey returns the Principal roles for a stored role: an API key's
// api_keys.role or a console user's users.role. A platform-admin is also
// a tenant admin, so it expands to [admin, platform-admin]; every other
// role maps to itself.
func RolesForKey(role string) []string {
	if role == RolePlatformAdmin {
		return []string{"admin", RolePlatformAdmin}
	}
	return []string{role}
}

// Permission names used by more than one package.
const (
	// PermPlatformAdmin gates tenant enumeration/creation and the
	// acting-tenant header. Held by platform-admin API keys and
	// platform-admin console users only (see Allow).
	PermPlatformAdmin = "platform.admin"
	// PermUsersManage gates every /users route and the auth audit. Like
	// "admin.manage" it is matched only by the admin role's "*" grant: the
	// agent role's "*.read" and the viewer role's "*.read" do not match a
	// permission whose last segment is not "read".
	PermUsersManage = "users.manage"
	// PermCatalogManage gates every model-catalog write route (providers
	// and their models). Like PermPlatformAdmin it is in the "platform."
	// namespace (matchPermission's carve-out), so a bare "*"/"*.read"
	// grant never implies it; only platform-admin ("platform.*") holds it.
	// Unlike PermPlatformAdmin, a console user's admin role
	// CAN hold it -- but only in a single-tenant deployment (see
	// RoleAuthorizer.SingleTenant and Allow); a platform-admin console
	// user holds it everywhere.
	PermCatalogManage = "platform.catalog.manage"
)

// platformNamespace is the leading segment of every platform-scoped
// permission (e.g. "platform.admin"). See matchPermission.
const platformNamespace = "platform"

// matchPermission reports whether permission matches pattern under the
// glob rules documented on RoleAuthorizer.Rules.
//
// One carve-out sits ahead of the ordinary glob rules: a permission in
// the "platform." namespace can only be matched by a pattern that itself
// starts with the literal segment "platform" (e.g. "platform.*" or
// "platform.admin") -- a leading "*" wildcard, which would otherwise
// match everything via the trailing-wildcard rule below, never matches
// it. This keeps platform-scoped permissions (tenant enumeration today;
// more later) from being satisfied by a broad "*" or "*.read" grant that
// was never meant to imply platform-admin access; see
// NewRoleAuthorizer's platform-admin role for how a key still gets it.
func matchPermission(pattern, permission string) bool {
	patternSegs := strings.Split(pattern, ".")
	permSegs := strings.Split(permission, ".")

	if permSegs[0] == platformNamespace && patternSegs[0] != platformNamespace {
		return false
	}

	for i, ps := range patternSegs {
		if ps == "*" && i == len(patternSegs)-1 {
			// Trailing wildcard: matches everything from here on,
			// including nothing more (an exact prefix match).
			return true
		}
		if i >= len(permSegs) {
			return false
		}
		if ps != "*" && ps != permSegs[i] {
			return false
		}
	}
	return len(patternSegs) == len(permSegs)
}
