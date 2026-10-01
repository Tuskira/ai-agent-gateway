package session_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func TestNewToken(t *testing.T) {
	raw, id, err := session.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 43 { // 32 bytes, unpadded base64url
		t.Errorf("raw token length = %d, want 43", len(raw))
	}
	if id != session.HashToken(raw) || id == raw || len(id) != 64 {
		t.Errorf("id = %q, want the 64-char hex sha256 of the raw token", id)
	}
	raw2, _, _ := session.NewToken()
	if raw == raw2 {
		t.Error("two tokens are identical")
	}
}

func TestCheckUsable(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	idle := 8 * time.Hour
	revoked := now.Add(-time.Minute)

	cases := []struct {
		name string
		s    store.UserSession
		want error
	}{
		{"fresh", store.UserSession{LastSeenAt: now, ExpiresAt: now.Add(24 * time.Hour)}, nil},
		{"idle just under the limit", store.UserSession{LastSeenAt: now.Add(-idle + time.Second), ExpiresAt: now.Add(time.Hour)}, nil},
		{"idle exactly at the limit", store.UserSession{LastSeenAt: now.Add(-idle), ExpiresAt: now.Add(time.Hour)}, session.ErrIdle},
		{"idle past the limit", store.UserSession{LastSeenAt: now.Add(-9 * time.Hour), ExpiresAt: now.Add(time.Hour)}, session.ErrIdle},
		{"absolute expiry reached", store.UserSession{LastSeenAt: now, ExpiresAt: now}, session.ErrExpired},
		{"absolute expiry passed even though active", store.UserSession{LastSeenAt: now, ExpiresAt: now.Add(-time.Second)}, session.ErrExpired},
		{"revoked", store.UserSession{LastSeenAt: now, ExpiresAt: now.Add(time.Hour), RevokedAt: &revoked}, session.ErrRevoked},
		{"revoked wins over expired", store.UserSession{LastSeenAt: now, ExpiresAt: now.Add(-time.Hour), RevokedAt: &revoked}, session.ErrRevoked},
	}
	for _, c := range cases {
		if got := session.CheckUsable(&c.s, now, idle); !errors.Is(got, c.want) {
			t.Errorf("%s: CheckUsable = %v, want %v", c.name, got, c.want)
		}
	}
}

// fixture builds an Authenticator over the in-memory store with one user
// and one live session, returning the raw cookie token.
type fixture struct {
	st    *dptest.Store
	auth  *session.Authenticator
	user  *store.User
	sess  *store.UserSession
	raw   string
	clock *time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st := dptest.New()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	f := &fixture{st: st, clock: &now}

	f.user = &store.User{TenantID: "tenant-1", Username: "alice@example.com", Role: store.UserRoleAdmin, PasswordHash: "x"}
	if err := st.Users().Create(ctx, f.user); err != nil {
		t.Fatal(err)
	}
	raw, id, _ := session.NewToken()
	f.raw = raw
	f.sess = &store.UserSession{ID: id, UserID: f.user.ID, TenantID: "tenant-1", CSRFToken: "csrf-abc", ExpiresAt: now.Add(24 * time.Hour)}
	if err := st.UserSessions().Create(ctx, f.sess); err != nil {
		t.Fatal(err)
	}
	// dptest stamps LastSeenAt with the real clock; pin it to the fake one.
	if err := st.UserSessions().Touch(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	f.auth = session.New(st.Users(), st.UserSessions(), session.Options{Idle: 8 * time.Hour, Now: func() time.Time { return *f.clock }})
	return f
}

func (f *fixture) req(cookie string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: session.CookieName, Value: cookie})
	}
	return r
}

func TestAuthenticate_Success(t *testing.T) {
	f := newFixture(t)
	p, err := f.auth.Authenticate(context.Background(), f.req(f.raw))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Subject != f.user.ID || p.TenantID != "tenant-1" || len(p.Roles) != 1 || p.Roles[0] != "admin" ||
		p.AuthMethod != auth.AuthMethodSession || !p.IsUser() || p.Username != "alice@example.com" ||
		p.Email != "alice@example.com" || p.SessionID != f.sess.ID || p.CSRFToken != "csrf-abc" || p.MustChangePassword {
		t.Errorf("principal = %+v", p)
	}
}

func TestAuthenticate_NoCookieIsNoCredential(t *testing.T) {
	f := newFixture(t)
	if _, err := f.auth.Authenticate(context.Background(), f.req("")); !errors.Is(err, auth.ErrNoCredential) {
		t.Errorf("error = %v, want ErrNoCredential", err)
	}
}

