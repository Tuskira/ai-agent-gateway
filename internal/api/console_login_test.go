package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/handlers"
	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/password"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// consoleEnv is the real router (routes, middleware order, permissions)
// over the real session Authenticator, the real RoleAuthorizer and the real
// rate limiter, with an in-memory store. API keys are scripted tokens: what
// is under test here is the cookie/CSRF/gate/permission behaviour, not key
// hashing (internal/auth/apikey covers that).
type consoleEnv struct {
	t       *testing.T
	h       http.Handler
	st      *dptest.Store
	tenantA *store.Tenant
	tenantB *store.Tenant
	limiter *internalauth.RateLimiter
}

const (
	cpwAdmin  = "admin-password-123"
	cpwViewer = "viewer-password-456"
)

var (
	chMu sync.Mutex
	chC  = map[string]string{}
)

func cHash(t *testing.T, pw string) string {
	t.Helper()
	chMu.Lock()
	defer chMu.Unlock()
	if h, ok := chC[pw]; ok {
		return h
	}
	h, err := password.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	chC[pw] = h
	return h
}

func newConsoleEnv(t *testing.T) *consoleEnv {
	t.Helper()
	ctx := context.Background()
	st := dptest.New()
	e := &consoleEnv{t: t, st: st}
	e.tenantA = &store.Tenant{Slug: "acme", Name: "Acme"}
	e.tenantB = &store.Tenant{Slug: "globex", Name: "Globex"}
	for _, tn := range []*store.Tenant{e.tenantA, e.tenantB} {
		if err := st.Tenants().Create(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}

	e.limiter = internalauth.NewRateLimiter(internalauth.RateLimiterConfig{Enabled: true, MaxFailures: 10, Window: time.Minute, Lockout: 5 * time.Minute})
	t.Cleanup(e.limiter.Close)

	keys := &scriptedAuthenticator{principals: map[string]*pkgauth.Principal{
		"admin-key-a":  {Subject: "key-admin", KeyID: "key-admin", TenantID: e.tenantA.ID, Roles: []string{"admin"}, AuthMethod: "apikey"},
		"platform-key": {Subject: "key-plat", KeyID: "key-plat", TenantID: e.tenantA.ID, Roles: pkgauth.RolesForKey(pkgauth.RolePlatformAdmin), AuthMethod: "apikey"},
		"agent-key-a":  {Subject: "key-agent", KeyID: "key-agent", TenantID: e.tenantA.ID, Roles: []string{"agent"}, AuthMethod: "apikey"},
	}}
	sess := session.New(st.Users(), st.UserSessions(), session.Options{})
	e.h = NewRouter(Deps{
		ServiceVersion: "test",
		Authenticator:  internalauth.RateLimitedAuthenticator(internalauth.Chain(keys, sess), e.limiter),
		Authorizer:     pkgauth.NewRoleAuthorizer(),
		Store:          st,
		RateLimiter:    e.limiter,
		Console:        handlers.ConsoleConfig{APIKeyLogin: true, DefaultTenant: "acme", CookieSecure: "auto"},
	})
	return e
}

func (e *consoleEnv) user(tenantID, username, role, pw string, mustChange bool) *store.User {
	e.t.Helper()
	u := &store.User{TenantID: tenantID, Username: username, PasswordHash: cHash(e.t, pw), Role: role, MustChangePassword: mustChange}
	if err := e.st.Users().Create(context.Background(), u); err != nil {
		e.t.Fatal(err)
	}
	return u
}

// client is a tiny cookie-and-CSRF-aware browser.
type client struct {
	e      *consoleEnv
	cookie *http.Cookie
	csrf   string
}

func (e *consoleEnv) browser() *client { return &client{e: e} }

func (c *client) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if c.cookie != nil {
		r.AddCookie(c.cookie)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	c.e.h.ServeHTTP(w, r)
	return w
}

// write is do() with the CSRF header a real console would send.
func (c *client) write(method, path, body string) *httptest.ResponseRecorder {
	return c.do(method, path, body, map[string]string{session.CSRFHeader: c.csrf})
}

func (c *client) login(tenant, username, pw string) *httptest.ResponseRecorder {
	w := c.do(http.MethodPost, "/api/v1/auth/login", `{"tenant":"`+tenant+`","username":"`+username+`","password":"`+pw+`"}`, nil)
	if w.Code == http.StatusOK {
		var resp struct {
			CSRFToken string `json:"csrf_token"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		c.csrf = resp.CSRFToken
		for _, ck := range w.Result().Cookies() {
			if ck.Name == session.CookieName {
				c.cookie = ck
			}
		}
	}
	return w
}

func key(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func errType(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return b.Error.Type
}

// ---------------------------------------------------------------------------

func TestConsole_PublicRoutesNeedNoCredential(t *testing.T) {
	e := newConsoleEnv(t)
	c := e.browser()
	w := c.do(http.MethodGet, "/api/v1/auth/config", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /auth/config = %d", w.Code)
	}
	var cfg map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &cfg)
	if cfg["password_login"] != true || cfg["api_key_login"] != true || cfg["single_tenant"] != false || cfg["default_tenant"] != "acme" {
		t.Errorf("config = %v", cfg)
	}
	// Login is reachable without credentials (and answers 401 for bad ones, not for "no credential").
	if w := c.login("acme", "nobody", "whatever-password"); w.Code != http.StatusUnauthorized {
		t.Errorf("login with bad credentials = %d, want 401", w.Code)
	}
}

func TestConsole_FullSessionLifecycle(t *testing.T) {
	e := newConsoleEnv(t)
	alice := e.user(e.tenantA.ID, "alice", "admin", cpwAdmin, false)
	c := e.browser()

	if w := c.do(http.MethodGet, "/api/v1/auth/me", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("/auth/me without a session = %d, want 401", w.Code)
	}
	w := c.login("acme", "alice", cpwAdmin)
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d, body = %s", w.Code, w.Body.String())
	}

	w = c.do(http.MethodGet, "/api/v1/auth/me", "", nil)
	var me struct {
		Kind      string `json:"kind"`
		CSRFToken string `json:"csrf_token"`
		User      struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Role     string `json:"role"`
			Tenant   string `json:"tenant"`
		} `json:"user"`
		MustChangePassword *bool `json:"must_change_password"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if w.Code != http.StatusOK || me.Kind != "user" || me.User.ID != alice.ID || me.User.Tenant != "acme" || me.CSRFToken != c.csrf ||
		me.MustChangePassword == nil || *me.MustChangePassword {
		t.Fatalf("/auth/me = %d %s", w.Code, w.Body.String())
	}

	// A cookie-authenticated read needs no CSRF token; a write does.
	if w := c.do(http.MethodGet, "/api/v1/users", "", nil); w.Code != http.StatusOK {
		t.Errorf("GET /users with the cookie = %d, want 200", w.Code)
	}
	if w := c.do(http.MethodPost, "/api/v1/users", `{"username":"bobby","role":"viewer"}`, nil); w.Code != http.StatusForbidden || errType(t, w) != "csrf_error" {
		t.Errorf("POST /users without X-CSRF-Token = %d %s, want 403 csrf_error", w.Code, w.Body.String())
	}
	if w := c.do(http.MethodPost, "/api/v1/users", `{"username":"bobby","role":"viewer"}`, map[string]string{session.CSRFHeader: "forged"}); w.Code != http.StatusForbidden {
		t.Errorf("POST /users with a wrong token = %d, want 403", w.Code)
	}
	if w := c.write(http.MethodPost, "/api/v1/users", `{"username":"bobby","role":"viewer"}`); w.Code != http.StatusCreated {
		t.Errorf("POST /users with the token = %d, want 201, body = %s", w.Code, w.Body.String())
	}

	// Logout ends the session server-side: the old cookie is dead.
	if w := c.write(http.MethodPost, "/api/v1/auth/logout", ""); w.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", w.Code)
	}
	if w := c.do(http.MethodGet, "/api/v1/auth/me", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("/auth/me with a logged-out cookie = %d, want 401", w.Code)
	}
}

func TestConsole_LogoutNeedsCSRFToo(t *testing.T) {
	e := newConsoleEnv(t)
	e.user(e.tenantA.ID, "alice", "admin", cpwAdmin, false)
	c := e.browser()
	c.login("acme", "alice", cpwAdmin)
	if w := c.do(http.MethodPost, "/api/v1/auth/logout", "", nil); w.Code != http.StatusForbidden {
		t.Errorf("logout without a CSRF token = %d, want 403 (logout CSRF)", w.Code)
	}
}

func TestConsole_MustChangePasswordGate(t *testing.T) {
	e := newConsoleEnv(t)
	e.user(e.tenantA.ID, "newbie", "admin", cpwAdmin, true)
	c := e.browser()
	w := c.login("acme", "newbie", cpwAdmin)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"must_change_password":true`) {
		t.Fatalf("login = %d %s", w.Code, w.Body.String())
	}

	for _, req := range [][2]string{
		{http.MethodGet, "/api/v1/users"},
		{http.MethodGet, "/api/v1/connectors"},
		{http.MethodGet, "/api/v1/skills"},
		{http.MethodGet, "/api/v1/auth/audit"},
		{http.MethodGet, "/api/v1/analytics/overview"},
	} {
		w := c.do(req[0], req[1], "", nil)
		if w.Code != http.StatusForbidden || errType(t, w) != "password_change_required" {
			t.Errorf("%s %s while must-change = %d %s, want 403 password_change_required", req[0], req[1], w.Code, w.Body.String())
		}
	}
	if w := c.do(http.MethodGet, "/api/v1/auth/me", "", nil); w.Code != http.StatusOK {
		t.Errorf("/auth/me while must-change = %d, want 200", w.Code)
	}
	if w := c.do(http.MethodGet, "/api/v1/auth/config", "", nil); w.Code != http.StatusOK {
		t.Errorf("/auth/config while must-change = %d, want 200", w.Code)
	}

	// Changing the password lifts the gate on the same session.
	if w := c.write(http.MethodPost, "/api/v1/auth/password", `{"current_password":"`+cpwAdmin+`","new_password":"my-new-secret-pass"}`); w.Code != http.StatusNoContent {
		t.Fatalf("change password = %d, body = %s", w.Code, w.Body.String())
	}
	if w := c.do(http.MethodGet, "/api/v1/users", "", nil); w.Code != http.StatusOK {
		t.Errorf("GET /users after changing the password = %d, want 200", w.Code)
	}
}

func TestConsole_MustChangeUserCanStillLogout(t *testing.T) {
	e := newConsoleEnv(t)
	e.user(e.tenantA.ID, "newbie", "admin", cpwAdmin, true)
	c := e.browser()
	c.login("acme", "newbie", cpwAdmin)
	if w := c.write(http.MethodPost, "/api/v1/auth/logout", ""); w.Code != http.StatusNoContent {
		t.Errorf("logout while must-change = %d, want 204", w.Code)
	}
}

func TestConsole_ViewerIsReadOnly(t *testing.T) {
	e := newConsoleEnv(t)
	e.user(e.tenantA.ID, "vera", "viewer", cpwViewer, false)
	c := e.browser()
	if w := c.login("acme", "vera", cpwViewer); w.Code != http.StatusOK {
		t.Fatalf("login = %d", w.Code)
	}
	allow := []string{"/api/v1/connectors", "/api/v1/profiles", "/api/v1/skills", "/api/v1/models", "/api/v1/auth/me"}
	for _, p := range allow {
		if w := c.do(http.MethodGet, p, "", nil); w.Code != http.StatusOK {
			t.Errorf("viewer GET %s = %d, want 200 (body %s)", p, w.Code, w.Body.String())
		}
	}
	// cache/stats is backed by the MCP data plane, absent here (503): what
	// matters is that the viewer got past the permission check.
	if w := c.do(http.MethodGet, "/api/v1/cache/stats", "", nil); w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Errorf("viewer GET /cache/stats = %d, want it permitted", w.Code)
	}
	deny := [][2]string{
		{http.MethodGet, "/api/v1/users"},
		{http.MethodGet, "/api/v1/auth/audit"},
		{http.MethodGet, "/api/v1/api-keys"},
		{http.MethodGet, "/api/v1/credentials"},
		{http.MethodGet, "/api/v1/tenants"},
	}
	for _, r := range deny {
		if w := c.do(r[0], r[1], "", nil); w.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", r[0], r[1], w.Code)
		}
	}
	for _, r := range [][2]string{{http.MethodPost, "/api/v1/connectors"}, {http.MethodDelete, "/api/v1/profiles/00000000-0000-0000-0000-000000000000"}, {http.MethodPost, "/api/v1/users"}} {
		if w := c.write(r[0], r[1], `{}`); w.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", r[0], r[1], w.Code)
		}
	}
	// A viewer can still change their own password.
	if w := c.write(http.MethodPost, "/api/v1/auth/password", `{"current_password":"`+cpwViewer+`","new_password":"viewer-new-password"}`); w.Code != http.StatusNoContent {
		t.Errorf("viewer changes own password = %d, want 204, body = %s", w.Code, w.Body.String())
	}
}

func TestConsole_AdminUserLacksPlatformAdmin(t *testing.T) {
	e := newConsoleEnv(t)
	e.user(e.tenantA.ID, "alice", "admin", cpwAdmin, false)
	c := e.browser()
	c.login("acme", "alice", cpwAdmin)

	if w := c.do(http.MethodGet, "/api/v1/tenants", "", nil); w.Code != http.StatusForbidden {
		t.Errorf("admin user GET /tenants = %d, want 403 (platform.admin is API-key only)", w.Code)
	}
	if w := c.write(http.MethodPost, "/api/v1/tenants", `{"slug":"new","name":"New"}`); w.Code != http.StatusForbidden {
		t.Errorf("admin user POST /tenants = %d, want 403", w.Code)
	}
	for _, p := range []string{"/api/v1/connectors", "/api/v1/users", "/api/v1/auth/audit"} {
		if w := c.do(http.MethodGet, p, "", nil); w.Code != http.StatusOK {
			t.Errorf("admin user GET %s = %d, want 200", p, w.Code)
		}
	}
	// A tenant admin key lacks it too; only a platform-admin key holds it.
	if w := c.do(http.MethodGet, "/api/v1/tenants", "", key("admin-key-a")); w.Code != http.StatusForbidden {
		t.Errorf("tenant admin key GET /tenants = %d, want 403", w.Code)
	}
	if w := c.do(http.MethodGet, "/api/v1/tenants", "", key("platform-key")); w.Code != http.StatusOK {
		t.Errorf("platform-admin key GET /tenants = %d, want 200", w.Code)
	}
}

func TestConsole_APIKeys_UsersRoutes(t *testing.T) {
	e := newConsoleEnv(t)
	bob := e.user(e.tenantA.ID, "bobby", "viewer", cpwViewer, false)
	gary := e.user(e.tenantB.ID, "gary", "admin", cpwAdmin, false)
	c := e.browser() // no cookie: API keys only

	// Agent keys get 403 on every users/audit route.
	for _, r := range [][2]string{
		{http.MethodGet, "/api/v1/users"},
		{http.MethodPost, "/api/v1/users"},
		{http.MethodGet, "/api/v1/users/" + bob.ID},
		{http.MethodPatch, "/api/v1/users/" + bob.ID},
		{http.MethodDelete, "/api/v1/users/" + bob.ID},
		{http.MethodPost, "/api/v1/users/" + bob.ID + "/reset-password"},
		{http.MethodPost, "/api/v1/users/" + bob.ID + "/revoke-sessions"},
		{http.MethodGet, "/api/v1/auth/audit"},
	} {
		if w := c.do(r[0], r[1], `{}`, key("agent-key-a")); w.Code != http.StatusForbidden {
			t.Errorf("agent key %s %s = %d, want 403", r[0], r[1], w.Code)
		}
	}
	// ...and cannot change a password (no session).
	if w := c.do(http.MethodPost, "/api/v1/auth/password", `{"current_password":"a","new_password":"b"}`, key("admin-key-a")); w.Code != http.StatusForbidden {
		t.Errorf("admin key POST /auth/password = %d, want 403 (users only)", w.Code)
	}

	// An admin key needs no CSRF token and can reset a tenant user's password.
	w := c.do(http.MethodPost, "/api/v1/users/"+bob.ID+"/reset-password", "", key("admin-key-a"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"temporary_password"`) {
		t.Fatalf("admin key reset = %d %s", w.Code, w.Body.String())
	}
	// The temporary password works for a login, and forces a change.
	var temp struct {
		TemporaryPassword string `json:"temporary_password"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &temp)
	bc := e.browser()
	lw := bc.login("acme", "bobby", temp.TemporaryPassword)
	if lw.Code != http.StatusOK || !strings.Contains(lw.Body.String(), `"must_change_password":true`) {
		t.Errorf("login with the temp password = %d %s", lw.Code, lw.Body.String())
	}
	// Cross-tenant: tenant A's key cannot see or touch tenant B's user.
	for _, r := range [][2]string{
		{http.MethodGet, "/api/v1/users/" + gary.ID},
		{http.MethodPatch, "/api/v1/users/" + gary.ID},
		{http.MethodDelete, "/api/v1/users/" + gary.ID},
		{http.MethodPost, "/api/v1/users/" + gary.ID + "/reset-password"},
	} {
		if w := c.do(r[0], r[1], `{"disabled":true}`, key("admin-key-a")); w.Code != http.StatusNotFound {
			t.Errorf("admin key of tenant A %s %s = %d, want 404", r[0], r[1], w.Code)
		}
	}
	// A non-UUID id is a 404 before any handler runs.
	if w := c.do(http.MethodGet, "/api/v1/users/not-a-uuid", "", key("admin-key-a")); w.Code != http.StatusNotFound {
		t.Errorf("GET /users/not-a-uuid = %d, want 404", w.Code)
	}
}

func TestConsole_ResetKicksTheUserOut(t *testing.T) {
	e := newConsoleEnv(t)
	e.user(e.tenantA.ID, "bobby", "viewer", cpwViewer, false)
	bc := e.browser()
	bc.login("acme", "bobby", cpwViewer)
	if w := bc.do(http.MethodGet, "/api/v1/auth/me", "", nil); w.Code != http.StatusOK {
		t.Fatalf("bobby's session not working: %d", w.Code)
	}
	var bobID string
	users, _ := e.st.Users().List(context.Background(), e.tenantA.ID, store.UserListOptions{})
	bobID = users[0].ID
	if w := e.browser().do(http.MethodPost, "/api/v1/users/"+bobID+"/reset-password", "", key("admin-key-a")); w.Code != http.StatusOK {
		t.Fatalf("reset = %d", w.Code)
	}
	if w := bc.do(http.MethodGet, "/api/v1/auth/me", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("bobby's old session after a reset = %d, want 401", w.Code)
	}
}

func TestConsole_DisabledAndDeletedUsersLoseAccessImmediately(t *testing.T) {
	e := newConsoleEnv(t)
	bob := e.user(e.tenantA.ID, "bobby", "viewer", cpwViewer, false)
	bc := e.browser()
	bc.login("acme", "bobby", cpwViewer)

	bob.Disabled = true
	_ = e.st.Users().Update(context.Background(), bob)
	if w := bc.do(http.MethodGet, "/api/v1/connectors", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("disabled user's live session = %d, want 401", w.Code)
	}
}

func TestConsole_ChangePasswordRevokesOtherSessions(t *testing.T) {
	e := newConsoleEnv(t)
	e.user(e.tenantA.ID, "alice", "admin", cpwAdmin, false)
	first, second := e.browser(), e.browser()
	first.login("acme", "alice", cpwAdmin)
	second.login("acme", "alice", cpwAdmin)
	if w := second.write(http.MethodPost, "/api/v1/auth/password", `{"current_password":"`+cpwAdmin+`","new_password":"another-long-password"}`); w.Code != http.StatusNoContent {
		t.Fatalf("change password = %d, body = %s", w.Code, w.Body.String())
	}
	if w := first.do(http.MethodGet, "/api/v1/auth/me", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("the other session after a password change = %d, want 401", w.Code)
	}
	if w := second.do(http.MethodGet, "/api/v1/auth/me", "", nil); w.Code != http.StatusOK {
		t.Errorf("the changing session = %d, want 200", w.Code)
	}
	// Old password no longer logs in; new one does.
	if w := e.browser().login("acme", "alice", cpwAdmin); w.Code != http.StatusUnauthorized {
		t.Errorf("login with the old password = %d, want 401", w.Code)
	}
	if w := e.browser().login("acme", "alice", "another-long-password"); w.Code != http.StatusOK {
		t.Errorf("login with the new password = %d, want 200", w.Code)
	}
}

func TestConsole_IPLockoutAppliesToLogin(t *testing.T) {
	e := newConsoleEnv(t)
	e.user(e.tenantA.ID, "alice", "admin", cpwAdmin, false)
	c := e.browser()
	for i := 0; i < 10; i++ {
		if w := c.login("acme", "alice", "wrong-wrong-wrong"); w.Code != http.StatusUnauthorized && w.Code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d = %d", i+1, w.Code)
		}
	}
	// 10 failures from one IP: now everything from it gets 429, even the right password.
	w := c.login("acme", "alice", cpwAdmin)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Errorf("login after the IP lockout = %d (Retry-After %q), want 429 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestConsole_NoCredentialRequestsDoNotCountTowardLockout(t *testing.T) {
	e := newConsoleEnv(t)
	e.user(e.tenantA.ID, "alice", "admin", cpwAdmin, false)
	c := e.browser()
	// The console probes /auth/me on every load; many probes must not lock the IP out.
	for i := 0; i < 30; i++ {
		if w := c.do(http.MethodGet, "/api/v1/auth/me", "", nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("probe %d = %d, want 401", i+1, w.Code)
		}
	}
	if w := c.login("acme", "alice", cpwAdmin); w.Code != http.StatusOK {
		t.Errorf("login after 30 credential-less probes = %d, want 200", w.Code)
	}
}

func TestConsole_BearerKeyWinsOverCookie_AndNoCSRFForKeys(t *testing.T) {
	e := newConsoleEnv(t)
	e.user(e.tenantA.ID, "vera", "viewer", cpwViewer, false)
	c := e.browser()
	c.login("acme", "vera", cpwViewer)
	// The browser still holds vera's cookie, but an Authorization header means API-key auth:
	// an admin key from that same client can write with no CSRF header.
	w := c.do(http.MethodPost, "/api/v1/users", `{"username":"newuser","role":"viewer"}`, key("admin-key-a"))
	if w.Code != http.StatusCreated {
		t.Errorf("admin key POST /users with a stray cookie = %d, want 201, body = %s", w.Code, w.Body.String())
	}
}

func TestConsole_StaleCookieIsRejected(t *testing.T) {
	e := newConsoleEnv(t)
	c := e.browser()
	c.cookie = &http.Cookie{Name: session.CookieName, Value: "forged-or-expired-token"}
	if w := c.do(http.MethodGet, "/api/v1/connectors", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("unknown cookie = %d, want 401", w.Code)
	}
}

func TestConsole_OpenAPIListsNewRoutes(t *testing.T) {
	e := newConsoleEnv(t)
	w := e.browser().do(http.MethodGet, "/api/v1/openapi.json", "", nil)
	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for path, methods := range map[string][]string{
		"/api/v1/auth/config":                {"get"},
		"/api/v1/auth/login":                 {"post"},
		"/api/v1/auth/logout":                {"post"},
		"/api/v1/auth/password":              {"post"},
		"/api/v1/auth/audit":                 {"get"},
		"/api/v1/users":                      {"get", "post"},
		"/api/v1/users/{id}":                 {"get", "patch", "delete"},
		"/api/v1/users/{id}/reset-password":  {"post"},
		"/api/v1/users/{id}/revoke-sessions": {"post"},
	} {
		for _, m := range methods {
			if doc.Paths[path][m] == nil {
				t.Errorf("openapi is missing %s %s", strings.ToUpper(m), path)
			}
		}
	}
	if doc.Paths["/api/v1/auth/login"]["post"].(map[string]any)["security"] != nil {
		t.Error("login is documented as requiring authentication")
	}
}
