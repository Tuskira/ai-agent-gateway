package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/handlers"
	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	oidcauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/oidc"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/oidc/oidctest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/password"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard/netguardtest"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const ssoConsole = "https://console.example.com"

// ssoEnv is the real router, session Authenticator, CSRF middleware and
// rate limiter over an in-memory store, with single sign-on against a
// local test issuer (internal/auth/oidc/oidctest).
type ssoEnv struct {
	t      *testing.T
	h      http.Handler
	st     *dptest.Store
	iss    *oidctest.Issuer
	tenant *store.Tenant
}

func newSSOEnv(t *testing.T, mutate ...func(*config.OIDCProvider)) *ssoEnv {
	t.Helper()
	netguardtest.AllowLoopback()
	ctx := context.Background()
	st := dptest.New()
	e := &ssoEnv{t: t, st: st, iss: oidctest.New(t)}
	e.tenant = &store.Tenant{ID: "3b9d1c2e-4f5a-4b6c-8d7e-9f0a1b2c3d4e", Slug: "acme", Name: "Acme"}
	other := &store.Tenant{ID: "7c8d9e0f-1a2b-4c3d-9e5f-6a7b8c9d0e1f", Slug: "globex", Name: "Globex"}
	for _, tn := range []*store.Tenant{e.tenant, other} {
		if err := st.Tenants().Create(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}
	pc := config.OIDCProvider{
		Name: "corp", DisplayName: "Corp SSO", Issuer: e.iss.URL, ClientID: e.iss.ClientID,
		ClientSecretEnv: "SSO_TEST_SECRET", RedirectURL: ssoConsole + "/api/v1/auth/oidc/corp/callback",
		Tenant: "acme", AllowedEmailDomains: []string{"example.com"},
	}
	for _, m := range mutate {
		m(&pc)
	}
	key := bytes.Repeat([]byte{7}, 32)
	sso, err := oidcauth.New([]config.OIDCProvider{pc}, key, oidcauth.Options{Getenv: func(string) string { return e.iss.ClientSecret }})
	if err != nil {
		t.Fatal(err)
	}
	limiter := internalauth.NewRateLimiter(internalauth.RateLimiterConfig{Enabled: true, MaxFailures: 10, Window: time.Minute, Lockout: 5 * time.Minute})
	t.Cleanup(limiter.Close)
	sess := session.New(st.Users(), st.UserSessions(), session.Options{})
	keys := &scriptedAuthenticator{principals: map[string]*pkgauth.Principal{
		"platform-key": {Subject: "key-plat", KeyID: "key-plat", TenantID: e.tenant.ID, Roles: pkgauth.RolesForKey(pkgauth.RolePlatformAdmin), AuthMethod: "apikey"},
	}}
	e.h = NewRouter(Deps{
		ServiceVersion: "test",
		Authenticator:  internalauth.RateLimitedAuthenticator(internalauth.Chain(keys, sess), limiter),
		Authorizer:     pkgauth.NewRoleAuthorizer(),
		Store:          st,
		RateLimiter:    limiter,
		Console:        handlers.ConsoleConfig{CookieSecure: "auto", SessionMax: 24 * time.Hour},
		OIDC:           sso,
	})
	return e
}

func (e *ssoEnv) user(username, role string, edit ...func(*store.User)) *store.User {
	e.t.Helper()
	u := &store.User{TenantID: e.tenant.ID, Username: username, PasswordHash: cHash(e.t, cpwAdmin), Role: role, MustChangePassword: true}
	for _, f := range edit {
		f(u)
	}
	if err := e.st.Users().Create(context.Background(), u); err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *ssoEnv) get(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, ssoConsole+path, nil)
	for _, c := range cookies {
		r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// ssoLogin drives start -> IdP -> callback and returns the callback answer.
func (e *ssoEnv) ssoLogin(sub, email, returnTo string) *httptest.ResponseRecorder {
	e.t.Helper()
	start := e.get("/api/v1/auth/oidc/corp/start?return_to=" + url.QueryEscape(returnTo))
	if start.Code != http.StatusFound {
		e.t.Fatalf("start = %d %s", start.Code, start.Body)
	}
	state := cookieNamed(start, oidcauth.StateCookieSecure)
	if state == nil {
		e.t.Fatalf("start set no %s cookie", oidcauth.StateCookieSecure)
	}
	q := e.iss.Authorize(e.t, start.Header().Get("Location"), sub, email)
	return e.get("/api/v1/auth/oidc/corp/callback?"+q.Encode(), state)
}

func (e *ssoEnv) audit(action string) []*store.AuthAuditEntry {
	rows, _, err := e.st.AuthAudit().List(context.Background(), e.tenant.ID, store.AuthAuditListOptions{Action: action})
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func TestSSOAuthConfigListsProviders(t *testing.T) {
	e := newSSOEnv(t)
	w := e.get("/api/v1/auth/config")
	var got struct {
		SSO []map[string]string `json:"sso_providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"name": "corp", "display_name": "Corp SSO", "start_url": "/api/v1/auth/oidc/corp/start"}
	if len(got.SSO) != 1 || got.SSO[0]["name"] != want["name"] || got.SSO[0]["display_name"] != want["display_name"] || got.SSO[0]["start_url"] != want["start_url"] {
		t.Errorf("sso_providers = %v, want [%v]", got.SSO, want)
	}
}

func TestSSOLoginStartsAUserSession(t *testing.T) {
	e := newSSOEnv(t)
	u := e.user("alice@example.com", store.UserRoleViewer)

	w := e.ssoLogin("sub-alice", "Alice@Example.com", "/connectors?tab=1")
	if w.Code != http.StatusOK {
		t.Fatalf("callback = %d %s (Location %s)", w.Code, w.Body, w.Header().Get("Location"))
	}
	body := w.Body.String()
	if !strings.Contains(body, `content="0;url=/connectors?tab=1"`) || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("callback page = %s / CSP %q", body, w.Header().Get("Content-Security-Policy"))
	}
	gw := cookieNamed(w, session.CookieName)
	if gw == nil || gw.Value == "" || !gw.HttpOnly || gw.SameSite != http.SameSiteStrictMode || !gw.Secure || gw.Path != "/" {
		t.Fatalf("session cookie = %+v, want HttpOnly, SameSite=Strict, Secure (https), Path=/", gw)
	}
	for _, name := range []string{oidcauth.StateCookieSecure, oidcauth.StateCookiePlain} {
		if c := cookieNamed(w, name); c == nil || c.MaxAge >= 0 {
			t.Errorf("login-state cookie %s not cleared: %+v", name, c)
		}
	}

	me := e.get("/api/v1/auth/me", gw)
	var view struct {
		Kind       string `json:"kind"`
		AuthMethod string `json:"auth_method"`
		TenantID   string `json:"tenant_id"`
		CSRFToken  string `json:"csrf_token"`
		User       struct {
			ID     string `json:"id"`
			Role   string `json:"role"`
			Tenant string `json:"tenant"`
		} `json:"user"`
		MustChangePassword bool `json:"must_change_password"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if me.Code != http.StatusOK || view.Kind != "user" || view.AuthMethod != pkgauth.AuthMethodOIDC || view.TenantID != e.tenant.ID ||
		view.User.ID != u.ID || view.User.Role != store.UserRoleViewer || view.User.Tenant != "acme" || view.CSRFToken == "" {
		t.Fatalf("/auth/me = %d %s", me.Code, me.Body)
	}
	// The pre-made user's pending password change does not gate an SSO session.
	if view.MustChangePassword {
		t.Error("must_change_password = true on an SSO session")
	}

	// CSRF applies to an SSO session like any other.
	logout := func(csrf string) int {
		r := httptest.NewRequest(http.MethodPost, ssoConsole+"/api/v1/auth/logout", nil)
		r.AddCookie(&http.Cookie{Name: gw.Name, Value: gw.Value})
		if csrf != "" {
			r.Header.Set(session.CSRFHeader, csrf)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := logout(""); code != http.StatusForbidden {
		t.Errorf("logout without CSRF = %d, want 403", code)
	}
	if code := logout(view.CSRFToken); code != http.StatusNoContent {
		t.Errorf("logout with CSRF = %d, want 204", code)
	}
	if me := e.get("/api/v1/auth/me", gw); me.Code != http.StatusUnauthorized {
		t.Errorf("/auth/me after logout = %d, want 401", me.Code)
	}

	ok := e.audit("login_ok")
	if len(ok) != 1 || ok[0].Detail != "oidc:corp" || ok[0].TargetUserID != u.ID {
		t.Errorf("login_ok audit = %+v", ok)
	}
	link, err := e.st.UserIdentities().GetBySubject(context.Background(), e.tenant.ID, "corp", e.iss.URL, "sub-alice")
	if err != nil || link.UserID != u.ID || link.Email != "alice@example.com" {
		t.Errorf("identity link = %+v, %v", link, err)
	}
}

func TestSSOLaterLoginMatchesBySubject(t *testing.T) {
	e := newSSOEnv(t)
	u := e.user("alice@example.com", store.UserRoleAdmin)
	if w := e.ssoLogin("sub-alice", "alice@example.com", "/"); w.Code != http.StatusOK {
		t.Fatalf("first login = %d", w.Code)
	}
	// The address changed at the IdP; the subject still names alice.
	w := e.ssoLogin("sub-alice", "alice.renamed@example.com", "/")
	if w.Code != http.StatusOK {
		t.Fatalf("second login = %d (Location %s)", w.Code, w.Header().Get("Location"))
	}
	me := e.get("/api/v1/auth/me", cookieNamed(w, session.CookieName))
	if !strings.Contains(me.Body.String(), u.ID) {
		t.Errorf("/auth/me = %s, want user %s", me.Body, u.ID)
	}
}

func TestSSODeletedUserLinkIsReplaced(t *testing.T) {
	e := newSSOEnv(t)
	old := e.user("alice@example.com", store.UserRoleViewer)
	if w := e.ssoLogin("sub-alice", "alice@example.com", "/"); w.Code != http.StatusOK {
		t.Fatalf("first login = %d", w.Code)
	}
	if err := e.st.Users().SoftDelete(context.Background(), e.tenant.ID, old.ID); err != nil {
		t.Fatal(err)
	}
	if w := e.ssoLogin("sub-alice", "alice@example.com", "/"); w.Header().Get("Location") != "/login?sso_error=no_user" {
		t.Fatalf("login after delete: %d %s, want no_user", w.Code, w.Header().Get("Location"))
	}
	if _, err := e.st.UserIdentities().GetBySubject(context.Background(), e.tenant.ID, "corp", e.iss.URL, "sub-alice"); err == nil {
		t.Error("the link to the deleted user was kept")
	}
	fresh := e.user("alice@example.com", store.UserRoleViewer)
	w := e.ssoLogin("sub-alice", "alice@example.com", "/")
	if w.Code != http.StatusOK {
		t.Fatalf("login after re-create = %d %s", w.Code, w.Header().Get("Location"))
	}
	if link, _ := e.st.UserIdentities().GetBySubject(context.Background(), e.tenant.ID, "corp", e.iss.URL, "sub-alice"); link == nil || link.UserID != fresh.ID {
		t.Errorf("link = %+v, want the re-created user %s", link, fresh.ID)
	}
}

func TestSSORefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.OIDCProvider)
		setup  func(e *ssoEnv)
		sub    string
		email  string
		code   string
	}{
		{name: "unknown user", sub: "s", email: "nobody@example.com", code: oidcauth.CodeNoUser},
		{name: "user in another tenant", setup: func(e *ssoEnv) {
			u := &store.User{TenantID: "7c8d9e0f-1a2b-4c3d-9e5f-6a7b8c9d0e1f", Username: "bob@example.com", PasswordHash: "x", Role: "admin"}
			_ = e.st.Users().Create(context.Background(), u)
		}, sub: "s", email: "bob@example.com", code: oidcauth.CodeNoUser},
		{name: "disallowed domain", setup: func(e *ssoEnv) { e.user("eve@evil.example.net", "viewer") },
			sub: "s", email: "eve@evil.example.net", code: oidcauth.CodeDomain},
		{name: "disabled user", setup: func(e *ssoEnv) { e.user("dan@example.com", "viewer", func(u *store.User) { u.Disabled = true }) },
			sub: "s", email: "dan@example.com", code: oidcauth.CodeDisabled},
		{name: "platform-admin", setup: func(e *ssoEnv) { e.user("root@example.com", store.UserRolePlatformAdmin) },
			sub: "s", email: "root@example.com", code: oidcauth.CodeRole},
		{name: "already linked to another subject", setup: func(e *ssoEnv) {
			e.user("carol@example.com", "viewer")
			if w := e.ssoLogin("sub-carol", "carol@example.com", "/"); w.Code != http.StatusOK {
				e.t.Fatalf("setup login = %d", w.Code)
			}
		}, sub: "sub-intruder", email: "carol@example.com", code: oidcauth.CodeLinked},
		{name: "unverified email", setup: func(e *ssoEnv) {
			e.user("vic@example.com", "viewer")
			e.iss.Claims = func(c map[string]any) { c["email_verified"] = false }
		}, sub: "s", email: "vic@example.com", code: oidcauth.CodeUnverified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var muts []func(*config.OIDCProvider)
			if tc.mutate != nil {
				muts = append(muts, tc.mutate)
			}
			e := newSSOEnv(t, muts...)
			if tc.setup != nil {
				tc.setup(e)
			}
			w := e.ssoLogin(tc.sub, tc.email, "/")
			if w.Code != http.StatusFound || w.Header().Get("Location") != "/login?sso_error="+tc.code {
				t.Fatalf("callback = %d Location %q, want 302 to /login?sso_error=%s", w.Code, w.Header().Get("Location"), tc.code)
			}
			if c := cookieNamed(w, session.CookieName); c != nil && c.MaxAge >= 0 {
				t.Errorf("a refused login set a session cookie: %+v", c)
			}
			fails := e.audit("login_fail")
			if len(fails) == 0 || fails[0].Detail != "oidc:corp:"+tc.code {
				t.Errorf("login_fail audit = %+v, want detail oidc:corp:%s", fails, tc.code)
			}
		})
	}
}

func TestSSOPlatformAdminAllowedWhenConfigured(t *testing.T) {
	e := newSSOEnv(t, func(p *config.OIDCProvider) { p.AllowPlatformAdmin = true })
	e.user("root@example.com", store.UserRolePlatformAdmin)
	if w := e.ssoLogin("s", "root@example.com", "/"); w.Code != http.StatusOK {
		t.Fatalf("callback = %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestSSOForgedStateAndUnknownProvider(t *testing.T) {
	e := newSSOEnv(t)
	e.user("alice@example.com", "viewer")
	start := e.get("/api/v1/auth/oidc/corp/start")
	q := e.iss.Authorize(t, start.Header().Get("Location"), "s", "alice@example.com")
	q.Set("state", "forged")
	w := e.get("/api/v1/auth/oidc/corp/callback?"+q.Encode(), cookieNamed(start, oidcauth.StateCookieSecure))
	if w.Header().Get("Location") != "/login?sso_error=state" {
		t.Errorf("forged state: Location %q", w.Header().Get("Location"))
	}
	for _, path := range []string{"/api/v1/auth/oidc/nope/start", "/api/v1/auth/oidc/nope/callback?code=x&state=y"} {
		if w := e.get(path); w.Code != http.StatusFound || w.Header().Get("Location") != "/login?sso_error=provider" {
			t.Errorf("%s = %d %q, want 302 to sso_error=provider", path, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestSSOReturnToIsSanitized(t *testing.T) {
	e := newSSOEnv(t)
	e.user("alice@example.com", "viewer")
	w := e.ssoLogin("s", "alice@example.com", "//evil.example.net/x")
	if !strings.Contains(w.Body.String(), `content="0;url=/"`) {
		t.Errorf("an off-site return_to was kept: %s", w.Body)
	}
}

func TestSSOSessionEndsWhenUserDisabled(t *testing.T) {
	e := newSSOEnv(t)
	u := e.user("alice@example.com", "viewer")
	gw := cookieNamed(e.ssoLogin("s", "alice@example.com", "/"), session.CookieName)
	if e.get("/api/v1/auth/me", gw).Code != http.StatusOK {
		t.Fatal("session not usable")
	}
	u.Disabled = true
	if err := e.st.Users().Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if w := e.get("/api/v1/auth/me", gw); w.Code != http.StatusUnauthorized {
		t.Errorf("/auth/me after disable = %d, want 401", w.Code)
	}
}

func TestSSOProviderSessionMax(t *testing.T) {
	e := newSSOEnv(t, func(p *config.OIDCProvider) { p.SessionMax = time.Hour })
	e.user("alice@example.com", "viewer")
	w := e.ssoLogin("s", "alice@example.com", "/")
	gw := cookieNamed(w, session.CookieName)
	if gw == nil || gw.MaxAge != 3600 {
		t.Fatalf("session cookie = %+v, want MaxAge 3600", gw)
	}
	sess, err := e.st.UserSessions().Get(context.Background(), session.HashToken(gw.Value))
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(sess.ExpiresAt); left > time.Hour || left < 59*time.Minute || sess.AuthMethod != store.SessionAuthOIDC {
		t.Errorf("session = expires in %v, method %q; want about 1h, oidc", left, sess.AuthMethod)
	}
}

func TestSSOJustInTimeProvisioning(t *testing.T) {
	e := newSSOEnv(t, func(p *config.OIDCProvider) { p.Provision = config.OIDCProvisionJIT })
	e.iss.Claims = func(c map[string]any) { c["name"] = "Nina New" }
	w := e.ssoLogin("sub-nina", "nina@example.com", "/")
	if w.Code != http.StatusOK {
		t.Fatalf("callback = %d %s", w.Code, w.Header().Get("Location"))
	}
	u, err := e.st.Users().GetByUsername(context.Background(), e.tenant.ID, "nina@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != store.UserRoleViewer || !password.IsUnusable(u.PasswordHash) || u.DisplayName != "Nina New" || u.MustChangePassword {
		t.Errorf("provisioned user = %+v", u)
	}
	if len(e.audit("user_create")) != 1 {
		t.Error("no user_create audit row")
	}

	// The account has no password: password login fails like any other.
	r := httptest.NewRequest(http.MethodPost, ssoConsole+"/api/v1/auth/login",
		strings.NewReader(`{"tenant":"acme","username":"nina@example.com","password":"`+password.Unusable+`"}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("password login of an SSO-only user = %d, want 401", rec.Code)
	}
	fails := e.audit("login_fail")
	if len(fails) == 0 || fails[0].Detail != "no_password" {
		t.Errorf("login_fail audit = %+v, want no_password", fails)
	}
}

// apiKeyCall makes a platform-admin API-key request (no cookie, no CSRF).
func (e *ssoEnv) apiKeyCall(method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, ssoConsole+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer platform-key")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func newUnverifiedSSOEnv(t *testing.T, mutate ...func(*config.OIDCProvider)) *ssoEnv {
	t.Helper()
	f := false
	e := newSSOEnv(t, append([]func(*config.OIDCProvider){func(p *config.OIDCProvider) {
		p.RequireVerifiedEmail = &f
		p.RequiredUpstreamIdP = "EntraID"
	}}, mutate...)...)
	e.iss.Claims = func(c map[string]any) {
		c["email_verified"] = false
		c["identities"] = []map[string]any{{"providerName": "EntraID"}}
	}
	return e
}

// Finding 1: with verification off, the (possibly self-edited) email must
// not link to a user who has a password.
func TestSSOUnverifiedEmailDoesNotLinkToPasswordUsers(t *testing.T) {
	e := newUnverifiedSSOEnv(t)
	admin := e.user("boss@example.com", store.UserRoleAdmin, func(u *store.User) { u.MustChangePassword = false })
	w := e.ssoLogin("attacker-sub", "boss@example.com", "/")
	if w.Header().Get("Location") != "/login?sso_error=link_required" {
		t.Fatalf("callback = %d %q, want sso_error=link_required", w.Code, w.Header().Get("Location"))
	}
	if _, err := e.st.UserIdentities().GetByUser(context.Background(), e.tenant.ID, "corp", e.iss.URL, admin.ID); err == nil {
		t.Error("the password user was linked")
	}
	if fails := e.audit("login_fail"); len(fails) == 0 || fails[0].Detail != "oidc:corp:link_required" {
		t.Errorf("login_fail audit = %+v", fails)
	}
	// Even a user whose temporary password was never used has a usable
	// password and is refused.
	e.user("fresh@example.com", store.UserRoleViewer)
	if w := e.ssoLogin("s2", "fresh@example.com", "/"); w.Header().Get("Location") != "/login?sso_error=link_required" {
		t.Errorf("must-change user: %q, want link_required", w.Header().Get("Location"))
	}
}

func TestSSOUnverifiedEmailLinksSSOOnlyUsers(t *testing.T) {
	e := newUnverifiedSSOEnv(t)
	// Created through the API with sso_only.
	w := e.apiKeyCall(http.MethodPost, "/api/v1/users", `{"username":"sso.only@example.com","role":"viewer","sso_only":true}`)
	if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), "temporary_password") || !strings.Contains(w.Body.String(), `"sso_only":true`) {
		t.Fatalf("POST /users sso_only = %d %s", w.Code, w.Body)
	}
	u, _ := e.st.Users().GetByUsername(context.Background(), e.tenant.ID, "sso.only@example.com")
	if u == nil || !password.IsUnusable(u.PasswordHash) || u.MustChangePassword {
		t.Fatalf("sso_only user = %+v", u)
	}
	if w := e.ssoLogin("sub-sso", "sso.only@example.com", "/"); w.Code != http.StatusOK {
		t.Fatalf("SSO-only login = %d %q", w.Code, w.Header().Get("Location"))
	}
	// Once linked, the link (by subject) keeps working.
	if w := e.ssoLogin("sub-sso", "sso.only@example.com", "/"); w.Code != http.StatusOK {
		t.Errorf("second login = %d", w.Code)
	}
}

func TestSSOUnverifiedJITStillWorks(t *testing.T) {
	e := newUnverifiedSSOEnv(t, func(p *config.OIDCProvider) { p.Provision = config.OIDCProvisionJIT })
	if w := e.ssoLogin("sub-new", "new.person@example.com", "/"); w.Code != http.StatusOK {
		t.Fatalf("jit login = %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestCreateUserSSOOnlyValidation(t *testing.T) {
	e := newSSOEnv(t)
	w := e.apiKeyCall(http.MethodPost, "/api/v1/users", `{"username":"x@example.com","role":"viewer","sso_only":true,"password":"a-long-password-123"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("sso_only with password = %d, want 400", w.Code)
	}
	w = e.apiKeyCall(http.MethodPost, "/api/v1/users", `{"username":"y@example.com","role":"viewer"}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"sso_only":false`) || !strings.Contains(w.Body.String(), "temporary_password") {
		t.Errorf("ordinary create = %d %s", w.Code, w.Body)
	}
}

// Finding 2: failures before the login state verifies (anyone can make a
// visitor's browser send them) never lock the visitor's IP out.
func TestSSOUnverifiedStateFailuresDoNotLockOut(t *testing.T) {
	e := newSSOEnv(t)
	for i := 0; i < 25; i++ {
		e.get("/api/v1/auth/oidc/corp/callback?code=x&state=forged")
		e.get("/api/v1/auth/oidc/corp/callback?error=access_denied&state=x")
		e.get("/api/v1/auth/oidc/nope/callback?code=x&state=y")
	}
	if w := e.get("/api/v1/auth/config"); w.Code != http.StatusOK {
		t.Fatalf("/auth/config after 75 forged callbacks = %d, want 200 (not locked out)", w.Code)
	}
	e.user("alice@example.com", "viewer")
	if w := e.ssoLogin("s", "alice@example.com", "/"); w.Code != http.StatusOK {
		t.Errorf("real login after forged callbacks = %d", w.Code)
	}
	if fails := e.audit("login_fail"); len(fails) == 0 {
		t.Error("forged callbacks are no longer audited")
	}
}

func TestSSOPostStateFailuresStillCount(t *testing.T) {
	e := newSSOEnv(t)
	for i := 0; i < 10; i++ {
		if w := e.ssoLogin("s", "nobody@example.com", "/"); w.Header().Get("Location") != "/login?sso_error=no_user" {
			t.Fatalf("login %d: %q", i, w.Header().Get("Location"))
		}
	}
	if w := e.get("/api/v1/auth/config"); w.Code != http.StatusTooManyRequests {
		t.Errorf("/auth/config after 10 refused real logins = %d, want 429", w.Code)
	}
}

// Finding 3: an unused temporary password does not outlive the first SSO
// link.
func TestSSOFirstLinkDiscardsTemporaryPassword(t *testing.T) {
	e := newSSOEnv(t)
	temp := e.user("temp@example.com", store.UserRoleViewer) // MustChangePassword: true
	keep := e.user("keeper@example.com", store.UserRoleViewer, func(u *store.User) { u.MustChangePassword = false })
	for _, u := range []*store.User{temp, keep} {
		if w := e.ssoLogin("sub-"+u.Username, u.Username, "/"); w.Code != http.StatusOK {
			t.Fatalf("%s: login = %d", u.Username, w.Code)
		}
	}
	got, _ := e.st.Users().Get(context.Background(), e.tenant.ID, temp.ID)
	if !password.IsUnusable(got.PasswordHash) || got.MustChangePassword {
		t.Errorf("temporary password kept: hash unusable=%v must_change=%v", password.IsUnusable(got.PasswordHash), got.MustChangePassword)
	}
	resets := e.audit("password_reset")
	if len(resets) != 1 || resets[0].TargetUserID != temp.ID || !strings.Contains(resets[0].Detail, "temporary password discarded") {
		t.Errorf("password_reset audit = %+v", resets)
	}
	if got, _ := e.st.Users().Get(context.Background(), e.tenant.ID, keep.ID); got.PasswordHash != keep.PasswordHash {
		t.Error("a user's own (non-temporary) password was replaced")
	}
	// The old temporary password no longer signs in.
	r := httptest.NewRequest(http.MethodPost, ssoConsole+"/api/v1/auth/login",
		strings.NewReader(`{"tenant":"acme","username":"temp@example.com","password":"`+cpwAdmin+`"}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("password login with the discarded temporary password = %d, want 401", rec.Code)
	}
}

// Finding 4: promotion to platform-admin ends existing (SSO) sessions, so
// the provider's platform-admin check applies again at the next login.
func TestSSOPromotionToPlatformAdminRevokesSessions(t *testing.T) {
	e := newSSOEnv(t)
	u := e.user("alice@example.com", store.UserRoleAdmin)
	gw := cookieNamed(e.ssoLogin("s", "alice@example.com", "/"), session.CookieName)
	if e.get("/api/v1/auth/me", gw).Code != http.StatusOK {
		t.Fatal("session not usable")
	}
	if w := e.apiKeyCall(http.MethodPatch, "/api/v1/users/"+u.ID, `{"role":"platform-admin"}`); w.Code != http.StatusOK {
		t.Fatalf("promote = %d %s", w.Code, w.Body)
	}
	if w := e.get("/api/v1/auth/me", gw); w.Code != http.StatusUnauthorized {
		t.Errorf("/auth/me after promotion = %d, want 401", w.Code)
	}
	if w := e.ssoLogin("s", "alice@example.com", "/"); w.Header().Get("Location") != "/login?sso_error=role" {
		t.Errorf("SSO login after promotion: %q, want sso_error=role", w.Header().Get("Location"))
	}
}
