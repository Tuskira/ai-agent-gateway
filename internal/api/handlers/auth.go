package handlers

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/password"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/session"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Auth implements the console login surface: GET /auth/config, POST
// /auth/login, POST /auth/logout, GET /auth/me and POST /auth/password.
// (User management lives in Users.)
//
// Login is public and sits behind the control plane's per-IP lockout
// (LoginThrottle); on top of that, five consecutive wrong passwords lock
// the one account for fifteen minutes. A failed login always looks the
// same to the caller -- 401 "invalid username or password" -- whichever
// part was wrong and whether the account is unknown, disabled or locked;
// the real reason goes to the audit trail only.
type Auth struct{ Deps }

const (
	// errAuthentication is the error type of a rejected login.
	errAuthentication = "authentication_error"
	// msgInvalidLogin is the one message every failed login gets.
	msgInvalidLogin = "invalid username or password"

	// Per-user lockout: maxUserFailures consecutive failures lock the
	// account for userLockout.
	maxUserFailures = 5
	userLockout     = 15 * time.Minute

	// maxLoginPasswordBytes bounds what login will hash; anything longer
	// cannot be a valid password (the policy maximum is 128 characters).
	maxLoginPasswordBytes = 1024
)

func (h Auth) idle() time.Duration {
	if h.Console.SessionIdle > 0 {
		return h.Console.SessionIdle
	}
	return session.DefaultIdle
}

func (h Auth) max() time.Duration {
	if h.Console.SessionMax > 0 {
		return h.Console.SessionMax
	}
	return session.DefaultMax
}

