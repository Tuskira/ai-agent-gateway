package oidc_test

import (
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/oidc"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/oidc/oidctest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard/netguardtest"
)

func init() { netguardtest.AllowLoopback() }

func masterKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func providerConfig(iss *oidctest.Issuer, mutate ...func(*config.OIDCProvider)) config.OIDCProvider {
	c := config.OIDCProvider{
		Name: "corp", DisplayName: "Corp SSO", Issuer: iss.URL, ClientID: iss.ClientID,
		ClientSecretEnv: "TEST_OIDC_SECRET", RedirectURL: "https://console.example.com/api/v1/auth/oidc/corp/callback",
		Tenant: "default", AllowedEmailDomains: []string{"example.com"},
	}
	for _, m := range mutate {
		m(&c)
	}
	return c
}

type rig struct {
	iss  *oidctest.Issuer
	svc  *oidc.Service
	p    *oidc.Provider
	now  time.Time
	opts oidc.Options
}

func newRig(t *testing.T, mutate ...func(*config.OIDCProvider)) *rig {
	t.Helper()
	iss := oidctest.New(t)
	r := &rig{iss: iss, now: time.Now()}
	r.opts = oidc.Options{
		Now:    func() time.Time { return r.now },
		Getenv: func(k string) string { return map[string]string{"TEST_OIDC_SECRET": iss.ClientSecret}[k] },
	}
	svc, err := oidc.New([]config.OIDCProvider{providerConfig(iss, mutate...)}, masterKey(t), r.opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.svc = svc
	r.p, _ = svc.Provider("corp")
	return r
}

// begin starts a login from a request to host and returns the authorize
// URL and the state cookie.
func (r *rig) begin(t *testing.T, host, returnTo string) (string, *http.Cookie) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+oidc.StartPath("corp"), nil)
	authURL, c, err := r.svc.Begin(r.p, req, returnTo)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return authURL, c
}

func callback(q url.Values, cookies ...*http.Cookie) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "https://console.example.com/api/v1/auth/oidc/corp/callback?"+q.Encode(), nil)
	for _, c := range cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	return req
}

// login runs a whole login and returns Finish's answer.
func (r *rig) login(t *testing.T, sub, email string) (*oidc.Result, error) {
	t.Helper()
	authURL, c := r.begin(t, "console.example.com", "/connectors")
	q := r.iss.Authorize(t, authURL, sub, email)
	return r.svc.Finish(r.p, callback(q, c))
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var oe *oidc.Error
	if !errors.As(err, &oe) {
		t.Fatalf("error = %v, want an *oidc.Error with code %q", err, code)
	}
	if oe.Code != code {
		t.Fatalf("code = %q (%v), want %q", oe.Code, oe.Err, code)
	}
}

func TestNewRejectsMissingSecretAndShortKey(t *testing.T) {
	iss := oidctest.New(t)
	if _, err := oidc.New([]config.OIDCProvider{providerConfig(iss)}, masterKey(t), oidc.Options{Getenv: func(string) string { return "" }}); err == nil {
		t.Error("New with an empty client secret succeeded")
	}
	if _, err := oidc.New(nil, []byte("short"), oidc.Options{}); err == nil {
		t.Error("New with a short master key succeeded")
	}
	file := providerConfig(iss, func(c *config.OIDCProvider) { c.ClientSecretEnv, c.ClientSecretFile = "", "/nope/secret" })
	if _, err := oidc.New([]config.OIDCProvider{file}, masterKey(t), oidc.Options{}); err == nil {
		t.Error("New with an unreadable client_secret_file succeeded")
	}
}

