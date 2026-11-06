//go:build e2e

package e2e

// Single sign-on end to end: the real gateway (gateway.Run, real Postgres)
// against a REAL Keycloak, driven like a browser (an HTTP client with a
// cookie jar that fills in Keycloak's HTML login form). Nothing is faked:
// discovery, JWKS, the token endpoint, PKCE enforcement and the login form
// are Keycloak's own.
//
// Run it with a Keycloak in dev mode (any recent version; the docs pin the
// one this was written against):
//
//	docker run -d --rm --name gw-oidc-keycloak -p 127.0.0.1:18180:8080 \
//	  -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
//	  quay.io/keycloak/keycloak:26.3.3 start-dev
//
//	GATEWAY_TEST_DATABASE_URL=postgres://... \
//	GATEWAY_MASTER_KEY=$(go run ./cmd/gateway secrets genkey) \
//	GATEWAY_TEST_KEYCLOAK_URL=http://localhost:18180 \
//	GATEWAY_TEST_KEYCLOAK_ADMIN=admin GATEWAY_TEST_KEYCLOAK_ADMIN_PASSWORD=admin \
//	go test -tags e2e ./test/e2e/ -run TestOIDCKeycloak -v
//
// The test creates its own realm (deleted afterwards), confidential client
// and users through Keycloak's admin REST API. Keycloak is addressed as
// "localhost" and the gateway as "127.0.0.1", so the two keep separate
// cookies in the test's jar, as two sites do in a browser.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/password"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/gateway"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const kcUserPassword = "Kc-e2e-password-1"

// keycloak is a thin admin REST client for one test realm.
type keycloak struct {
	t     *testing.T
	base  string
	realm string
	token string
}

func newKeycloak(t *testing.T) *keycloak {
	t.Helper()
	base := strings.TrimRight(os.Getenv("GATEWAY_TEST_KEYCLOAK_URL"), "/")
	user, pw := os.Getenv("GATEWAY_TEST_KEYCLOAK_ADMIN"), os.Getenv("GATEWAY_TEST_KEYCLOAK_ADMIN_PASSWORD")
	if base == "" || user == "" || pw == "" {
		t.Skip("GATEWAY_TEST_KEYCLOAK_URL / _ADMIN / _ADMIN_PASSWORD not set; skipping the Keycloak SSO end-to-end test")
	}
	resp, err := http.PostForm(base+"/realms/master/protocol/openid-connect/token", url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {user}, "password": {pw},
	})
	if err != nil {
		t.Fatalf("keycloak admin token: %v", err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&tok) != nil || tok.AccessToken == "" {
		t.Fatalf("keycloak admin token: HTTP %d", resp.StatusCode)
	}
	return &keycloak{t: t, base: base, token: tok.AccessToken}
}

func (k *keycloak) admin(method, path string, body any) {
	k.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, k.base+"/admin/realms"+path, rd)
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		k.t.Fatalf("keycloak %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		k.t.Fatalf("keycloak %s %s: HTTP %d %s", method, path, resp.StatusCode, b)
	}
}

// setup creates a realm with a confidential, PKCE-enforcing client whose
// only redirect URIs are redirectURLs, and returns the issuer URL. The
// client's ID tokens carry a hard-coded Cognito-style identities claim
// ([{"providerName":"corp"}]) so required_upstream_idp can be exercised.
func (k *keycloak) setup(clientSecret string, redirectURLs ...string) string {
	k.t.Helper()
	k.realm = "gw-e2e-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	k.admin(http.MethodPost, "", map[string]any{"realm": k.realm, "enabled": true, "sslRequired": "none"})
	k.t.Cleanup(func() { k.admin(http.MethodDelete, "/"+k.realm, nil) })
	k.admin(http.MethodPost, "/"+k.realm+"/clients", map[string]any{
		"clientId": "gateway", "enabled": true, "protocol": "openid-connect",
		"publicClient": false, "clientAuthenticatorType": "client-secret", "secret": clientSecret,
		"standardFlowEnabled": true, "directAccessGrantsEnabled": false,
		"redirectUris": redirectURLs,
		"attributes":   map[string]string{"pkce.code.challenge.method": "S256"},
		"protocolMappers": []map[string]any{{
			"name": "identities", "protocol": "openid-connect", "protocolMapper": "oidc-hardcoded-claim-mapper",
			"config": map[string]string{
				"claim.name": "identities", "claim.value": `[{"providerName":"corp"}]`, "jsonType.label": "JSON",
				"id.token.claim": "true", "access.token.claim": "false", "userinfo.token.claim": "false",
			},
		}},
	})
	return k.base + "/realms/" + k.realm
}