// clientIP is the caller's IP as the rate limiter sees it (honouring
// trusted proxies), or the raw peer address without one.
func (d Deps) clientIP(r *http.Request) string {
	if d.LoginThrottle != nil {
		return d.LoginThrottle.ClientIP(r)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// actor describes who performed an audited action: the logged-in user, or
// the API key for an API-key call.
func actor(r *http.Request) (kind, id string) {
	p, ok := pkgauth.PrincipalFrom(r.Context())
	if !ok || p == nil {
		return "anonymous", ""
	}
	if p.IsUser() {
		return "user", p.Subject
	}
	if p.KeyID != "" {
		return "api_key", p.KeyID
	}
	return "api_key", p.Subject
}

// audit appends one auth_audit row. It is best-effort: a failure is logged
// but never fails the request it describes. ActorTenantID is filled in
// when the actor's home tenant (Principal.Home) is not the tenant the row
// belongs to -- a platform-admin acting in another tenant -- so that
// tenant's trail shows the actor came from elsewhere.
func (d Deps) audit(r *http.Request, e store.AuthAuditEntry) {
	if d.Store == nil {
		return
	}
	if e.ActorKind == "" {
		e.ActorKind, e.ActorID = actor(r)
	}
	if e.ActorTenantID == "" {
		if p, ok := pkgauth.PrincipalFrom(r.Context()); ok {
			if home := p.Home(); home != "" && home != e.TenantID {
				e.ActorTenantID = home
			}
		}
	}
	e.IP = d.clientIP(r)
	if err := d.Store.AuthAudit().Append(r.Context(), &e); err != nil {
		applog.From(r.Context()).Error("auth audit write failed", "action", e.Action, "error", err)
	}
}

// ---------------------------------------------------------------------------
// GET /auth/config
// ---------------------------------------------------------------------------

type authConfigView struct {
	PasswordLogin bool   `json:"password_login"`
	APIKeyLogin   bool   `json:"api_key_login"`
	SingleTenant  bool   `json:"single_tenant"`
	DefaultTenant string `json:"default_tenant"`
	// HasUsers is whether the default tenant has at least one active
	// (live, not disabled) console user. Only computed on a single-tenant
	// install; with several tenants it is always true, so the public
	// endpoint never reveals whether a given tenant has users.
	HasUsers bool `json:"has_users"`
	// TenantOptional tells the console that POST /auth/login accepts a
	// body without "tenant": the server then finds the account by
	// username across tenants (see Login). Always true; the field exists
	// so a console built for this server can hide the tenant field and
	// an older server (which lacks it) keeps showing it.
	TenantOptional bool `json:"tenant_optional"`
}

// defaultTenant resolves the tenant the login form should prefill: the
// only tenant when exactly one exists, otherwise the configured default
// slug. tenantID is set only in the single-tenant case. Login itself no
// longer needs it (a body without a tenant is resolved by username).
func (h Auth) defaultTenant(ctx context.Context) (slug, tenantID string, single bool) {
	if h.Store != nil {
		if tenants, err := h.Store.Tenants().List(ctx); err == nil && len(tenants) == 1 {
			return tenants[0].Slug, tenants[0].ID, true
		}
	}
	return h.Console.DefaultTenant, "", false
}

// hasActiveUsers reports whether the tenant has a live, non-disabled
// console user. A read failure answers true: the first-run notice must
// never show on a transient error.
func (h Auth) hasActiveUsers(ctx context.Context, tenantID string) bool {
	users, err := h.Store.Users().List(ctx, tenantID, store.UserListOptions{})
	if err != nil {
		applog.From(ctx).Error("auth config: list users failed", "error", err)
		return true
	}
	for _, u := range users {
		if !u.Disabled {
			return true
		}
	}
	return false
}

// Config handles GET /api/v1/auth/config (public): what the login page
// should offer.
func (h Auth) Config(w http.ResponseWriter, r *http.Request) {
	slug, tenantID, single := h.defaultTenant(r.Context())
	hasUsers := true
	if single {
		hasUsers = h.hasActiveUsers(r.Context(), tenantID)
	}
	httpx.WriteJSON(w, http.StatusOK, authConfigView{
		PasswordLogin:  true,
		APIKeyLogin:    h.Console.APIKeyLogin,
		SingleTenant:   single,
		DefaultTenant:  slug,
		HasUsers:       hasUsers,
		TenantOptional: true,
	})
}

// ---------------------------------------------------------------------------
// POST /auth/login
// ---------------------------------------------------------------------------

type loginRequest struct {
	// Tenant is the tenant slug. Optional: when empty the username is
	// looked up across every tenant (see Login).
	Tenant   string `json:"tenant"`
	Username string `json:"username"`
	Password string `json:"password"`
}

const (
	// typeTenantRequired is the error type of the 409 a tenant-less login
	// gets when the username exists in more than one tenant.
	typeTenantRequired = "tenant_required"
	// msgTenantRequired names no tenant: the body must not list where the
	// username exists.
	msgTenantRequired = "choose a tenant"
)

// sessionUserView is the user object of the login and /auth/me responses.
type sessionUserView struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Tenant      string `json:"tenant"`
}

type loginResponse struct {
	User               sessionUserView `json:"user"`
	MustChangePassword bool            `json:"must_change_password"`
	CSRFToken          string          `json:"csrf_token"`
}

// Login handles POST /api/v1/auth/login (public).
//
// The body's tenant is optional. With one, the account is (tenant,
// username) as before. Without one, the username is looked up across all
// tenants: exactly one live account logs in as if its tenant had been
// named; none is the same generic 401 as a wrong password (after the same
// dummy hash work, so timing does not tell the cases apart); more than one
// is 409 tenant_required with a body that names no tenant, and the caller
// retries with the tenant set. That 409 does say the username exists in
// several tenants -- the accepted trade-off for not having to type a
// tenant on every login.
func (h Auth) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	username := store.NormalizeUsername(req.Username)
	if username == "" || req.Password == "" {
		httpx.ValidationError(w, "username and password are required")
		return
	}
	ctx := r.Context()

	fail := func(tenantID, targetUserID, reason string) {
		if h.LoginThrottle != nil {
			h.LoginThrottle.ReportFailure(h.clientIP(r))
		}
		h.audit(r, store.AuthAuditEntry{
			TenantID: tenantID, ActorKind: "anonymous", ActorID: truncate(username, 80),
			Action: "login_fail", TargetUserID: targetUserID, Detail: reason,
		})
		httpx.WriteError(w, http.StatusUnauthorized, errAuthentication, msgInvalidLogin)
	}

	if len(req.Password) > maxLoginPasswordBytes {
		fail("", "", "password_too_long")
		return
	}

	var (
		tenant *store.Tenant
		user   *store.User
	)
	if slug := strings.TrimSpace(req.Tenant); slug != "" {
		var err error
		tenant, err = h.Store.Tenants().GetBySlug(ctx, slug)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				applog.From(ctx).Error("login: resolve tenant failed", "error", err)
				httpx.Internal(w, "internal error")
				return
			}
			password.DummyVerify(req.Password)
			fail("", "", "unknown_tenant")
			return
		}
		user, err = h.Store.Users().GetByUsername(ctx, tenant.ID, username)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				applog.From(ctx).Error("login: load user failed", "error", err)
				httpx.Internal(w, "internal error")
				return
			}
			password.DummyVerify(req.Password)
			fail(tenant.ID, "", "unknown_user")
			return
		}
	} else {
		matches, err := h.Store.Users().FindByUsername(ctx, username)
		if err != nil {
			applog.From(ctx).Error("login: find user failed", "error", err)
			httpx.Internal(w, "internal error")
			return
		}
		switch len(matches) {
		case 0:
			password.DummyVerify(req.Password)
			fail("", "", "unknown_user")
			return
		case 1:
			user = matches[0]
		default:
			httpx.WriteError(w, http.StatusConflict, typeTenantRequired, msgTenantRequired)
			return
		}
		tenant, err = h.Store.Tenants().GetByID(ctx, user.TenantID)
		if err != nil {
			// A live user always has a tenant; this is a broken store, not a
			// caller error.
			applog.From(ctx).Error("login: load user's tenant failed", "user_id", user.ID, "error", err)
			httpx.Internal(w, "internal error")
			return
		}
	}

	now := time.Now()
	if user.Disabled || (user.LockedUntil != nil && now.Before(*user.LockedUntil)) {
		// Same work as a real attempt, same answer; a locked account's
		// counter is left alone so guessing cannot extend the lock.
		password.DummyVerify(req.Password)
		reason := "user_disabled"
		if !user.Disabled {
			reason = "user_locked"
		}
		fail(tenant.ID, user.ID, reason)
		return
	}

	ok, err := password.Verify(req.Password, user.PasswordHash)
	if err != nil {
		applog.From(ctx).Error("login: stored password hash unusable", "user_id", user.ID, "error", err)
		fail(tenant.ID, user.ID, "bad_stored_hash")
		return
	}
	if !ok {
		if err := h.Store.Users().RecordLoginFailure(ctx, tenant.ID, user.ID, maxUserFailures, userLockout, now); err != nil {
			applog.From(ctx).Error("login: record failure failed", "user_id", user.ID, "error", err)
		}
		fail(tenant.ID, user.ID, "bad_password")
		return
	}

	sess, raw, err := h.newSession(r, user)
	if err != nil {
		applog.From(ctx).Error("login: create session failed", "user_id", user.ID, "error", err)
		httpx.Internal(w, "internal error")
		return
	}
	if err := h.Store.Users().RecordLoginSuccess(ctx, tenant.ID, user.ID, now); err != nil {
		applog.From(ctx).Error("login: record success failed", "user_id", user.ID, "error", err)
	}
	if h.LoginThrottle != nil {
		h.LoginThrottle.ReportSuccess(h.clientIP(r))
	}
	h.audit(r, store.AuthAuditEntry{
		TenantID: tenant.ID, ActorKind: "user", ActorID: user.ID, Action: "login_ok", TargetUserID: user.ID,
	})

	session.SetCookie(w, r, raw, h.Console.CookieSecure, h.max())
	httpx.WriteJSON(w, http.StatusOK, loginResponse{
		User:               newSessionUserView(user, tenant.Slug),
		MustChangePassword: user.MustChangePassword,
		CSRFToken:          sess.CSRFToken,
	})
}