func TestBeginAuthorizeRequestAndCookie(t *testing.T) {
	r := newRig(t, func(c *config.OIDCProvider) {
		c.AuthParams = map[string]string{"identity_provider": "EntraID"}
	})
	authURL, c := r.begin(t, "console.example.com", "/connectors?x=1")
	u, _ := url.Parse(authURL)
	q := u.Query()
	for k, want := range map[string]string{
		"response_type": "code", "client_id": r.iss.ClientID, "code_challenge_method": "S256",
		"redirect_uri":      "https://console.example.com/api/v1/auth/oidc/corp/callback",
		"scope":             "openid email profile",
		"identity_provider": "EntraID",
	} {
		if q.Get(k) != want {
			t.Errorf("authorize %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if len(q.Get("state")) < 40 || len(q.Get("nonce")) < 40 || len(q.Get("code_challenge")) != 43 {
		t.Errorf("state/nonce/code_challenge too short: %q %q %q", q.Get("state"), q.Get("nonce"), q.Get("code_challenge"))
	}
	if q.Get("code_verifier") != "" {
		t.Error("the PKCE verifier leaked into the authorize URL")
	}
	if c.Name != oidc.StateCookieSecure || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode ||
		c.Path != "/" || c.Domain != "" || c.MaxAge != 600 {
		t.Errorf("https state cookie = %+v, want __Host-gw_oidc, Secure, HttpOnly, Lax, Path=/, no Domain, 600s", c)
	}
	for _, leak := range []string{q.Get("state"), q.Get("nonce"), "/connectors"} {
		if strings.Contains(c.Value, leak) {
			t.Errorf("state cookie value carries %q in the clear", leak)
		}
	}
	// Two logins never share state or nonce.
	authURL2, _ := r.begin(t, "console.example.com", "/")
	u2, _ := url.Parse(authURL2)
	if u2.Query().Get("state") == q.Get("state") || u2.Query().Get("nonce") == q.Get("nonce") {
		t.Error("state or nonce repeated across logins")
	}
}

func TestBeginPlainHTTPLoopbackCookie(t *testing.T) {
	r := newRig(t, func(c *config.OIDCProvider) {
		c.RedirectURL = "http://localhost:18081/api/v1/auth/oidc/corp/callback"
	})
	_, c := r.begin(t, "localhost:18081", "/")
	if c.Name != oidc.StateCookiePlain || c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("http state cookie = %+v, want gw_oidc, not Secure, HttpOnly, Lax", c)
	}
}

func TestBeginPicksRedirectByHost(t *testing.T) {
	r := newRig(t, func(c *config.OIDCProvider) {
		c.RedirectURLs = []string{"https://other.example.com/api/v1/auth/oidc/corp/callback"}
	})
	for host, want := range map[string]string{
		"other.example.com":   "https://other.example.com/api/v1/auth/oidc/corp/callback",
		"console.example.com": "https://console.example.com/api/v1/auth/oidc/corp/callback",
		"evil.example.net":    "https://console.example.com/api/v1/auth/oidc/corp/callback", // unknown host: the first one
	} {
		authURL, _ := r.begin(t, host, "/")
		u, _ := url.Parse(authURL)
		if got := u.Query().Get("redirect_uri"); got != want {
			t.Errorf("host %s: redirect_uri = %s, want %s", host, got, want)
		}
	}
	// Behind a proxy the forwarded host decides.
	req := httptest.NewRequest(http.MethodGet, "http://gateway.internal:8081"+oidc.StartPath("corp"), nil)
	req.Header.Set("X-Forwarded-Host", "other.example.com")
	authURL, _, err := r.svc.Begin(r.p, req, "/")
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := url.Parse(authURL); u.Query().Get("redirect_uri") != "https://other.example.com/api/v1/auth/oidc/corp/callback" {
		t.Errorf("forwarded host ignored: %s", u.Query().Get("redirect_uri"))
	}
}

func TestFinishHappyPath(t *testing.T) {
	r := newRig(t)
	res, err := r.login(t, "sub-1", "Alice@Example.COM")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	id := res.Identity
	if id.Subject != "sub-1" || id.Email != "alice@example.com" || id.Issuer != r.iss.URL || id.Provider != "corp" || !id.EmailVerified {
		t.Errorf("identity = %+v", id)
	}
	if res.ReturnTo != "/connectors" {
		t.Errorf("ReturnTo = %q, want /connectors", res.ReturnTo)
	}
}

func TestFinishStateChecks(t *testing.T) {
	r := newRig(t)

	t.Run("no cookie", func(t *testing.T) {
		authURL, _ := r.begin(t, "console.example.com", "/")
		q := r.iss.Authorize(t, authURL, "s", "a@example.com")
		_, err := r.svc.Finish(r.p, callback(q))
		wantCode(t, err, oidc.CodeState)
	})
	t.Run("forged state parameter", func(t *testing.T) {
		authURL, c := r.begin(t, "console.example.com", "/")
		q := r.iss.Authorize(t, authURL, "s", "a@example.com")
		q.Set("state", q.Get("state")+"x")
		_, err := r.svc.Finish(r.p, callback(q, c))
		wantCode(t, err, oidc.CodeState)
	})
	t.Run("another login's cookie", func(t *testing.T) {
		authURL, _ := r.begin(t, "console.example.com", "/")
		_, other := r.begin(t, "console.example.com", "/")
		q := r.iss.Authorize(t, authURL, "s", "a@example.com")
		_, err := r.svc.Finish(r.p, callback(q, other))
		wantCode(t, err, oidc.CodeState)
	})
	t.Run("tampered cookie", func(t *testing.T) {
		authURL, c := r.begin(t, "console.example.com", "/")
		q := r.iss.Authorize(t, authURL, "s", "a@example.com")
		b := []byte(c.Value)
		b[len(b)/2] ^= 1
		if b[len(b)/2] == c.Value[len(b)/2] {
			b[len(b)/2] = 'A'
		}
		_, err := r.svc.Finish(r.p, callback(q, &http.Cookie{Name: c.Name, Value: string(b)}))
		wantCode(t, err, oidc.CodeState)
	})
	t.Run("https state in the plain cookie name", func(t *testing.T) {
		authURL, c := r.begin(t, "console.example.com", "/")
		q := r.iss.Authorize(t, authURL, "s", "a@example.com")
		_, err := r.svc.Finish(r.p, callback(q, &http.Cookie{Name: oidc.StateCookiePlain, Value: c.Value}))
		wantCode(t, err, oidc.CodeState)
	})
	t.Run("expired", func(t *testing.T) {
		authURL, c := r.begin(t, "console.example.com", "/")
		q := r.iss.Authorize(t, authURL, "s", "a@example.com")
		r.now = r.now.Add(oidc.StateLifetime + time.Second)
		defer func() { r.now = time.Now() }()
		_, err := r.svc.Finish(r.p, callback(q, c))
		wantCode(t, err, oidc.CodeState)
	})
	t.Run("another master key", func(t *testing.T) {
		other, err := oidc.New([]config.OIDCProvider{providerConfig(r.iss)}, masterKey(t), r.opts)
		if err != nil {
			t.Fatal(err)
		}
		op, _ := other.Provider("corp")
		authURL, c := r.begin(t, "console.example.com", "/")
		q := r.iss.Authorize(t, authURL, "s", "a@example.com")
		_, err = other.Finish(op, callback(q, c))
		wantCode(t, err, oidc.CodeState)
	})
	t.Run("idp error", func(t *testing.T) {
		authURL, c := r.begin(t, "console.example.com", "/")
		q := r.iss.Authorize(t, authURL, "s", "a@example.com")
		q.Del("code")
		q.Set("error", "access_denied")
		_, err := r.svc.Finish(r.p, callback(q, c))
		wantCode(t, err, oidc.CodeProvider)
	})
	t.Run("no code", func(t *testing.T) {
		authURL, c := r.begin(t, "console.example.com", "/")
		q := r.iss.Authorize(t, authURL, "s", "a@example.com")
		q.Del("code")
		_, err := r.svc.Finish(r.p, callback(q, c))
		wantCode(t, err, oidc.CodeToken)
	})
	t.Run("code replay", func(t *testing.T) {
		authURL, c := r.begin(t, "console.example.com", "/")
		q := r.iss.Authorize(t, authURL, "s", "a@example.com")
		if _, err := r.svc.Finish(r.p, callback(q, c)); err != nil {
			t.Fatal(err)
		}
		_, err := r.svc.Finish(r.p, callback(q, c))
		wantCode(t, err, oidc.CodeToken)
	})
}

func TestFinishProviderMismatch(t *testing.T) {
	iss := oidctest.New(t)
	env := func(string) string { return iss.ClientSecret }
	two := providerConfig(iss, func(c *config.OIDCProvider) {
		c.Name = "two"
		c.RedirectURL = "https://console.example.com/api/v1/auth/oidc/two/callback"
	})
	svc, err := oidc.New([]config.OIDCProvider{providerConfig(iss), two}, masterKey(t), oidc.Options{Getenv: env})
	if err != nil {
		t.Fatal(err)
	}
	corp, _ := svc.Provider("corp")
	p2, _ := svc.Provider("two")
	authURL, c, err := svc.Begin(corp, httptest.NewRequest(http.MethodGet, "https://console.example.com/", nil), "/")
	if err != nil {
		t.Fatal(err)
	}
	q := iss.Authorize(t, authURL, "s", "a@example.com")
	_, err = svc.Finish(p2, callback(q, c))
	wantCode(t, err, oidc.CodeState)
}

func TestFinishTokenChecks(t *testing.T) {
	cases := []struct {
		name  string
		alg   string
		edit  func(c map[string]any)
		rig   func(*testing.T, *rig)
		code  string
		after func(t *testing.T, r *rig)
	}{
		{name: "wrong audience", edit: func(c map[string]any) { c["aud"] = "someone-else" }, code: oidc.CodeToken},
		{name: "wrong issuer", edit: func(c map[string]any) { c["iss"] = "https://evil.example.net" }, code: oidc.CodeToken},
		{name: "expired", edit: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, code: oidc.CodeToken},
		{name: "iat in the future", edit: func(c map[string]any) { c["iat"] = time.Now().Add(time.Hour).Unix() }, code: oidc.CodeToken},
		{name: "no iat", edit: func(c map[string]any) { delete(c, "iat") }, code: oidc.CodeToken},
		{name: "wrong nonce", edit: func(c map[string]any) { c["nonce"] = "replayed" }, code: oidc.CodeToken},
		{name: "no nonce", edit: func(c map[string]any) { delete(c, "nonce") }, code: oidc.CodeToken},
		{name: "access token use", edit: func(c map[string]any) { c["token_use"] = "access" }, code: oidc.CodeToken},
		{name: "id token use", edit: func(c map[string]any) { c["token_use"] = "id" }},
		{name: "several audiences without azp", edit: func(c map[string]any) { c["aud"] = []string{"gateway-test", "other"} }, code: oidc.CodeToken},
		{name: "several audiences with azp", edit: func(c map[string]any) {
			c["aud"] = []string{"gateway-test", "other"}
			c["azp"] = "gateway-test"
		}},
		{name: "HS256 signed with the client secret", alg: "HS256", code: oidc.CodeToken},
		{name: "unsigned", alg: "none", code: oidc.CodeToken},
		{name: "no email", edit: func(c map[string]any) { delete(c, "email") }, code: oidc.CodeDomain},
		{name: "email_verified false", edit: func(c map[string]any) { c["email_verified"] = false }, code: oidc.CodeUnverified},
		{name: "email_verified missing", edit: func(c map[string]any) { delete(c, "email_verified") }, code: oidc.CodeUnverified},
		{name: "email_verified string true", edit: func(c map[string]any) { c["email_verified"] = "true" }},
		{name: "email_verified string false", edit: func(c map[string]any) { c["email_verified"] = "false" }, code: oidc.CodeUnverified},
		{name: "subdomain", edit: func(c map[string]any) { c["email"] = "a@eu.example.com" }, code: oidc.CodeDomain},
		{name: "rotated signing key (unknown kid refetches JWKS)", rig: func(t *testing.T, r *rig) {
			if _, err := r.login(t, "warm", "a@example.com"); err != nil {
				t.Fatal(err)
			}
			r.iss.RotateKey()
		}, after: func(t *testing.T, r *rig) {
			if r.iss.JWKSHits < 2 {
				t.Errorf("JWKS fetched %d times, want a refetch after rotation", r.iss.JWKSHits)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			if tc.rig != nil {
				tc.rig(t, r)
			}
			r.iss.Claims, r.iss.Alg = tc.edit, tc.alg
			_, err := r.login(t, "sub-1", "a@example.com")
			if tc.code == "" {
				if err != nil {
					t.Fatalf("Finish: %v", err)
				}
			} else {
				wantCode(t, err, tc.code)
			}
			if tc.after != nil {
				tc.after(t, r)
			}
		})
	}
}

func TestFinishUpstreamIdP(t *testing.T) {
	cases := []struct {
		name       string
		identities any
		code       string
	}{
		{"array", []map[string]any{{"providerName": "EntraID", "userId": "x"}}, ""},
		{"json string", `[{"providerName":"EntraID","userId":"x"}]`, ""},
		{"missing", nil, oidc.CodeIdP},
		{"empty array", []map[string]any{}, oidc.CodeIdP},
		{"other provider", []map[string]any{{"providerName": "Google"}}, oidc.CodeIdP},
		{"one of several", []map[string]any{{"providerName": "EntraID"}, {"providerName": "Google"}}, oidc.CodeIdP},
		{"case differs", []map[string]any{{"providerName": "entraid"}}, oidc.CodeIdP},
		{"garbage string", "not json", oidc.CodeIdP},
		{"number", 7, oidc.CodeIdP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, func(c *config.OIDCProvider) {
				c.RequiredUpstreamIdP = "EntraID"
				f := false
				c.RequireVerifiedEmail = &f
			})
			r.iss.Claims = func(c map[string]any) {
				c["email_verified"] = false // federated users often have it false
				if tc.identities != nil {
					c["identities"] = tc.identities
				}
			}
			res, err := r.login(t, "sub-1", "a@example.com")
			if tc.code == "" {
				if err != nil {
					t.Fatalf("Finish: %v", err)
				}
				if res.Identity.EmailVerified {
					t.Error("EmailVerified = true for email_verified false")
				}
				return
			}
			wantCode(t, err, tc.code)
		})
	}
}

