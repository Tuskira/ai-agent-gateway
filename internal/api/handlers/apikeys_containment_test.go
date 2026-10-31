package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/inflight"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// keyAuth authenticates every request as the key it names.
type keyAuth string

func (keyAuth) Name() string { return "test" }
func (k keyAuth) Authenticate(context.Context, *http.Request) (*pkgauth.Principal, error) {
	return &pkgauth.Principal{KeyID: string(k)}, nil
}

type keyHashRecorder struct{ hashes []string }

func (r *keyHashRecorder) InvalidateKey(h string) { r.hashes = append(r.hashes, h) }

// openCall files one open request of keyID on plane in reg and returns
// its context, cancelled when containment cuts it.
func openCall(t *testing.T, reg *inflight.Registry, plane, keyID string) context.Context {
	t.Helper()
	ctxCh := make(chan context.Context, 1)
	release := make(chan struct{})
	auth := reg.Authenticator(keyAuth(keyID))
	h := reg.Middleware(plane)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = auth.Authenticate(r.Context(), r)
		ctxCh <- r.Context()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
	t.Cleanup(func() { close(release) })
	return <-ctxCh
}

func TestAPIKeys_Containment(t *testing.T) {
	deps := newTestDeps()
	inv := &keyHashRecorder{}
	deps.KeyInvalidator = inv
	deps.Inflight = inflight.New()
	h := APIKeys{Deps: deps}

	w := serve(http.MethodPost, "/api-keys", h.Create,
		withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"agent","role":"agent"}`)), "tenant-a", "admin"))
	var created apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	contain := func(body string) (*httptest.ResponseRecorder, containmentView) {
		t.Helper()
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys/"+created.ID+"/containment", strings.NewReader(body)), "tenant-a", "admin")
		w := serve(http.MethodPost, "/api-keys/{id}/containment", h.Containment, req)
		var v containmentView
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return w, v
	}

	if w, _ := contain(`{"level":"kill"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "a note is required") {
		t.Fatalf("kill without note: %d %s", w.Code, w.Body.String())
	}
	if w, _ := contain(`{"level":"quarantine","note":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad level: %d", w.Code)
	}

	llm, mcp := openCall(t, deps.Inflight, "llm", created.ID), openCall(t, deps.Inflight, "mcp", created.ID)
	w, v := contain(`{"level":"restrict","note":"prompt injection seen"}`)
	if w.Code != http.StatusOK || v.Containment != "restricted" || v.RestrictedAt == "" || v.ContainmentNote != "prompt injection seen" {
		t.Fatalf("restrict: %d %s", w.Code, w.Body.String())
	}
	if v.Cut["mcp"] != 1 || v.Cut["llm"] != 0 || mcp.Err() == nil || llm.Err() != nil {
		t.Fatalf("restrict cut %v (mcp err %v, llm err %v)", v.Cut, mcp.Err(), llm.Err())
	}
	if len(inv.hashes) != 1 {
		t.Fatalf("cache invalidations %d", len(inv.hashes))
	}

	w, v = contain(`{"level":"kill","reason":"exfiltration"}`) // reason is an alias of note
	if w.Code != http.StatusOK || v.Containment != "killed" || v.SuspendedAt == "" || v.RestrictedAt != "" || v.Cut["llm"] != 1 || llm.Err() == nil {
		t.Fatalf("kill: %d %s", w.Code, w.Body.String())
	}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys/"+created.ID+"/resume", nil), "tenant-a", "admin")
	w = serve(http.MethodPost, "/api-keys/{id}/resume", h.Resume, req)
	v = containmentView{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || w.Code != http.StatusOK || v.Containment != "normal" || v.SuspendedAt != "" {
		t.Fatalf("resume: %d %s", w.Code, w.Body.String())
	}
	req = withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys/"+created.ID+"/suspend", nil), "tenant-a", "admin")
	w = serve(http.MethodPost, "/api-keys/{id}/suspend", h.Suspend, req)
	v = containmentView{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || w.Code != http.StatusOK || v.Containment != "killed" || v.ContainmentNote != "suspended" {
		t.Fatalf("suspend: %d %s", w.Code, w.Body.String())
	}

	// Another tenant cannot contain the key.
	req = withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys/"+created.ID+"/containment", strings.NewReader(`{"level":"kill","note":"x"}`)), "tenant-b", "admin")
	if w := serve(http.MethodPost, "/api-keys/{id}/containment", h.Containment, req); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant: %d", w.Code)
	}
}

func TestAPIKeys_ContainmentRefusesOwnKey(t *testing.T) {
	deps := newTestDeps()
	h := APIKeys{Deps: deps}
	w := serve(http.MethodPost, "/api-keys", h.Create,
		withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"me","role":"admin"}`)), "tenant-a", "admin"))
	var created apiKeyCreateView
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	req := httptest.NewRequest(http.MethodPost, "/api-keys/"+created.ID+"/containment", strings.NewReader(`{"level":"kill","note":"x"}`))
	req = req.WithContext(pkgauth.WithPrincipal(req.Context(), &pkgauth.Principal{TenantID: "tenant-a", Roles: []string{"admin"}, KeyID: created.ID}))
	if w := serve(http.MethodPost, "/api-keys/{id}/containment", h.Containment, req); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "you can't contain the key you are signed in with") {
		t.Fatalf("own key: %d %s", w.Code, w.Body.String())
	}
}
