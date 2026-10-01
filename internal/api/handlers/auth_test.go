package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/password"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/session"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

var (
	hashMu    sync.Mutex
	hashCache = map[string]string{}
)

// hashOf hashes pw once per test binary (argon2id at production cost is
// deliberately slow; the tests reuse the result).
func hashOf(t *testing.T, pw string) string {
	t.Helper()
	hashMu.Lock()
	defer hashMu.Unlock()
	if h, ok := hashCache[pw]; ok {
		return h
	}
	h, err := password.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	hashCache[pw] = h
	return h
}

type authEnv struct {
	deps    Deps
	fs      *fakeStore
	tenantA *store.Tenant
	tenantB *store.Tenant
}

const (
	pwAlice = "alice-password-123"
	pwBob   = "bob-password-4567"
)

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	deps := newTestDeps()
	deps.Console = ConsoleConfig{APIKeyLogin: true, DefaultTenant: "acme", CookieSecure: "auto"}
	fs := deps.Store.(*fakeStore)
	env := &authEnv{deps: deps, fs: fs}
	env.tenantA = &store.Tenant{Slug: "acme", Name: "Acme"}
	env.tenantB = &store.Tenant{Slug: "globex", Name: "Globex"}
	for _, tn := range []*store.Tenant{env.tenantA, env.tenantB} {
		if err := fs.Tenants().Create(context.Background(), tn); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

func (e *authEnv) user(t *testing.T, tenantID, username, role, pw string, mustChange bool) *store.User {
	t.Helper()
	u := &store.User{
		TenantID: tenantID, Username: username, DisplayName: strings.ToUpper(username[:1]) + username[1:],
		PasswordHash: hashOf(t, pw), Role: role, MustChangePassword: mustChange,
	}
	if err := e.fs.Users().Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func (e *authEnv) session(t *testing.T, u *store.User) (raw string, s *store.UserSession) {
	t.Helper()
	raw, id, _ := session.NewToken()
	s = &store.UserSession{ID: id, UserID: u.ID, TenantID: u.TenantID, CSRFToken: "csrf-" + id[:8], ExpiresAt: time.Now().Add(time.Hour)}
	if err := e.fs.UserSessions().Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return raw, s
}

// asUser attaches the Principal the session Authenticator would produce.
func asUser(r *http.Request, u *store.User, s *store.UserSession) *http.Request {
	p := &pkgauth.Principal{
		Subject: u.ID, TenantID: u.TenantID, Roles: []string{u.Role}, AuthMethod: pkgauth.AuthMethodSession,
		Username: u.Username, MustChangePassword: u.MustChangePassword,
	}
	if s != nil {
		p.SessionID, p.CSRFToken = s.ID, s.CSRFToken
	}
	return r.WithContext(pkgauth.WithPrincipal(r.Context(), p))
}

// asAPIKey attaches an API-key Principal.
func asAPIKey(r *http.Request, tenantID, role string) *http.Request {
	p := &pkgauth.Principal{Subject: "key-1", TenantID: tenantID, Roles: []string{role}, AuthMethod: "apikey", KeyID: "key-1"}
	return r.WithContext(pkgauth.WithPrincipal(r.Context(), p))
}

func jsonReq(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func decodeInto(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
}

type throttleSpy struct {
	mu                sync.Mutex
	failures, success int
}

func (s *throttleSpy) ClientIP(*http.Request) string { return "198.51.100.23" }
func (s *throttleSpy) ReportFailure(string)          { s.mu.Lock(); s.failures++; s.mu.Unlock() }
func (s *throttleSpy) ReportSuccess(string)          { s.mu.Lock(); s.success++; s.mu.Unlock() }

func (e *authEnv) audit(t *testing.T, tenantID string) []*store.AuthAuditEntry {
	t.Helper()
	got, _, err := e.fs.AuthAudit().List(context.Background(), tenantID, store.AuthAuditListOptions{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func hasAudit(entries []*store.AuthAuditEntry, action string) *store.AuthAuditEntry {
	for _, e := range entries {
		if e.Action == action {
			return e
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// GET /auth/config
// ---------------------------------------------------------------------------

func TestAuthConfig_SingleTenant(t *testing.T) {
	deps := newTestDeps()
	deps.Console = ConsoleConfig{APIKeyLogin: true, DefaultTenant: "ignored"}
	_ = deps.Store.Tenants().Create(context.Background(), &store.Tenant{Slug: "only", Name: "Only"})
	h := Auth{Deps: deps}

	w := serve(http.MethodGet, "/auth/config", h.Config, httptest.NewRequest(http.MethodGet, "/auth/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got map[string]any
	decodeInto(t, w, &got)
	want := map[string]any{"password_login": true, "api_key_login": true, "single_tenant": true, "default_tenant": "only", "has_users": false}
	if len(got) != len(want) {
		t.Errorf("config = %v, want exactly the keys %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("config[%s] = %v, want %v", k, got[k], v)
		}
	}
}

func TestAuthConfig_HasUsers(t *testing.T) {
	cfg := func(t *testing.T, h Auth) authConfigView {
		t.Helper()
		w := serve(http.MethodGet, "/auth/config", h.Config, httptest.NewRequest(http.MethodGet, "/auth/config", nil))
		var got authConfigView
		decodeInto(t, w, &got)
		return got
	}
	newSingle := func(t *testing.T) (*authEnv, *store.Tenant) {
		env := newAuthEnv(t)
		fs := env.fs
		only := &store.Tenant{Slug: "solo", Name: "Solo"}
		// Replace the two fixture tenants with one.
		env.deps = newTestDeps()
		env.deps.Console = ConsoleConfig{APIKeyLogin: true, DefaultTenant: "solo"}
		env.fs = env.deps.Store.(*fakeStore)
		_ = fs
		if err := env.fs.Tenants().Create(context.Background(), only); err != nil {
			t.Fatal(err)
		}
		return env, only
	}

	t.Run("single tenant without users", func(t *testing.T) {
		env, _ := newSingle(t)
		if got := cfg(t, Auth{Deps: env.deps}); !got.SingleTenant || got.HasUsers {
			t.Errorf("config = %+v, want single tenant, has_users=false", got)
		}
	})
	t.Run("single tenant with an active user", func(t *testing.T) {
		env, tn := newSingle(t)
		env.user(t, tn.ID, "alice", store.UserRoleAdmin, pwAlice, false)
		if got := cfg(t, Auth{Deps: env.deps}); !got.HasUsers {
			t.Errorf("config = %+v, want has_users=true", got)
		}
	})
	t.Run("a disabled user counts as none", func(t *testing.T) {
		env, tn := newSingle(t)
		u := env.user(t, tn.ID, "alice", store.UserRoleAdmin, pwAlice, false)
		u.Disabled = true
		if err := env.fs.Users().Update(context.Background(), u); err != nil {
			t.Fatal(err)
		}
		if got := cfg(t, Auth{Deps: env.deps}); got.HasUsers {
			t.Errorf("config = %+v, want has_users=false", got)
		}
	})
	t.Run("multi tenant is always true", func(t *testing.T) {
		env := newAuthEnv(t) // two tenants, no users
		if got := cfg(t, Auth{Deps: env.deps}); got.SingleTenant || !got.HasUsers {
			t.Errorf("config = %+v, want multi tenant, has_users=true", got)
		}
	})
}

func TestAuthConfig_MultiTenantUsesConfiguredDefault(t *testing.T) {
	env := newAuthEnv(t)
	env.deps.Console.APIKeyLogin = false
	h := Auth{Deps: env.deps}

	w := serve(http.MethodGet, "/auth/config", h.Config, httptest.NewRequest(http.MethodGet, "/auth/config", nil))
	var got authConfigView
	decodeInto(t, w, &got)
	if !got.PasswordLogin || got.APIKeyLogin || got.SingleTenant || got.DefaultTenant != "acme" {
		t.Errorf("config = %+v, want password only, multi-tenant, default acme", got)
	}
}

// ---------------------------------------------------------------------------
// POST /auth/login
// ---------------------------------------------------------------------------

func login(h Auth, body string) *httptest.ResponseRecorder {
	return serve(http.MethodPost, "/auth/login", h.Login, jsonReq(http.MethodPost, "/auth/login", body))
}

func TestLogin_Success(t *testing.T) {
	env := newAuthEnv(t)
	spy := &throttleSpy{}
	env.deps.LoginThrottle = spy
	alice := env.user(t, env.tenantA.ID, "alice", store.UserRoleAdmin, pwAlice, false)
	h := Auth{Deps: env.deps}

	w := login(h, `{"tenant":"acme","username":"  Alice ","password":"`+pwAlice+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got loginResponse
	decodeInto(t, w, &got)
	if got.User.ID != alice.ID || got.User.Username != "alice" || got.User.DisplayName != "Alice" ||
		got.User.Role != "admin" || got.User.Tenant != "acme" || got.MustChangePassword || got.CSRFToken == "" {
		t.Errorf("response = %+v", got)
	}
	// Exact JSON shape the console relies on.
	var raw map[string]json.RawMessage
	decodeInto(t, w, &raw)
	if len(raw) != 3 || raw["user"] == nil || raw["must_change_password"] == nil || raw["csrf_token"] == nil {
		t.Errorf("login keys = %v, want user, must_change_password, csrf_token", raw)
	}

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != "gw_session" || c.Value == "" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge != 24*3600 {
		t.Errorf("cookie = %+v", c)
	}
	if c.Secure {
		t.Error("cookie is Secure on a plain-HTTP request under cookie_secure=auto")
	}

	// The store holds the hash of the cookie value, never the value, plus the csrf token returned.
	sess, err := env.fs.UserSessions().Get(context.Background(), session.HashToken(c.Value))
	if err != nil {
		t.Fatalf("session not stored under sha256(cookie): %v", err)
	}
	if sess.CSRFToken != got.CSRFToken || sess.UserID != alice.ID || sess.TenantID != env.tenantA.ID {
		t.Errorf("stored session = %+v", sess)
	}
	if d := time.Until(sess.ExpiresAt); d < 23*time.Hour || d > 24*time.Hour {
		t.Errorf("session expires in %s, want about 24h", d)
	}
	if u, _ := env.fs.Users().Get(context.Background(), env.tenantA.ID, alice.ID); u.LastLoginAt == nil {
		t.Error("last_login_at not recorded")
	}
	if e := hasAudit(env.audit(t, env.tenantA.ID), "login_ok"); e == nil || e.ActorKind != "user" || e.ActorID != alice.ID || e.IP != "198.51.100.23" {
		t.Errorf("login_ok audit = %+v", e)
	}
	if spy.success != 1 || spy.failures != 0 {
		t.Errorf("throttle saw %d successes, %d failures; want 1, 0", spy.success, spy.failures)
	}
}

func TestLogin_SecureCookieOverForwardedHTTPS(t *testing.T) {
	env := newAuthEnv(t)
	env.user(t, env.tenantA.ID, "alice", store.UserRoleAdmin, pwAlice, false)
	h := Auth{Deps: env.deps}

	r := jsonReq(http.MethodPost, "/auth/login", `{"tenant":"acme","username":"alice","password":"`+pwAlice+`"}`)
	r.Header.Set("X-Forwarded-Proto", "https")
	w := serve(http.MethodPost, "/auth/login", h.Login, r)
	if c := w.Result().Cookies(); len(c) != 1 || !c[0].Secure {
		t.Errorf("cookies = %+v, want one Secure cookie", c)
	}
}

func TestLogin_MustChangePasswordFlagIsReturned(t *testing.T) {
	env := newAuthEnv(t)
	env.user(t, env.tenantA.ID, "newbie", store.UserRoleViewer, pwBob, true)
	w := login(Auth{Deps: env.deps}, `{"tenant":"acme","username":"newbie","password":"`+pwBob+`"}`)
	var got loginResponse
	decodeInto(t, w, &got)
	if w.Code != http.StatusOK || !got.MustChangePassword {
		t.Errorf("status %d, response %+v; want 200 with must_change_password", w.Code, got)
	}
}

func TestLogin_FailuresAreIndistinguishable(t *testing.T) {
	env := newAuthEnv(t)
	spy := &throttleSpy{}
	env.deps.LoginThrottle = spy
	env.user(t, env.tenantA.ID, "alice", store.UserRoleAdmin, pwAlice, false)
	disabled := env.user(t, env.tenantA.ID, "dora", store.UserRoleViewer, pwBob, false)
	disabled.Disabled = true
	_ = env.fs.Users().Update(context.Background(), disabled)
	locked := env.user(t, env.tenantA.ID, "lena", store.UserRoleViewer, pwBob, false)
	for i := 0; i < maxUserFailures; i++ {
		_ = env.fs.Users().RecordLoginFailure(context.Background(), env.tenantA.ID, locked.ID, maxUserFailures, userLockout, time.Now())
	}
	env.user(t, env.tenantB.ID, "gary", store.UserRoleViewer, pwBob, false) // exists, but in another tenant
	h := Auth{Deps: env.deps}

	cases := map[string]string{
		"wrong password":          `{"tenant":"acme","username":"alice","password":"not-the-password"}`,
		"unknown user":            `{"tenant":"acme","username":"nobody","password":"` + pwAlice + `"}`,
		"unknown tenant":          `{"tenant":"initech","username":"alice","password":"` + pwAlice + `"}`,
		"user of another tenant":  `{"tenant":"acme","username":"gary","password":"` + pwBob + `"}`,
		"disabled user, right pw": `{"tenant":"acme","username":"dora","password":"` + pwBob + `"}`,
		"locked user, right pw":   `{"tenant":"acme","username":"lena","password":"` + pwBob + `"}`,
		"absurdly long password":  `{"tenant":"acme","username":"alice","password":"` + strings.Repeat("x", 2000) + `"}`,
	}
	var first string
	for name, body := range cases {
		w := login(h, body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401, body = %s", name, w.Code, w.Body.String())
			continue
		}
		assertErrorType(t, w, "authentication_error")
		if first == "" {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Errorf("%s: body %q differs from the others %q -- a failure must not reveal what was wrong", name, w.Body.String(), first)
		}
		if strings.Contains(w.Body.String(), "lock") || strings.Contains(w.Body.String(), "disabled") {
			t.Errorf("%s: body leaks account state: %s", name, w.Body.String())
		}
		if len(w.Result().Cookies()) != 0 {
			t.Errorf("%s: a failed login set a cookie", name)
		}
	}
	if !strings.Contains(first, "invalid username or password") {
		t.Errorf("failure body = %s, want the generic message", first)
	}
	if spy.failures != len(cases) || spy.success != 0 {
		t.Errorf("throttle saw %d failures, %d successes; want %d, 0", spy.failures, spy.success, len(cases))
	}
	// Reasons are for admins, recorded in the audit trail.
	reasons := map[string]bool{}
	for _, e := range env.audit(t, env.tenantA.ID) {
		if e.Action == "login_fail" {
			reasons[e.Detail] = true
		}
	}
	for _, want := range []string{"bad_password", "unknown_user", "user_disabled", "user_locked"} {
		if !reasons[want] {
			t.Errorf("no login_fail audit row with reason %q (have %v)", want, reasons)
		}
	}
}

func TestLogin_PerUserLockoutAfterFiveFailures(t *testing.T) {
	env := newAuthEnv(t)
	alice := env.user(t, env.tenantA.ID, "alice", store.UserRoleAdmin, pwAlice, false)
	h := Auth{Deps: env.deps}
	bad := `{"tenant":"acme","username":"alice","password":"wrong-wrong-wrong"}`
	good := `{"tenant":"acme","username":"alice","password":"` + pwAlice + `"}`

	for i := 1; i < maxUserFailures; i++ {
		if w := login(h, bad); w.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d", i, w.Code)
		}
	}
	u, _ := env.fs.Users().Get(context.Background(), env.tenantA.ID, alice.ID)
	if u.LockedUntil != nil || u.FailedLogins != maxUserFailures-1 {
		t.Fatalf("after %d failures: locked_until=%v failed_logins=%d; want unlocked", maxUserFailures-1, u.LockedUntil, u.FailedLogins)
	}
	// A success resets the streak: failures must be consecutive.
	if w := login(h, good); w.Code != http.StatusOK {
		t.Fatalf("login with the right password before the threshold: %d", w.Code)
	}
	for i := 1; i <= maxUserFailures; i++ {
		login(h, bad)
	}
	u, _ = env.fs.Users().Get(context.Background(), env.tenantA.ID, alice.ID)
	if u.LockedUntil == nil || time.Until(*u.LockedUntil) < 14*time.Minute || time.Until(*u.LockedUntil) > 16*time.Minute {
		t.Fatalf("locked_until = %v, want about 15 minutes out", u.LockedUntil)
	}
	// Now even the right password is refused, identically.
	if w := login(h, good); w.Code != http.StatusUnauthorized {
		t.Errorf("right password while locked: status = %d, want 401", w.Code)
	}
}

func TestLogin_Validation(t *testing.T) {
	env := newAuthEnv(t)
	h := Auth{Deps: env.deps}
	for name, body := range map[string]string{
		"empty body":       `{}`,
		"missing password": `{"tenant":"acme","username":"alice"}`,
		"missing username": `{"tenant":"acme","password":"x"}`,
		"not json":         `nope`,
		"unknown field":    `{"tenant":"acme","username":"a","password":"b","remember":true}`,
	} {
		w := login(h, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, w.Code)
		}
	}
}

func TestLogin_TenantDefaults(t *testing.T) {
	// One tenant: the tenant field may be omitted.
	deps := newTestDeps()
	env := &authEnv{deps: deps, fs: deps.Store.(*fakeStore)}
	env.tenantA = &store.Tenant{Slug: "solo", Name: "Solo"}
	_ = env.fs.Tenants().Create(context.Background(), env.tenantA)
	env.user(t, env.tenantA.ID, "alice", store.UserRoleAdmin, pwAlice, false)
	if w := login(Auth{Deps: deps}, `{"username":"alice","password":"`+pwAlice+`"}`); w.Code != http.StatusOK {
		t.Errorf("single-tenant login without tenant: status = %d, body = %s", w.Code, w.Body.String())
	}

	// Several tenants and no configured default: omitting it cannot work.
	multi := newAuthEnv(t)
	multi.deps.Console.DefaultTenant = ""
	multi.user(t, multi.tenantA.ID, "alice", store.UserRoleAdmin, pwAlice, false)
	if w := login(Auth{Deps: multi.deps}, `{"username":"alice","password":"`+pwAlice+`"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("multi-tenant login without tenant or default: status = %d, want 401", w.Code)
	}
}

// ---------------------------------------------------------------------------
// GET /auth/me
// ---------------------------------------------------------------------------

func TestMe_User(t *testing.T) {
	env := newAuthEnv(t)
	alice := env.user(t, env.tenantA.ID, "alice@example.com", store.UserRoleAdmin, pwAlice, true)
	_, sess := env.session(t, alice)
	h := Auth{Deps: env.deps}

	w := serve(http.MethodGet, "/auth/me", h.Me, asUser(httptest.NewRequest(http.MethodGet, "/auth/me", nil), alice, sess))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got meView
	decodeInto(t, w, &got)
	if got.Kind != "user" || got.Subject != alice.ID || got.TenantID != env.tenantA.ID || got.AuthMethod != "session" ||
		got.User == nil || got.User.Username != "alice@example.com" || got.User.Tenant != "acme" || got.User.Role != "admin" ||
		got.MustChangePassword == nil || !*got.MustChangePassword || got.CSRFToken != sess.CSRFToken || got.KeyID != "" {
		t.Errorf("me = %+v (user %+v)", got, got.User)
	}
}

func TestMe_APIKeyKeepsLegacyShape(t *testing.T) {
	h := Auth{Deps: newTestDeps()}
	w := serve(http.MethodGet, "/auth/me", h.Me, asAPIKey(httptest.NewRequest(http.MethodGet, "/auth/me", nil), "tenant-x", "agent"))
	var raw map[string]any
	decodeInto(t, w, &raw)
	if raw["kind"] != "api_key" || raw["subject"] != "key-1" || raw["tenant_id"] != "tenant-x" || raw["auth_method"] != "apikey" || raw["key_id"] != "key-1" {
		t.Errorf("me = %v", raw)
	}
	for _, k := range []string{"user", "must_change_password", "csrf_token"} {
		if _, ok := raw[k]; ok {
			t.Errorf("an API-key /auth/me carries %q", k)
		}
	}
}

// ---------------------------------------------------------------------------
// POST /auth/logout
// ---------------------------------------------------------------------------

func TestLogout_RevokesSessionAndClearsCookie(t *testing.T) {
	env := newAuthEnv(t)
	alice := env.user(t, env.tenantA.ID, "alice", store.UserRoleAdmin, pwAlice, false)
	_, s1 := env.session(t, alice)
	_, s2 := env.session(t, alice)
	h := Auth{Deps: env.deps}

	w := serve(http.MethodPost, "/auth/logout", h.Logout, asUser(httptest.NewRequest(http.MethodPost, "/auth/logout", nil), alice, s1))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d", w.Code)
	}
	if c := w.Result().Cookies(); len(c) != 1 || c[0].Name != "gw_session" || c[0].MaxAge >= 0 {
		t.Errorf("cookies = %+v, want an expiring gw_session", c)
	}
	got1, _ := env.fs.UserSessions().Get(context.Background(), s1.ID)
	got2, _ := env.fs.UserSessions().Get(context.Background(), s2.ID)
	if got1.RevokedAt == nil {
		t.Error("the calling session was not revoked")
	}
	if got2.RevokedAt != nil {
		t.Error("logout revoked another of the user's sessions")
	}
	if hasAudit(env.audit(t, env.tenantA.ID), "logout") == nil {
		t.Error("no logout audit row")
	}
}

func TestLogout_APIKeyIsNoOp(t *testing.T) {
	h := Auth{Deps: newTestDeps()}
	w := serve(http.MethodPost, "/auth/logout", h.Logout, asAPIKey(httptest.NewRequest(http.MethodPost, "/auth/logout", nil), "t", "admin"))
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

// ---------------------------------------------------------------------------
// POST /auth/password
// ---------------------------------------------------------------------------

func changePassword(env *authEnv, u *store.User, s *store.UserSession, body string) *httptest.ResponseRecorder {
	h := Auth{Deps: env.deps}
	return serve(http.MethodPost, "/auth/password", h.ChangePassword, asUser(jsonReq(http.MethodPost, "/auth/password", body), u, s))
}

func TestChangePassword_Success(t *testing.T) {
	env := newAuthEnv(t)
	alice := env.user(t, env.tenantA.ID, "alice", store.UserRoleAdmin, pwAlice, true)
	_, current := env.session(t, alice)
	_, other := env.session(t, alice)
	const newPW = "a-brand-new-password"

	w := changePassword(env, alice, current, `{"current_password":"`+pwAlice+`","new_password":"`+newPW+`"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	u, _ := env.fs.Users().Get(context.Background(), env.tenantA.ID, alice.ID)
	if u.MustChangePassword {
		t.Error("must_change_password still set")
	}
	if ok, _ := password.Verify(newPW, u.PasswordHash); !ok {
		t.Error("the new password does not verify against the stored hash")
	}
	if c, _ := env.fs.UserSessions().Get(context.Background(), current.ID); c.RevokedAt != nil {
		t.Error("the calling session was revoked")
	}
	if o, _ := env.fs.UserSessions().Get(context.Background(), other.ID); o.RevokedAt == nil {
		t.Error("the user's other session survived a password change")
	}
	if hasAudit(env.audit(t, env.tenantA.ID), "password_change") == nil {
		t.Error("no password_change audit row")
	}
}

func TestChangePassword_Rejections(t *testing.T) {
	env := newAuthEnv(t)
	alice := env.user(t, env.tenantA.ID, "alice", store.UserRoleAdmin, pwAlice, false)
	_, s := env.session(t, alice)

	cases := []struct {
		name, body string
	}{
		{"wrong current password", `{"current_password":"wrong-wrong-wrong","new_password":"a-brand-new-password"}`},
		{"new password too short", `{"current_password":"` + pwAlice + `","new_password":"short"}`},
		{"new password equals username", `{"current_password":"` + pwAlice + `","new_password":"ALICE"}`},
		{"new password unchanged", `{"current_password":"` + pwAlice + `","new_password":"` + pwAlice + `"}`},
		{"missing fields", `{"current_password":"` + pwAlice + `"}`},
		{"too long", `{"current_password":"` + pwAlice + `","new_password":"` + strings.Repeat("x", 129) + `"}`},
	}
	for _, c := range cases {
		w := changePassword(env, alice, s, c.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400, body = %s", c.name, w.Code, w.Body.String())
		}
	}
	u, _ := env.fs.Users().Get(context.Background(), env.tenantA.ID, alice.ID)
	if ok, _ := password.Verify(pwAlice, u.PasswordHash); !ok {
		t.Error("a rejected change altered the password")
	}
}

func TestChangePassword_WrongCurrentCountsTowardLockout(t *testing.T) {
	env := newAuthEnv(t)
	alice := env.user(t, env.tenantA.ID, "alice", store.UserRoleAdmin, pwAlice, false)
	_, s := env.session(t, alice)
	for i := 0; i < maxUserFailures; i++ {
		changePassword(env, alice, s, `{"current_password":"wrong-wrong-wrong","new_password":"a-brand-new-password"}`)
	}
	u, _ := env.fs.Users().Get(context.Background(), env.tenantA.ID, alice.ID)
	if u.LockedUntil == nil {
		t.Error("guessing the current password through /auth/password never locked the account")
	}
	// While locked, even the right current password is refused.
	if w := changePassword(env, alice, s, `{"current_password":"`+pwAlice+`","new_password":"a-brand-new-password"}`); w.Code != http.StatusBadRequest {
		t.Errorf("right current password while locked: status = %d, want 400", w.Code)
	}
}

func TestChangePassword_APIKeyForbidden(t *testing.T) {
	h := Auth{Deps: newTestDeps()}
	req := asAPIKey(jsonReq(http.MethodPost, "/auth/password", `{"current_password":"a","new_password":"b"}`), "t", "admin")
	w := serve(http.MethodPost, "/auth/password", h.ChangePassword, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}