func TestNormalizeEmail(t *testing.T) {
	allowed := map[string]bool{"example.com": true}
	for in, want := range map[string]string{
		"alice@example.com":     "alice@example.com",
		"Alice.B+x@EXAMPLE.com": "alice.b+x@example.com",
	} {
		got, err := oidc.NormalizeEmail(in, allowed)
		if err != nil || got != want {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "alice", "@example.com", "alice@", "a@b@example.com", "alice@example.com.", "alice@sub.example.com",
		"alice@example.co", "alice@exampleXcom", " alice@example.com", "alice@example.com\n", "al ice@example.com",
		"alice@exämple.com", "alice@example.com\x00", strings.Repeat("a", 250) + "@example.com",
	} {
		if got, err := oidc.NormalizeEmail(in, allowed); err == nil {
			t.Errorf("NormalizeEmail(%q) = %q, want an error", in, got)
		}
	}
}

func TestCleanReturnTo(t *testing.T) {
	for in, want := range map[string]string{
		"":                    "/",
		"/":                   "/",
		"/connectors":         "/connectors",
		"/a/b?x=1&y=%2F#frag": "/a/b?x=1&y=%2F#frag",
	} {
		got, ok := oidc.CleanReturnTo(in)
		if !ok || got != want {
			t.Errorf("CleanReturnTo(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"//evil.example.net", "/\\evil.example.net", "\\\\evil", "https://evil.example.net", "javascript:alert(1)",
		"connectors", "/a\\b", "/a b", "/a\tb", "/a\nb", "/a\x00", "/ä", "/" + strings.Repeat("a", 2048),
		" /x", "http:/x",
	} {
		if got, ok := oidc.CleanReturnTo(in); ok {
			t.Errorf("CleanReturnTo(%q) = %q, want refused", in, got)
		}
	}
}

func TestWriteCompletePage(t *testing.T) {
	w := httptest.NewRecorder()
	oidc.WriteCompletePage(w, `/x?a="><script>alert(1)</script>&b='`)
	body := w.Body.String()
	if strings.Contains(body, "<script>") || strings.Contains(body, `"><script`) {
		t.Errorf("return_to not escaped: %s", body)
	}
	if !strings.Contains(body, `content="0;url=/x?a=&#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;&amp;b=&#39;"`) {
		t.Errorf("meta refresh missing or wrongly escaped: %s", body)
	}
	h := w.Header()
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
		t.Errorf("CSP = %q, want default-src 'none' and no script", csp)
	}
	if h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" || !strings.HasPrefix(h.Get("Content-Type"), "text/html") {
		t.Errorf("headers = %v", h)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestLoginErrorPath(t *testing.T) {
	if got := oidc.LoginErrorPath(oidc.CodeNoUser); got != "/login?sso_error=no_user" {
		t.Errorf("LoginErrorPath = %q", got)
	}
}

func TestDiscoveryFailureRetries(t *testing.T) {
	iss := oidctest.New(t)
	c := providerConfig(iss, func(c *config.OIDCProvider) { c.Issuer = "http://127.0.0.1:1" })
	now := time.Now()
	svc, err := oidc.New([]config.OIDCProvider{c}, masterKey(t), oidc.Options{
		Getenv: func(string) string { return "x" }, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := svc.Provider("corp")
	_, _, err = svc.Begin(p, httptest.NewRequest(http.MethodGet, "https://console.example.com/", nil), "/")
	wantCode(t, err, oidc.CodeProvider)
}