func TestAuthenticate_Rejections(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		mutate func(f *fixture)
		cookie func(f *fixture) string
	}{
		{"unknown token", func(*fixture) {}, func(*fixture) string { return "not-a-real-token" }},
		{"revoked", func(f *fixture) { _ = f.st.UserSessions().Revoke(ctx, f.sess.ID) }, nil},
		{"absolute expiry", func(f *fixture) { *f.clock = f.clock.Add(25 * time.Hour) }, nil},
		{"idle timeout", func(f *fixture) { *f.clock = f.clock.Add(9 * time.Hour) }, nil},
		{"user disabled", func(f *fixture) {
			f.user.Disabled = true
			_ = f.st.Users().Update(ctx, f.user)
		}, nil},
		{"user deleted", func(f *fixture) { _ = f.st.Users().SoftDelete(ctx, f.user.TenantID, f.user.ID) }, nil},
	}
	for _, tc := range tests {
		f := newFixture(t)
		tc.mutate(f)
		cookie := f.raw
		if tc.cookie != nil {
			cookie = tc.cookie(f)
		}
		if _, err := f.auth.Authenticate(ctx, f.req(cookie)); !errors.Is(err, auth.ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", tc.name, err)
		}
	}
}

func TestAuthenticate_TouchThrottledToOncePerMinute(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seen := func() time.Time {
		s, _ := f.st.UserSessions().Get(ctx, f.sess.ID)
		return s.LastSeenAt
	}
	base := *f.clock

	*f.clock = base.Add(30 * time.Second)
	if _, err := f.auth.Authenticate(ctx, f.req(f.raw)); err != nil {
		t.Fatal(err)
	}
	if got := seen(); !got.Equal(base) {
		t.Errorf("a request 30s after the last touch moved last_seen_at to %v; want no write", got)
	}

	*f.clock = base.Add(61 * time.Second)
	if _, err := f.auth.Authenticate(ctx, f.req(f.raw)); err != nil {
		t.Fatal(err)
	}
	if got := seen(); !got.Equal(base.Add(61 * time.Second)) {
		t.Errorf("last_seen_at = %v, want %v after a request 61s in", got, base.Add(61*time.Second))
	}

	// A touch slides the idle window: 7h later is fine, and so is 7h after that.
	*f.clock = base.Add(61*time.Second + 7*time.Hour)
	if _, err := f.auth.Authenticate(ctx, f.req(f.raw)); err != nil {
		t.Errorf("session active within idle window rejected: %v", err)
	}
}

func TestAuthenticate_MustChangePasswordFlag(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_ = f.st.Users().SetPassword(ctx, f.user.TenantID, f.user.ID, "h", true)
	p, err := f.auth.Authenticate(ctx, f.req(f.raw))
	if err != nil || !p.MustChangePassword {
		t.Errorf("principal = %+v, err %v; want MustChangePassword", p, err)
	}
}