func (h Auth) newSession(r *http.Request, user *store.User) (*store.UserSession, string, error) {
	raw, id, err := session.NewToken()
	if err != nil {
		return nil, "", err
	}
	csrf, err := session.NewCSRFToken()
	if err != nil {
		return nil, "", err
	}
	sess := &store.UserSession{
		ID: id, UserID: user.ID, TenantID: user.TenantID, CSRFToken: csrf,
		ExpiresAt: time.Now().Add(h.max()),
		UserAgent: truncate(r.UserAgent(), 255), IP: h.clientIP(r),
	}
	if err := h.Store.UserSessions().Create(r.Context(), sess); err != nil {
		return nil, "", err
	}
	return sess, raw, nil
}

func newSessionUserView(u *store.User, tenantSlug string) sessionUserView {
	return sessionUserView{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role, Tenant: tenantSlug}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---------------------------------------------------------------------------
// POST /auth/logout
// ---------------------------------------------------------------------------

// Logout handles POST /api/v1/auth/logout: revokes the calling session
// and clears the cookie. For a non-session caller (an API key) there is no
// session to end and it is a no-op 204. The audit row goes to the user's
// home tenant, whatever tenant it was acting in.
func (h Auth) Logout(w http.ResponseWriter, r *http.Request) {
	p, ok := pkgauth.PrincipalFrom(r.Context())
	if ok && p.IsUser() && p.SessionID != "" {
		if err := h.Store.UserSessions().Revoke(r.Context(), p.SessionID); err != nil && !errors.Is(err, store.ErrNotFound) {
			applog.From(r.Context()).Error("logout: revoke session failed", "error", err)
			httpx.Internal(w, "internal error")
			return
		}
		h.audit(r, store.AuthAuditEntry{TenantID: p.Home(), Action: "logout", TargetUserID: p.Subject})
	}
	session.ClearCookie(w, r, h.Console.CookieSecure)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// GET /auth/me
// ---------------------------------------------------------------------------

// meView is the JSON shape of GET /api/v1/auth/me. The first group
// mirrors pkgauth.Principal (minus RawCredential, which must never be
// echoed): TenantID is the tenant the caller acts in (the acting tenant
// while a platform-admin sends X-Gateway-Acting-Tenant) and HomeTenantID
// the one it belongs to (Principal.Home; equal to TenantID otherwise).
// The rest is new: Kind says whether the caller is a logged-in console
// user or an API key, and for a user the account (always the home
// account), whether it must change its password, and the CSRF token to
// echo on writes.
type meView struct {
	Subject      string   `json:"subject"`
	TenantID     string   `json:"tenant_id"`
	HomeTenantID string   `json:"home_tenant_id"`
	Email        string   `json:"email,omitempty"`
	Roles        []string `json:"roles"`
	AuthMethod   string   `json:"auth_method"`
	KeyID        string   `json:"key_id,omitempty"`

	Kind               string           `json:"kind"` // "user" | "api_key"
	User               *sessionUserView `json:"user,omitempty"`
	MustChangePassword *bool            `json:"must_change_password,omitempty"`
	CSRFToken          string           `json:"csrf_token,omitempty"`
}

// Me handles GET /api/v1/auth/me.
func (h Auth) Me(w http.ResponseWriter, r *http.Request) {
	p, ok := pkgauth.PrincipalFrom(r.Context())
	if !ok || p == nil {
		// Unreachable in practice: this route only runs behind
		// internalauth.Middleware, which always attaches a Principal.
		httpx.Internal(w, "no principal in request context")
		return
	}

	view := meView{
		Subject: p.Subject, TenantID: p.TenantID, HomeTenantID: p.Home(), Email: p.Email, Roles: p.Roles,
		AuthMethod: p.AuthMethod, KeyID: p.KeyID, Kind: "api_key",
	}
	if p.IsUser() {
		user, err := h.Store.Users().Get(r.Context(), p.Home(), p.Subject)
		if err != nil {
			writeStoreErr(w, r, "auth me: load user", err)
			return
		}
		slug, err := h.tenantSlug(r.Context(), p.Home())
		if err != nil {
			writeStoreErr(w, r, "auth me: load tenant", err)
			return
		}
		uv := newSessionUserView(user, slug)
		mc := p.MustChangePassword
		view.Kind, view.User, view.MustChangePassword, view.CSRFToken = "user", &uv, &mc, p.CSRFToken
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// tenantSlug maps a tenant id to its slug (store.ErrNotFound when there is
// no such tenant).
func (d Deps) tenantSlug(ctx context.Context, tenantID string) (string, error) {
	t, err := d.Store.Tenants().GetByID(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return t.Slug, nil
}

// ---------------------------------------------------------------------------
// POST /auth/password
// ---------------------------------------------------------------------------

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePassword handles POST /api/v1/auth/password: a logged-in user
// changes their own password. The current password must be right (a wrong
// one counts toward the account's login lockout, since a stolen session
// must not become an unlimited password oracle), the new one must satisfy
// the policy and differ from the current one. Every OTHER session of the
// user is revoked and must_change_password is cleared. The account is
// always the caller's own, in its home tenant: an acting-tenant override
// never redirects a password change.
func (h Auth) ChangePassword(w http.ResponseWriter, r *http.Request) {
	p, ok := pkgauth.PrincipalFrom(r.Context())
	if !ok || p == nil || !p.IsUser() {
		httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission, "changing a password requires a console user session")
		return
	}
	var req changePasswordRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if req.CurrentPassword == "" || req.NewPassword == "" {
		httpx.ValidationError(w, "current_password and new_password are required")
		return
	}
	ctx := r.Context()

	user, err := h.Store.Users().Get(ctx, p.Home(), p.Subject)
	if err != nil {
		writeStoreErr(w, r, "change password: load user", err)
		return
	}

	locked := user.LockedUntil != nil && time.Now().Before(*user.LockedUntil)
	matches := false
	if locked || len(req.CurrentPassword) > maxLoginPasswordBytes {
		password.DummyVerify(req.CurrentPassword)
	} else {
		matches, err = password.Verify(req.CurrentPassword, user.PasswordHash)
		if err != nil {
			applog.From(ctx).Error("change password: stored hash unusable", "user_id", user.ID, "error", err)
			httpx.Internal(w, "internal error")
			return
		}
	}
	if !matches {
		if !locked {
			if err := h.Store.Users().RecordLoginFailure(ctx, user.TenantID, user.ID, maxUserFailures, userLockout, time.Now()); err != nil {
				applog.From(ctx).Error("change password: record failure failed", "user_id", user.ID, "error", err)
			}
		}
		httpx.ValidationError(w, "current password is incorrect")
		return
	}

	if err := password.ValidatePolicy(req.NewPassword, user.Username); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if req.NewPassword == req.CurrentPassword {
		httpx.ValidationError(w, "new password must be different from the current password")
		return
	}

	hash, err := password.Hash(req.NewPassword)
	if err != nil {
		applog.From(ctx).Error("change password: hash failed", "error", err)
		httpx.Internal(w, "internal error")
		return
	}
	if err := h.Store.Users().SetPassword(ctx, user.TenantID, user.ID, hash, false); err != nil {
		writeStoreErr(w, r, "change password: store", err)
		return
	}
	if _, err := h.Store.UserSessions().RevokeAllForUser(ctx, user.ID, p.SessionID); err != nil {
		applog.From(ctx).Error("change password: revoke other sessions failed", "user_id", user.ID, "error", err)
		httpx.Internal(w, "internal error")
		return
	}
	h.audit(r, store.AuthAuditEntry{TenantID: user.TenantID, Action: "password_change", TargetUserID: user.ID})
	w.WriteHeader(http.StatusNoContent)
}
