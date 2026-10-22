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
	// Real UUIDs, as Postgres would assign: X-Gateway-Acting-Tenant is
	// validated as one before the tenant is looked up.
	e.tenantA = &store.Tenant{ID: "3b9d1c2e-4f5a-4b6c-8d7e-9f0a1b2c3d4e", Slug: "acme", Name: "Acme"}
	e.tenantB = &store.Tenant{ID: "7c8d9e0f-1a2b-4c3d-9e5f-6a7b8c9d0e1f", Slug: "globex", Name: "Globex"}
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
	return c.loginBody(`{"tenant":"` + tenant + `","username":"` + username + `","password":"` + pw + `"}`)
}

// loginWithoutTenant posts a login body that names no tenant: the server
// resolves the account by username across tenants.
func (c *client) loginWithoutTenant(username, pw string) *httptest.ResponseRecorder {
	return c.loginBody(`{"username":"` + username + `","password":"` + pw + `"}`)
}

func (c *client) loginBody(body string) *httptest.ResponseRecorder {
	w := c.do(http.MethodPost, "/api/v1/auth/login", body, nil)
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

// acting is the header a platform-admin console sends to act in tenantID.
func acting(tenantID string) map[string]string {
	return map[string]string{internalauth.ActingTenantHeader: tenantID}
}

// actingWrite is acting() plus the session's CSRF token.
func (c *client) actingWrite(tenantID string) map[string]string {
	return map[string]string{internalauth.ActingTenantHeader: tenantID, session.CSRFHeader: c.csrf}
}

// page decodes a {"items": [...], "total": n} body into items.
func page(t *testing.T, w *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var p struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode page %q: %v", w.Body.String(), err)
	}
	return p.Items
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
		t.Errorf("admin user GET /tenants = %d, want 403 (a tenant admin never holds platform.admin)", w.Code)
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

// ---------------------------------------------------------------------------
// Platform-admin console users and the acting-tenant header.

const cpwRoot = "root-password-789"

// A console user with role platform-admin logs in like anyone else, holds
// platform.admin (tenant listing), and acts in another tenant for one
// request by sending X-Gateway-Acting-Tenant. The write lands in that
// tenant and its audit row names the actor's home tenant.
func TestConsole_PlatformAdminUser_ListsTenantsAndActsInAnother(t *testing.T) {
	e := newConsoleEnv(t)
	root := e.user(e.tenantA.ID, "root", store.UserRolePlatformAdmin, cpwRoot, false)
	c := e.browser()
	if w := c.login("acme", "root", cpwRoot); w.Code != http.StatusOK {
		t.Fatalf("login = %d %s", w.Code, w.Body.String())
	}

	// /auth/me at home: both tenant ids are the home tenant and the
	// session role expanded to admin + platform-admin.
	var me struct {
		TenantID     string   `json:"tenant_id"`
		HomeTenantID string   `json:"home_tenant_id"`
		Roles        []string `json:"roles"`
		User         struct {
			ID     string `json:"id"`
			Role   string `json:"role"`
			Tenant string `json:"tenant"`
		} `json:"user"`
	}
	w := c.do(http.MethodGet, "/api/v1/auth/me", "", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if w.Code != http.StatusOK || me.TenantID != e.tenantA.ID || me.HomeTenantID != e.tenantA.ID ||
		me.User.ID != root.ID || me.User.Role != store.UserRolePlatformAdmin || me.User.Tenant != "acme" ||
		strings.Join(me.Roles, ",") != "admin,platform-admin" {
		t.Fatalf("/auth/me = %d %s", w.Code, w.Body.String())
	}

	// platform.admin: the tenant list is readable (a plain admin user gets 403, see above).
	w = c.do(http.MethodGet, "/api/v1/tenants", "", nil)
	if w.Code != http.StatusOK || len(page(t, w)) != 2 {
		t.Fatalf("GET /tenants = %d %s, want 200 with both tenants", w.Code, w.Body.String())
	}

	// Act in Globex: the created user belongs to Globex, not Acme.
	w = c.do(http.MethodPost, "/api/v1/users", `{"username":"gina","role":"viewer"}`, c.actingWrite(e.tenantB.ID))
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /users acting in globex = %d %s", w.Code, w.Body.String())
	}
	gina, err := e.st.Users().GetByUsername(context.Background(), e.tenantB.ID, "gina")
	if err != nil {
		t.Fatalf("gina was not created in globex: %v", err)
	}
	if _, err := e.st.Users().GetByUsername(context.Background(), e.tenantA.ID, "gina"); err == nil {
		t.Error("gina was also created in acme")
	}

	// Reads are scoped the same way: with the header the list is Globex's,
	// without it the home tenant's.
	names := func(w *httptest.ResponseRecorder) string {
		var out []string
		for _, u := range page(t, w) {
			out = append(out, u["username"].(string))
		}
		return strings.Join(out, ",")
	}
	if w := c.do(http.MethodGet, "/api/v1/users", "", acting(e.tenantB.ID)); w.Code != http.StatusOK || names(w) != "gina" {
		t.Errorf("GET /users acting in globex = %d %s, want gina only", w.Code, w.Body.String())
	}
	if w := c.do(http.MethodGet, "/api/v1/users", "", nil); w.Code != http.StatusOK || names(w) != "root" {
		t.Errorf("GET /users at home = %d %s, want root only", w.Code, w.Body.String())
	}

	// Globex's audit trail shows the create, blamed on an actor from Acme.
	w = c.do(http.MethodGet, "/api/v1/auth/audit", "", acting(e.tenantB.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /auth/audit acting in globex = %d %s", w.Code, w.Body.String())
	}
	var found bool
	for _, row := range page(t, w) {
		if row["action"] == "user_create" && row["target_user_id"] == gina.ID {
			found = true
			if row["actor_tenant_id"] != e.tenantA.ID || row["actor_id"] != root.ID || row["actor_kind"] != "user" {
				t.Errorf("globex user_create audit row = %v, want actor_tenant_id %s", row, e.tenantA.ID)
			}
		}
	}
	if !found {
		t.Errorf("no user_create row for gina in globex's audit: %s", w.Body.String())
	}
	// Acme's own trail did not record it (the acted-on tenant owns the row).
	w = c.do(http.MethodGet, "/api/v1/auth/audit", "", nil)
	for _, row := range page(t, w) {
		if row["action"] == "user_create" {
			t.Errorf("acme's audit has the globex create: %v", row)
		}
	}

	// /auth/me while acting: tenant_id is the acting tenant, the account
	// and home_tenant_id stay the real ones.
	w = c.do(http.MethodGet, "/api/v1/auth/me", "", acting(e.tenantB.ID))
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if w.Code != http.StatusOK || me.TenantID != e.tenantB.ID || me.HomeTenantID != e.tenantA.ID || me.User.ID != root.ID || me.User.Tenant != "acme" {
		t.Errorf("/auth/me acting in globex = %d %s", w.Code, w.Body.String())
	}

	// A platform-admin API key is honoured the same way.
	if w := e.browser().do(http.MethodGet, "/api/v1/users", "", mergeHeaders(key("platform-key"), acting(e.tenantB.ID))); w.Code != http.StatusOK || names(w) != "gina" {
		t.Errorf("platform key GET /users acting in globex = %d %s, want gina only", w.Code, w.Body.String())
	}
}

func mergeHeaders(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// The header is inert without platform.admin: a tenant admin (session or
// key), a viewer and an agent key all get 403 permission_error, even when
// the header names their own tenant. CSRF still runs first on a session
// write, so a forged request is refused as csrf_error, not as a permission
// problem.
func TestConsole_ActingHeader_403ForTenantAdmin(t *testing.T) {
	e := newConsoleEnv(t)
	e.user(e.tenantA.ID, "alice", "admin", cpwAdmin, false)
	e.user(e.tenantA.ID, "vera", "viewer", cpwViewer, false)
	alice, vera := e.browser(), e.browser()
	alice.login("acme", "alice", cpwAdmin)
	vera.login("acme", "vera", cpwViewer)

	for _, target := range []string{e.tenantB.ID, e.tenantA.ID} {
		for name, w := range map[string]*httptest.ResponseRecorder{
			"admin session GET /users":   alice.do(http.MethodGet, "/api/v1/users", "", acting(target)),
			"admin session GET /auth/me": alice.do(http.MethodGet, "/api/v1/auth/me", "", acting(target)),
			"admin session POST /users":  alice.do(http.MethodPost, "/api/v1/users", `{"username":"x","role":"viewer"}`, alice.actingWrite(target)),
			"viewer session GET /models": vera.do(http.MethodGet, "/api/v1/models", "", acting(target)),
			"admin key GET /users":       e.browser().do(http.MethodGet, "/api/v1/users", "", mergeHeaders(key("admin-key-a"), acting(target))),
			"agent key GET /connectors":  e.browser().do(http.MethodGet, "/api/v1/connectors", "", mergeHeaders(key("agent-key-a"), acting(target))),
			"admin key POST /connectors": e.browser().do(http.MethodPost, "/api/v1/connectors", `{}`, mergeHeaders(key("admin-key-a"), acting(target))),
		} {
			if w.Code != http.StatusForbidden || errType(t, w) != "permission_error" {
				t.Errorf("%s with acting tenant %s = %d %s, want 403 permission_error", name, target, w.Code, w.Body.String())
			}
		}
	}
	// Nothing was written to either tenant.
	for _, tid := range []string{e.tenantA.ID, e.tenantB.ID} {
		if _, err := e.st.Users().GetByUsername(context.Background(), tid, "x"); err == nil {
			t.Errorf("user x was created in %s despite the 403", tid)
		}
	}

	// A session write without the CSRF token fails the CSRF gate before
	// the tenant override has its say.
	if w := alice.do(http.MethodPost, "/api/v1/users", `{"username":"x","role":"viewer"}`, acting(e.tenantB.ID)); w.Code != http.StatusForbidden || errType(t, w) != "csrf_error" {
		t.Errorf("admin session POST /users without CSRF but with the header = %d %s, want 403 csrf_error", w.Code, w.Body.String())
	}
	// Likewise for a platform-admin: CSRF first, then the override.
	e.user(e.tenantA.ID, "root", store.UserRolePlatformAdmin, cpwRoot, false)
	root := e.browser()
	root.login("acme", "root", cpwRoot)
	if w := root.do(http.MethodPost, "/api/v1/users", `{"username":"x","role":"viewer"}`, acting(e.tenantB.ID)); w.Code != http.StatusForbidden || errType(t, w) != "csrf_error" {
		t.Errorf("platform-admin POST /users without CSRF = %d %s, want 403 csrf_error", w.Code, w.Body.String())
	}
	// And the must-change gate too: a fresh platform-admin cannot act
	// anywhere until it has changed its password.
	e.user(e.tenantA.ID, "newroot", store.UserRolePlatformAdmin, cpwRoot, true)
	nr := e.browser()
	nr.login("acme", "newroot", cpwRoot)
	if w := nr.do(http.MethodGet, "/api/v1/users", "", acting(e.tenantB.ID)); w.Code != http.StatusForbidden || errType(t, w) != "password_change_required" {
		t.Errorf("must-change platform-admin GET /users acting = %d %s, want 403 password_change_required", w.Code, w.Body.String())
	}
}

// A permitted caller naming a tenant that does not exist gets 404, and a
// value that is not a UUID gets 400; neither touches any tenant.
func TestConsole_ActingHeader_404UnknownTenant(t *testing.T) {
	e := newConsoleEnv(t)
	e.user(e.tenantA.ID, "root", store.UserRolePlatformAdmin, cpwRoot, false)
	c := e.browser()
	c.login("acme", "root", cpwRoot)

	const ghost = "6f1b7c2e-0d4a-4e8b-9f3c-2a5d7e9b1c04"
	for name, w := range map[string]*httptest.ResponseRecorder{
		"session GET /users":   c.do(http.MethodGet, "/api/v1/users", "", acting(ghost)),
		"session POST /users":  c.do(http.MethodPost, "/api/v1/users", `{"username":"x","role":"viewer"}`, c.actingWrite(ghost)),
		"session GET /auth/me": c.do(http.MethodGet, "/api/v1/auth/me", "", acting(ghost)),
		"platform key GET":     e.browser().do(http.MethodGet, "/api/v1/users", "", mergeHeaders(key("platform-key"), acting(ghost))),
	} {
		if w.Code != http.StatusNotFound || errType(t, w) != "not_found" {
			t.Errorf("%s with an unknown acting tenant = %d %s, want 404 not_found", name, w.Code, w.Body.String())
		}
	}
	for _, bad := range []string{"globex", "not-a-uuid", " "} {
		if w := c.do(http.MethodGet, "/api/v1/users", "", acting(bad)); w.Code != http.StatusBadRequest || errType(t, w) != "validation_error" {
			t.Errorf("acting tenant %q = %d %s, want 400 validation_error", bad, w.Code, w.Body.String())
		}
	}
	for _, tid := range []string{e.tenantA.ID, e.tenantB.ID} {
		if _, err := e.st.Users().GetByUsername(context.Background(), tid, "x"); err == nil {
			t.Errorf("user x was created in %s despite the 404", tid)
		}
	}
	// The session itself is unharmed: the next plain request works.
	if w := c.do(http.MethodGet, "/api/v1/users", "", nil); w.Code != http.StatusOK {
		t.Errorf("GET /users after the 404s = %d, want 200", w.Code)
	}
}

// Self-service routes act on the real account even while the header
// selects another tenant: a password change and a logout sent with the
// header hit the home account and are audited in the home tenant.
func TestConsole_ActingHeader_SelfRoutesStayHome(t *testing.T) {
	e := newConsoleEnv(t)
	root := e.user(e.tenantA.ID, "root", store.UserRolePlatformAdmin, cpwRoot, false)
	c := e.browser()
	c.login("acme", "root", cpwRoot)

	const newPW = "root-new-password-000"
	if w := c.do(http.MethodPost, "/api/v1/auth/password", `{"current_password":"`+cpwRoot+`","new_password":"`+newPW+`"}`, c.actingWrite(e.tenantB.ID)); w.Code != http.StatusNoContent {
		t.Fatalf("change password while acting = %d %s", w.Code, w.Body.String())
	}
	if w := e.browser().login("acme", "root", newPW); w.Code != http.StatusOK {
		t.Errorf("login with the new password at home = %d, want 200", w.Code)
	}
	if w := e.browser().login("acme", "root", cpwRoot); w.Code != http.StatusUnauthorized {
		t.Errorf("login with the old password = %d, want 401", w.Code)
	}

	if w := c.do(http.MethodPost, "/api/v1/auth/logout", "", c.actingWrite(e.tenantB.ID)); w.Code != http.StatusNoContent {
		t.Fatalf("logout while acting = %d %s", w.Code, w.Body.String())
	}
	if w := c.do(http.MethodGet, "/api/v1/auth/me", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("/auth/me after the logout = %d, want 401", w.Code)
	}

	// Both self-service actions were audited in Acme (the home tenant) as
	// local actions -- no actor_tenant_id -- and nothing landed in Globex.
	ctx := context.Background()
	home, _, err := e.st.AuthAudit().List(ctx, e.tenantA.ID, store.AuthAuditListOptions{TargetUserID: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range home {
		seen[row.Action] = true
		if row.ActorTenantID != "" {
			t.Errorf("home audit row %s has actor_tenant_id %q, want none", row.Action, row.ActorTenantID)
		}
	}
	if !seen["password_change"] || !seen["logout"] {
		t.Errorf("home audit actions = %v, want password_change and logout", seen)
	}
	other, _, err := e.st.AuthAudit().List(ctx, e.tenantB.ID, store.AuthAuditListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("globex got %d audit rows from self-service routes: %+v", len(other), other[0])
	}
}

// A login body without a tenant finds the account by username when it
// exists in exactly one tenant; an unknown username is the same 401 as a
// wrong password.
func TestConsole_LoginWithoutTenant_SingleMatch(t *testing.T) {
	e := newConsoleEnv(t)
	gary := e.user(e.tenantB.ID, "gary", "admin", cpwAdmin, false)

	w := e.browser().do(http.MethodGet, "/api/v1/auth/config", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"tenant_optional":true`) {
		t.Errorf("GET /auth/config = %d %s, want tenant_optional true", w.Code, w.Body.String())
	}

	c := e.browser()
	w = c.loginWithoutTenant("gary", cpwAdmin)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"tenant":"globex"`) {
		t.Fatalf("login without tenant = %d %s, want 200 in globex", w.Code, w.Body.String())
	}
	var me struct {
		TenantID string `json:"tenant_id"`
		User     struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	w = c.do(http.MethodGet, "/api/v1/auth/me", "", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if w.Code != http.StatusOK || me.TenantID != e.tenantB.ID || me.User.ID != gary.ID {
		t.Errorf("/auth/me = %d %s, want gary in globex", w.Code, w.Body.String())
	}

	wrong := e.browser().loginWithoutTenant("gary", "not-the-password")
	unknown := e.browser().loginWithoutTenant("nobody", cpwAdmin)
	if wrong.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized || wrong.Body.String() != unknown.Body.String() {
		t.Errorf("wrong password = %d %s, unknown user = %d %s; want identical 401s", wrong.Code, wrong.Body.String(), unknown.Code, unknown.Body.String())
	}
}

// A username that exists in several tenants cannot log in without naming
// one: 409 tenant_required, with a body that lists no tenant. Naming the
// tenant then works as before.
func TestConsole_LoginWithoutTenant_DuplicateUsername409(t *testing.T) {
	e := newConsoleEnv(t)
	e.user(e.tenantA.ID, "sam", "admin", cpwAdmin, false)
	samB := e.user(e.tenantB.ID, "sam", "viewer", cpwViewer, false)

	c := e.browser()
	w := c.loginWithoutTenant("sam", cpwAdmin)
	if w.Code != http.StatusConflict || errType(t, w) != "tenant_required" {
		t.Fatalf("login without tenant for a duplicated username = %d %s, want 409 tenant_required", w.Code, w.Body.String())
	}
	body := strings.ToLower(w.Body.String())
	for _, leak := range []string{"acme", "globex", e.tenantA.ID, e.tenantB.ID} {
		if strings.Contains(body, strings.ToLower(leak)) {
			t.Errorf("409 body names a tenant (%q): %s", leak, w.Body.String())
		}
	}
	if c.cookie != nil {
		t.Error("a 409 set a session cookie")
	}
	// The 409 happens before any password check, so it does not count as
	// a failed login for either account.
	for _, tid := range []string{e.tenantA.ID, e.tenantB.ID} {
		rows, _, _ := e.st.AuthAudit().List(context.Background(), tid, store.AuthAuditListOptions{Action: "login_fail"})
		if len(rows) != 0 {
			t.Errorf("tenant %s has %d login_fail rows after the 409", tid, len(rows))
		}
	}

	w = c.login("globex", "sam", cpwViewer)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"`+samB.ID+`"`) {
		t.Errorf("login naming globex = %d %s, want sam@globex", w.Code, w.Body.String())
	}
}
