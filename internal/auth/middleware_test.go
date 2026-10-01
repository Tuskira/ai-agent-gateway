package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// fakeAuthorizer is a scripted pkg/auth.Authorizer for testing
// RequirePermission.
type fakeAuthorizer struct {
	allow bool
	gotP  *pkgauth.Principal
}

func (f *fakeAuthorizer) Allow(_ context.Context, p *pkgauth.Principal, _ string) bool {
	f.gotP = p
	return f.allow
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func decodeErrorBody(t *testing.T, w *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", w.Body.String(), err)
	}
	return body
}

func TestMiddleware_Success(t *testing.T) {
	principal := &pkgauth.Principal{Subject: "u1", TenantID: "t1", KeyID: "k1"}
	a := &fakeAuthenticator{name: "fake", principal: principal}

	var gotPrincipal *pkgauth.Principal
	var gotLoggerPresent bool
	handler := Middleware(a, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotPrincipal, _ = pkgauth.PrincipalFrom(r.Context())
		gotLoggerPresent = applog.From(r.Context()) != nil
	}))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusOK && w.Code != 0 {
		// handler doesn't write a status; default is 200 once anything writes,
		// but since it writes nothing, recorder defaults to 200.
	}
	if gotPrincipal != principal {
		t.Errorf("PrincipalFrom(ctx) = %+v, want %+v", gotPrincipal, principal)
	}
	if !gotLoggerPresent {
		t.Error("expected a request-scoped logger to be present in context")
	}
}

func TestMiddleware_NoCredential401(t *testing.T) {
	a := &fakeAuthenticator{name: "fake", err: pkgauth.ErrNoCredential}
	handler := Middleware(a, nil)(okHandler())

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want Bearer", got)
	}
	body := decodeErrorBody(t, w)
	if body.Error.Type != "authentication_error" {
		t.Errorf("error.type = %q, want authentication_error", body.Error.Type)
	}
	if body.Error.Message == "" {
		t.Error("error.message is empty")
	}
}

func TestMiddleware_InvalidCredential401(t *testing.T) {
	a := &fakeAuthenticator{name: "fake", err: pkgauth.ErrInvalid}
	handler := Middleware(a, nil)(okHandler())

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	body := decodeErrorBody(t, w)
	if body.Error.Type != "authentication_error" {
		t.Errorf("error.type = %q, want authentication_error", body.Error.Type)
	}
	// The body must never leak *why* the credential was invalid.
	if body.Error.Message == "auth: invalid credential" {
		t.Error("error.message leaked the raw sentinel error")
	}
}

func TestMiddleware_DoesNotCallNextOnFailure(t *testing.T) {
	a := &fakeAuthenticator{name: "fake", err: pkgauth.ErrNoCredential}
	called := false
	handler := Middleware(a, nil)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if called {
		t.Error("next handler was called despite authentication failure")
	}
}

func TestRequirePermission_Allowed(t *testing.T) {
	principal := &pkgauth.Principal{Subject: "u1"}
	authz := &fakeAuthorizer{allow: true}
	called := false

	handler := RequirePermission(authz, "connector.read")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(pkgauth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if !called {
		t.Error("next handler was not called despite Allow=true")
	}
	if authz.gotP != principal {
		t.Errorf("Allow() principal = %+v, want %+v", authz.gotP, principal)
	}
}

func TestRequirePermission_Denied403(t *testing.T) {
	principal := &pkgauth.Principal{Subject: "u1"}
	authz := &fakeAuthorizer{allow: false}
	called := false

	handler := RequirePermission(authz, "connector.create")(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(pkgauth.WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if called {
		t.Error("next handler was called despite Allow=false")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	body := decodeErrorBody(t, w)
	if body.Error.Type != "permission_error" {
		t.Errorf("error.type = %q, want permission_error", body.Error.Type)
	}
}

func TestRequirePermission_NoPrincipalDenied(t *testing.T) {
	authz := &fakeAuthorizer{allow: false}
	handler := RequirePermission(authz, "connector.read")(okHandler())

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if authz.gotP != nil {
		t.Errorf("Allow() principal = %+v, want nil", authz.gotP)
	}
}

func TestRateLimitedAuthenticator_NoCredentialIsNotAFailure(t *testing.T) {
	rl := newTestRateLimiter(t, &manualClock{t: time.Now()}, nil) // locks after 3 failures
	none := RateLimitedAuthenticator(&fakeAuthenticator{name: "fake", err: pkgauth.ErrNoCredential}, rl)
	bad := RateLimitedAuthenticator(&fakeAuthenticator{name: "fake", err: pkgauth.ErrInvalid}, rl)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.5:1234"

	for i := 0; i < 10; i++ {
		_, _ = none.Authenticate(r.Context(), r)
	}
	if locked, _ := rl.Locked("203.0.113.5"); locked {
		t.Fatal("requests carrying no credential counted toward the IP lockout")
	}
	for i := 0; i < 3; i++ {
		_, _ = bad.Authenticate(r.Context(), r)
	}
	if locked, _ := rl.Locked("203.0.113.5"); !locked {
		t.Error("rejected credentials no longer trip the IP lockout")
	}
}