func TestSecureCookie(t *testing.T) {
	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	fwd := httptest.NewRequest(http.MethodGet, "/", nil)
	fwd.Header.Set("X-Forwarded-Proto", "https")
	fwdList := httptest.NewRequest(http.MethodGet, "/", nil)
	fwdList.Header.Set("X-Forwarded-Proto", "HTTPS, http")
	fwdHTTP := httptest.NewRequest(http.MethodGet, "/", nil)
	fwdHTTP.Header.Set("X-Forwarded-Proto", "http")
	tlsReq := httptest.NewRequest(http.MethodGet, "https://gw.example/", nil)

	cases := []struct {
		name string
		r    *http.Request
		mode string
		want bool
	}{
		{"auto plain http", plain, "auto", false},
		{"auto tls", tlsReq, "auto", true},
		{"auto forwarded https", fwd, "auto", true},
		{"auto forwarded https first of list", fwdList, "auto", true},
		{"auto forwarded http", fwdHTTP, "auto", false},
		{"true always", plain, "true", true},
		{"false never, even over tls", tlsReq, "false", false},
		{"empty mode behaves like auto", fwd, "", true},
	}
	for _, c := range cases {
		if got := session.SecureCookie(c.r, c.mode); got != c.want {
			t.Errorf("%s: SecureCookie = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSetAndClearCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")

	w := httptest.NewRecorder()
	session.SetCookie(w, r, "tok", "auto", 24*time.Hour)
	c := w.Result().Cookies()[0]
	if c.Name != "gw_session" || c.Value != "tok" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge != 86400 {
		t.Errorf("cookie = %+v", c)
	}

	w = httptest.NewRecorder()
	session.ClearCookie(w, r, "auto")
	c = w.Result().Cookies()[0]
	if c.Name != "gw_session" || c.Value != "" || c.MaxAge >= 0 || !c.HttpOnly {
		t.Errorf("clearing cookie = %+v, want an expired gw_session", c)
	}
}

// ---------------------------------------------------------------------------
// middleware
// ---------------------------------------------------------------------------

func withPrincipal(r *http.Request, p *auth.Principal) *http.Request {
	return r.WithContext(auth.WithPrincipal(r.Context(), p))
}

func userPrincipal(mustChange bool) *auth.Principal {
	return &auth.Principal{Subject: "u1", TenantID: "t1", Roles: []string{"admin"}, AuthMethod: auth.AuthMethodSession, CSRFToken: "tok-123", MustChangePassword: mustChange}
}

func TestCSRF(t *testing.T) {
	apiKey := &auth.Principal{Subject: "k1", TenantID: "t1", Roles: []string{"admin"}, AuthMethod: "apikey"}
	cases := []struct {
		name   string
		method string
		p      *auth.Principal
		header string
		want   int
	}{
		{"session POST with matching token", http.MethodPost, userPrincipal(false), "tok-123", http.StatusOK},
		{"session POST without token", http.MethodPost, userPrincipal(false), "", http.StatusForbidden},
		{"session POST with wrong token", http.MethodPost, userPrincipal(false), "tok-124", http.StatusForbidden},
		{"session POST with a prefix of the token", http.MethodPost, userPrincipal(false), "tok-12", http.StatusForbidden},
		{"session PATCH without token", http.MethodPatch, userPrincipal(false), "", http.StatusForbidden},
		{"session PUT without token", http.MethodPut, userPrincipal(false), "", http.StatusForbidden},
		{"session DELETE without token", http.MethodDelete, userPrincipal(false), "", http.StatusForbidden},
		{"session GET needs no token", http.MethodGet, userPrincipal(false), "", http.StatusOK},
		{"session HEAD needs no token", http.MethodHead, userPrincipal(false), "", http.StatusOK},
		{"api key POST is exempt", http.MethodPost, apiKey, "", http.StatusOK},
		{"api key DELETE is exempt", http.MethodDelete, apiKey, "", http.StatusOK},
		{"session with an empty stored token rejects an empty header", http.MethodPost, &auth.Principal{AuthMethod: auth.AuthMethodSession}, "", http.StatusForbidden},
	}
	h := session.CSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/api/v1/x", nil)
		if c.header != "" {
			r.Header.Set(session.CSRFHeader, c.header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, withPrincipal(r, c.p))
		if w.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, w.Code, c.want)
		}
		if c.want == http.StatusForbidden && !strings.Contains(w.Body.String(), `"type":"csrf_error"`) {
			t.Errorf("%s: body = %s, want type csrf_error", c.name, w.Body.String())
		}
	}
}

func TestMustChangeGate(t *testing.T) {
	allowed := func(r *http.Request) bool {
		return (r.Method == http.MethodGet && r.URL.Path == "/me") || (r.Method == http.MethodPost && r.URL.Path == "/password")
	}
	h := session.MustChangeGate(allowed)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	apiKey := &auth.Principal{Subject: "k1", AuthMethod: "apikey"}

	cases := []struct {
		name, method, path string
		p                  *auth.Principal
		want               int
	}{
		{"must-change user reaches an allowed route", http.MethodGet, "/me", userPrincipal(true), http.StatusOK},
		{"must-change user can change password", http.MethodPost, "/password", userPrincipal(true), http.StatusOK},
		{"must-change user blocked elsewhere", http.MethodGet, "/connectors", userPrincipal(true), http.StatusForbidden},
		{"allowed path with the wrong method is blocked", http.MethodPost, "/me", userPrincipal(true), http.StatusForbidden},
		{"normal user is not gated", http.MethodGet, "/connectors", userPrincipal(false), http.StatusOK},
		{"api key is never gated", http.MethodGet, "/connectors", apiKey, http.StatusOK},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, withPrincipal(httptest.NewRequest(c.method, c.path, nil), c.p))
		if w.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, w.Code, c.want)
		}
		if c.want == http.StatusForbidden && !strings.Contains(w.Body.String(), `"type":"password_change_required"`) {
			t.Errorf("%s: body = %s, want type password_change_required", c.name, w.Body.String())
		}
	}
}