func (k *keycloak) user(username, email string) {
	k.t.Helper()
	k.admin(http.MethodPost, "/"+k.realm+"/users", map[string]any{
		"username": username, "email": email, "emailVerified": true, "enabled": true,
		"firstName": "E2E", "lastName": username,
		"credentials": []map[string]any{{"type": "password", "value": kcUserPassword, "temporary": false}},
	})
}

// browser is one user agent: its own cookie jar, following redirects
// except back to the gateway, so each gateway answer can be inspected.
type browser struct {
	t      *testing.T
	c      *http.Client
	gwHost string
}

func newBrowser(t *testing.T, gwHost string) *browser {
	jar, _ := cookiejar.New(nil)
	b := &browser{t: t, gwHost: gwHost}
	b.c = &http.Client{Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Host == gwHost {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	return b
}

func (b *browser) do(req *http.Request) (*http.Response, string) {
	b.t.Helper()
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (b *browser) get(rawURL string) (*http.Response, string) {
	req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	return b.do(req)
}

var reLoginForm = regexp.MustCompile(`<form[^>]+id="kc-form-login"[^>]+action="([^"]+)"`)

// toIdPCallback starts a login at the gateway, signs in at Keycloak's
// login form as username, and returns the redirect Keycloak sends the
// browser to (the gateway callback URL), without following it.
func (b *browser) toIdPCallback(gw, username string) *url.URL {
	return b.toIdPCallbackVia(gw, "keycloak", username)
}

func (b *browser) toIdPCallbackVia(gw, provider, username string) *url.URL {
	b.t.Helper()
	// The start route answers 302 to Keycloak, which the client follows
	// (it leaves the gateway host) to Keycloak's HTML login form.
	resp, body := b.get(gw + "/api/v1/auth/oidc/" + provider + "/start?return_to=%2Fconnectors")
	m := reLoginForm.FindStringSubmatch(body)
	if resp.StatusCode != http.StatusOK || m == nil || resp.Request.URL.Host == b.gwHost {
		b.t.Fatalf("start -> keycloak login page: HTTP %d at %s, no kc-form-login form", resp.StatusCode, resp.Request.URL)
	}
	req, _ := http.NewRequest(http.MethodPost, html.UnescapeString(m[1]),
		strings.NewReader(url.Values{"username": {username}, "password": {kcUserPassword}, "credentialId": {""}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, body = b.do(req)
	if resp.StatusCode != http.StatusFound {
		b.t.Fatalf("keycloak login POST: HTTP %d (did the form reject the credentials?) %.300s", resp.StatusCode, body)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Host != b.gwHost {
		b.t.Fatalf("keycloak redirected to %q, want the gateway callback", resp.Header.Get("Location"))
	}
	return loc
}

// login runs the whole flow and returns the gateway's callback answer.
func (b *browser) login(gw, username string) (*http.Response, string) {
	b.t.Helper()
	return b.get(b.toIdPCallback(gw, username).String())
}

func TestOIDCKeycloakEndToEnd(t *testing.T) {
	kc := newKeycloak(t)
	a := setupRunEnv(t)

	const secret = "e2e-client-secret-0123456789"
	gw := "http://" + a.api
	issuer := kc.setup(secret, gw+"/api/v1/auth/oidc/keycloak/callback", gw+"/api/v1/auth/oidc/kc-unverified/callback")
	for _, u := range [][2]string{
		{"alice", "alice@example.com"}, {"dan", "dan@example.com"}, {"root", "root@example.com"},
		{"nobody", "nobody@example.com"}, {"eve", "eve@evil.example.net"},
		// For the kc-unverified provider, which reads the address from
		// preferred_username (the Keycloak username) instead of email.
		{"boss@example.com", "boss.mail@elsewhere.example.net"},
		{"sso.only@example.com", "sso.mail@elsewhere.example.net"},
	} {
		kc.user(u[0], u[1])
	}

	providers, _ := json.Marshal([]map[string]any{{
		"name": "keycloak", "display_name": "Keycloak", "issuer": issuer, "client_id": "gateway",
		"client_secret_env": "GATEWAY_E2E_KC_SECRET", "redirect_url": gw + "/api/v1/auth/oidc/keycloak/callback",
		"tenant": "sso-e2e", "allowed_email_domains": []string{"example.com"},
	}, {
		// require_verified_email off: an address links only to SSO-only users.
		"name": "kc-unverified", "issuer": issuer, "client_id": "gateway",
		"client_secret_env": "GATEWAY_E2E_KC_SECRET", "redirect_url": gw + "/api/v1/auth/oidc/kc-unverified/callback",
		"tenant": "sso-e2e", "allowed_email_domains": []string{"example.com"},
		"require_verified_email": false, "required_upstream_idp": "corp", "email_claim": "preferred_username",
	}})
	t.Setenv("GATEWAY_AUTH_OIDC_PROVIDERS", string(providers))
	t.Setenv("GATEWAY_E2E_KC_SECRET", secret)
	// The issuer is on this machine: open the egress guard for loopback.
	t.Setenv("GATEWAY_EGRESS_ALLOWED_CIDRS", "127.0.0.0/8,::1/128")
	startRun(t, a, gateway.Options{Version: "oidc-e2e"})

	// The gateway's own database: a tenant and pre-made users.
	ctx := context.Background()
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		os.Getenv("GATEWAY_DATABASE_USER"), url.QueryEscape(os.Getenv("GATEWAY_DATABASE_PASSWORD")),
		os.Getenv("GATEWAY_DATABASE_HOST"), os.Getenv("GATEWAY_DATABASE_PORT"),
		os.Getenv("GATEWAY_DATABASE_DATABASE"), os.Getenv("GATEWAY_DATABASE_SSL_MODE"))
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := postgres.New(db)
	tenant := &store.Tenant{Slug: "sso-e2e", Name: "SSO e2e"}
	if err := st.Tenants().Create(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	hash, err := password.Hash("a-temporary-password-1")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(username, role string, disabled bool) *store.User {
		u := &store.User{TenantID: tenant.ID, Username: username, PasswordHash: hash, Role: role, MustChangePassword: true, Disabled: disabled}
		if err := st.Users().Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	alice := mk("alice@example.com", store.UserRoleAdmin, false)
	mk("dan@example.com", store.UserRoleViewer, true)
	mk("root@example.com", store.UserRolePlatformAdmin, false)
	mk("eve@evil.example.net", store.UserRoleViewer, false)
	boss := mk("boss@example.com", store.UserRoleAdmin, false)
	if err := st.Users().SetPassword(ctx, tenant.ID, boss.ID, hash, false); err != nil { // a real password, not a temporary one
		t.Fatal(err)
	}

	gwHost := a.api

	t.Run("auth config lists the provider", func(t *testing.T) {
		resp, body := newBrowser(t, gwHost).get(gw + "/api/v1/auth/config")
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"start_url":"/api/v1/auth/oidc/keycloak/start"`) {
			t.Fatalf("auth/config: %d %s", resp.StatusCode, body)
		}
	})

	t.Run("happy path", func(t *testing.T) {
		b := newBrowser(t, gwHost)
		resp, body := b.login(gw, "alice")
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `content="0;url=/connectors"`) {
			t.Fatalf("callback: HTTP %d %s (Location %q)", resp.StatusCode, body, resp.Header.Get("Location"))
		}
		var sawSession bool
		for _, c := range resp.Cookies() {
			if c.Name == "gw_session" && c.Value != "" {
				sawSession = true
				if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
					t.Errorf("gw_session = %+v, want HttpOnly SameSite=Strict", c)
				}
			}
		}
		if !sawSession {
			t.Fatal("callback set no gw_session cookie")
		}

		resp, body = b.get(gw + "/api/v1/auth/me")
		var me struct {
			Kind       string `json:"kind"`
			AuthMethod string `json:"auth_method"`
			TenantID   string `json:"tenant_id"`
			CSRF       string `json:"csrf_token"`
			MustChange bool   `json:"must_change_password"`
			User       struct {
				ID, Username, Role, Tenant string
			} `json:"user"`
		}
		if err := json.Unmarshal([]byte(body), &me); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("/auth/me: %d %s", resp.StatusCode, body)
		}
		if me.Kind != "user" || me.AuthMethod != "oidc" || me.TenantID != tenant.ID || me.User.ID != alice.ID ||
			me.User.Username != "alice@example.com" || me.User.Role != "admin" || me.User.Tenant != "sso-e2e" || me.MustChange || me.CSRF == "" {
			t.Fatalf("/auth/me = %s", body)
		}

		write := func(csrf string) (int, string) {
			req, _ := http.NewRequest(http.MethodPost, gw+"/api/v1/users",
				strings.NewReader(`{"username":"sso.only@example.com","role":"viewer","sso_only":true}`))
			req.Header.Set("Content-Type", "application/json")
			if csrf != "" {
				req.Header.Set("X-CSRF-Token", csrf)
			}
			resp, body := b.do(req)
			return resp.StatusCode, body
		}
		if code, body := write(""); code != http.StatusForbidden || !strings.Contains(body, "csrf_error") {
			t.Errorf("write without CSRF token: %d %s, want 403 csrf_error", code, body)
		}
		if code, body := write(me.CSRF); code != http.StatusCreated || !strings.Contains(body, `"sso_only":true`) || strings.Contains(body, "temporary_password") {
			t.Errorf("write with CSRF token (create an SSO-only user): %d %s, want 201 sso_only", code, body)
		}

		var method string
		if err := db.QueryRowContext(ctx, `SELECT auth_method FROM user_sessions WHERE user_id = $1 AND revoked_at IS NULL`, alice.ID).Scan(&method); err != nil || method != "oidc" {
			t.Errorf("session row auth_method = %q, %v", method, err)
		}
		var linked string
		if err := db.QueryRowContext(ctx, `SELECT user_id FROM user_identities WHERE tenant_id = $1 AND provider = 'keycloak' AND issuer = $2`, tenant.ID, issuer).Scan(&linked); err != nil || linked != alice.ID {
			t.Errorf("identity link user = %q, %v", linked, err)
		}
		var detail string
		if err := db.QueryRowContext(ctx, `SELECT detail FROM auth_audit WHERE tenant_id = $1 AND action = 'login_ok' ORDER BY id DESC LIMIT 1`, tenant.ID).Scan(&detail); err != nil || detail != "oidc:keycloak" {
			t.Errorf("login_ok detail = %q, %v", detail, err)
		}

		req, _ := http.NewRequest(http.MethodPost, gw+"/api/v1/auth/logout", nil)
		req.Header.Set("X-CSRF-Token", me.CSRF)
		if resp, body := b.do(req); resp.StatusCode != http.StatusNoContent {
			t.Errorf("logout: %d %s", resp.StatusCode, body)
		}
		if resp, _ := b.get(gw + "/api/v1/auth/me"); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("/auth/me after logout: %d, want 401", resp.StatusCode)
		}
	})

	t.Run("forged state is refused", func(t *testing.T) {
		b := newBrowser(t, gwHost)
		cb := b.toIdPCallback(gw, "alice")
		q := cb.Query()
		q.Set("state", "forged-"+q.Get("state"))
		cb.RawQuery = q.Encode()
		resp, _ := b.get(cb.String())
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login?sso_error=state" {
			t.Errorf("forged state: %d Location %q", resp.StatusCode, resp.Header.Get("Location"))
		}
	})

	for _, tc := range []struct{ name, kcUser, code string }{
		{"disallowed domain", "eve", "domain"},
		{"unknown user", "nobody", "no_user"},
		{"disabled user", "dan", "disabled"},
		{"platform-admin", "root", "role"},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			b := newBrowser(t, gwHost)
			resp, _ := b.login(gw, tc.kcUser)
			if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login?sso_error="+tc.code {
				t.Fatalf("callback: %d Location %q, want 302 to /login?sso_error=%s", resp.StatusCode, resp.Header.Get("Location"), tc.code)
			}
			for _, c := range resp.Cookies() {
				if c.Name == "gw_session" && c.Value != "" {
					t.Error("a refused login set a session cookie")
				}
			}
			var detail string
			if err := db.QueryRowContext(ctx, `SELECT detail FROM auth_audit WHERE tenant_id = $1 AND action = 'login_fail' ORDER BY id DESC LIMIT 1`, tenant.ID).Scan(&detail); err != nil || detail != "oidc:keycloak:"+tc.code {
				t.Errorf("login_fail detail = %q, %v", detail, err)
			}
		})
	}

	t.Run("unverified email does not link to a password user", func(t *testing.T) {
		b := newBrowser(t, gwHost)
		resp, _ := b.get(b.toIdPCallbackVia(gw, "kc-unverified", "boss@example.com").String())
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login?sso_error=link_required" {
			t.Fatalf("callback: %d Location %q, want sso_error=link_required", resp.StatusCode, resp.Header.Get("Location"))
		}
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM user_identities WHERE user_id = $1`, boss.ID).Scan(&n); err != nil || n != 0 {
			t.Errorf("identity links of the password admin = %d, %v; want 0", n, err)
		}
	})

	t.Run("unverified email links an SSO-only user, by email_claim", func(t *testing.T) {
		b := newBrowser(t, gwHost)
		resp, body := b.get(b.toIdPCallbackVia(gw, "kc-unverified", "sso.only@example.com").String())
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("callback: %d Location %q %s", resp.StatusCode, resp.Header.Get("Location"), body)
		}
		resp, body = b.get(gw + "/api/v1/auth/me")
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"username":"sso.only@example.com"`) || !strings.Contains(body, `"auth_method":"oidc"`) {
			t.Errorf("/auth/me: %d %s", resp.StatusCode, body)
		}
	})

	t.Run("forged callbacks do not lock the browser out", func(t *testing.T) {
		b := newBrowser(t, gwHost)
		for i := 0; i < 25; i++ {
			// What an <img> on another site can make the browser send: no
			// login-state cookie, a made-up state. Paced under the general
			// per-IP request cap (auth.rate_limit.requests_per_minute, a
			// token bucket every route shares), so a 429 here could only
			// come from the failure lockout under test.
			time.Sleep(120 * time.Millisecond)
			resp, _ := b.get(gw + "/api/v1/auth/oidc/keycloak/callback?code=x&state=forged")
			if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login?sso_error=state" {
				t.Fatalf("forged callback %d: %d %q", i, resp.StatusCode, resp.Header.Get("Location"))
			}
		}
		if resp, _ := b.get(gw + "/api/v1/auth/config"); resp.StatusCode != http.StatusOK {
			t.Fatalf("/auth/config after 25 forged callbacks: %d, want 200 (not locked out)", resp.StatusCode)
		}
		resp, _ := b.login(gw, "alice")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("real login after forged callbacks: %d %q", resp.StatusCode, resp.Header.Get("Location"))
		}
	})
}
