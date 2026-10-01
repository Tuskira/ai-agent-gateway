package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// fakeAuthenticator is a scripted pkg/auth.Authenticator for router tests.
type fakeAuthenticator struct {
	principal *pkgauth.Principal
	err       error
}

func (f *fakeAuthenticator) Name() string { return "fake" }

func (f *fakeAuthenticator) Authenticate(_ context.Context, _ *http.Request) (*pkgauth.Principal, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.principal, nil
}

func newTestRouter(auth pkgauth.Authenticator) http.Handler {
	return NewRouter(Deps{
		ServiceVersion: "1.2.3",
		Authenticator:  auth,
	})
}

func TestNewRouter_ServiceInfoNoAuth(t *testing.T) {
	h := newTestRouter(&fakeAuthenticator{err: pkgauth.ErrNoCredential})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["plane"] != "api" || body["version"] != "1.2.3" {
		t.Errorf("body = %+v", body)
	}
}

func TestNewRouter_RootNotFoundWhenUIDisabled(t *testing.T) {
	// ServeUI defaults to false in newTestRouter (Deps zero value); "/"
	// must not be registered at all in that mode.
	h := newTestRouter(&fakeAuthenticator{err: pkgauth.ErrNoCredential})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestNewRouter_UIMountedAtRootWhenServeUIEnabled(t *testing.T) {
	uiCalled := false
	uiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uiCalled = true
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<div id="root"></div>`))
	})

	h := NewRouter(Deps{
		ServiceVersion: "1.2.3",
		Authenticator:  &fakeAuthenticator{err: pkgauth.ErrNoCredential},
		ServeUI:        true,
		UIHandler:      uiHandler,
	})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !uiCalled {
		t.Error("UIHandler was not invoked for GET /")
	}

	// A deep client-side route also reaches the UI handler, not a 404 --
	// this is what makes SPA fallback routing work end to end.
	uiCalled = false
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil))
	if w.Code != http.StatusOK || !uiCalled {
		t.Fatalf("GET /login: status = %d, uiCalled = %v, want 200/true", w.Code, uiCalled)
	}

	// /api/v1/* must still take precedence over the UI mount.
	uiCalled = false
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if w.Code != http.StatusOK || uiCalled {
		t.Fatalf("GET /api/v1/health: status = %d, uiCalled = %v, want 200/false", w.Code, uiCalled)
	}
}

func TestNewRouter_UnknownAPIRouteIsJSON404(t *testing.T) {
	h := newTestRouter(&fakeAuthenticator{err: pkgauth.ErrNoCredential})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Type != "not_found" || body.Error.Message == "" {
		t.Errorf("body = %+v, want error.type=not_found with a non-empty message", body)
	}
}

func TestNewRouter_HealthNoAuth(t *testing.T) {
	h := newTestRouter(&fakeAuthenticator{err: pkgauth.ErrNoCredential})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// GET /health carries the LLM plane's limit-denial counters under "limits"
// when this process runs the LLM plane, and omits the key otherwise.
func TestNewRouter_HealthLimits(t *testing.T) {
	get := func(deps Deps) map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		NewRouter(deps).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}
	auth := &fakeAuthenticator{err: pkgauth.ErrNoCredential}
	if _, ok := get(Deps{Authenticator: auth})["limits"]; ok {
		t.Error("limits reported without an LLM plane")
	}
	body := get(Deps{Authenticator: auth, LimitsStatus: func() map[string]any {
		return map[string]any{"budget_denials": uint64(3), "rpm_denials": uint64(5)}
	}})
	lim, _ := body["limits"].(map[string]any)
	if lim["budget_denials"] != float64(3) || lim["rpm_denials"] != float64(5) {
		t.Errorf("limits = %v, want budget_denials 3, rpm_denials 5", body["limits"])
	}
}

func TestNewRouter_AuthMe_Unauthenticated(t *testing.T) {
	h := newTestRouter(&fakeAuthenticator{err: pkgauth.ErrNoCredential})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestNewRouter_AuthMe_Authenticated(t *testing.T) {
	principal := &pkgauth.Principal{
		Subject: "key-1", TenantID: "t1", Roles: []string{"admin"},
		AuthMethod: "apikey", KeyID: "key-1", RawCredential: "gk_supersecret",
	}
	h := newTestRouter(&fakeAuthenticator{principal: principal})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["subject"] != "key-1" || body["tenant_id"] != "t1" || body["auth_method"] != "apikey" {
		t.Errorf("body = %+v", body)
	}
	if _, present := body["raw_credential"]; present {
		t.Error("response leaked raw_credential")
	}
	for k := range body {
		if k == "RawCredential" || k == "rawCredential" {
			t.Errorf("response leaked raw credential under key %q", k)
		}
	}
	rendered := w.Body.String()
	if strings.Contains(rendered, "gk_supersecret") {
		t.Errorf("response body contains the raw credential: %s", rendered)
	}
}

// TestOpenAPI_CoversEveryRoute walks the live chi routing tree (every
// method+pattern actually mounted by NewRouter) and asserts each one is a
// documented path+method in the generated OpenAPI document served at
// GET /api/v1/openapi.json. This is the drift guard buildRoutes/
// buildOpenAPI's single-source-of-truth design promises: if a route is
// ever mounted without going through buildRoutes' table, this test fails.
func TestOpenAPI_CoversEveryRoute(t *testing.T) {
	h := newTestRouter(&fakeAuthenticator{err: pkgauth.ErrNoCredential})

	router, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("NewRouter's return value does not implement chi.Routes (got %T)", h)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/openapi.json status = %d, want 200", w.Code)
	}

	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatalf("decode openapi.json: %v", err)
	}
	if len(spec.Paths) == 0 {
		t.Fatal("openapi.json has no paths")
	}

	var walked int
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1") {
			return nil // e.g. a mounted UI's "/*"; ServeUI is false here anyway
		}
		walked++

		methods, ok := spec.Paths[route]
		if !ok {
			t.Errorf("chi route %s %s has no entry in openapi.json paths", method, route)
			return nil
		}
		if _, ok := methods[strings.ToLower(method)]; !ok {
			t.Errorf("chi route %s %s: openapi.json paths[%q] has no %q operation", method, route, route, strings.ToLower(method))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if walked == 0 {
		t.Fatal("chi.Walk found no /api/v1 routes -- test is not exercising anything")
	}
}

func TestMalformedIDIs404NotInternal(t *testing.T) {
	principals := map[string]*pkgauth.Principal{
		"admin-a": {Subject: "admin-a", TenantID: "tenant-a", Roles: []string{"admin"}, AuthMethod: "test"},
	}
	h := NewRouter(Deps{
		ServiceVersion: "test",
		Authenticator:  &scriptedAuthenticator{principals: principals},
		Authorizer:     pkgauth.NewRoleAuthorizer(),
		Store:          nopStore{},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/connectors/not-a-uuid", nil)
	req.Header.Set("Authorization", "Bearer admin-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /connectors/not-a-uuid = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}
