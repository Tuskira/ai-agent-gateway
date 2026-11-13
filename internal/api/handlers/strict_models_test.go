package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// PATCH /tenants/{id} sets the tenant's strict model policy (alone or with
// a rename), reports it in the view, and drops the LLM plane's cached
// policy for that tenant.
func TestTenants_UpdateStrictModels(t *testing.T) {
	deps := newTestDeps()
	inv := &recordingInvalidator{}
	deps.ModelInvalidator = inv
	h := Tenants{Deps: deps}

	w := serve(http.MethodPost, "/tenants", h.Create,
		withPrincipal(httptest.NewRequest(http.MethodPost, "/tenants", strings.NewReader(`{"slug":"acme","name":"Acme"}`)), "t", "admin"))
	var created tenantView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.StrictModels || !strings.Contains(w.Body.String(), `"strict_models":false`) {
		t.Fatalf("new tenant = %s, want strict_models false", w.Body.String())
	}
	patch := func(body string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodPatch, "/tenants/"+created.ID, strings.NewReader(body)), "t", "admin")
		return serve(http.MethodPatch, "/tenants/{tenant_id}", h.Update, req)
	}

	w = patch(`{"strict_models":true}`)
	var got tenantView
	if err := json.Unmarshal(w.Body.Bytes(), &got); w.Code != http.StatusOK || err != nil || !got.StrictModels || got.Name != "Acme" {
		t.Fatalf("set strict: %d %s", w.Code, w.Body.String())
	}
	if inv.count() != 1 || inv.tenants[0] != created.ID {
		t.Errorf("invalidations = %v, want [%s]", inv.tenants, created.ID)
	}

	w = patch(`{"name":"Acme Corp","strict_models":false}`)
	got = tenantView{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); w.Code != http.StatusOK || err != nil || got.StrictModels || got.Name != "Acme Corp" {
		t.Fatalf("rename + clear strict: %d %s", w.Code, w.Body.String())
	}

	for _, bad := range []string{`{"strict_models":"yes"}`, `{"strict_models":null}`, `{"strict_models":1}`} {
		if w := patch(bad); w.Code != http.StatusBadRequest {
			t.Errorf("patch %s: status %d, want 400", bad, w.Code)
		}
	}
	if tn, _ := deps.Store.Tenants().GetByID(context.Background(), created.ID); tn.StrictModels {
		t.Error("rejected patches changed the policy")
	}

	// Create can set it too.
	w = serve(http.MethodPost, "/tenants", h.Create,
		withPrincipal(httptest.NewRequest(http.MethodPost, "/tenants", strings.NewReader(`{"slug":"strict","name":"Strict","strict_models":true}`)), "t", "admin"))
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"strict_models":true`) {
		t.Errorf("create strict: %d %s", w.Code, w.Body.String())
	}
}

// PATCH /api-keys/{id} sets (true/false) or clears (null) the key's strict
// model override, and evicts the key's cached lookup so the LLM plane sees
// it on the next call. Rotation carries the override over.
func TestAPIKeys_UpdateStrictModels(t *testing.T) {
	deps := newTestDeps()
	inv := &keyHashRecorder{}
	deps.KeyInvalidator = inv
	h := APIKeys{Deps: deps}

	w := serve(http.MethodPost, "/api-keys", h.Create,
		withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"k","role":"agent"}`)), "tenant-a", "admin"))
	var created apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), `"strict_models":null`) {
		t.Fatalf("new key = %s, want strict_models null (inherit)", w.Body.String())
	}
	patch := func(tenant, body string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodPatch, "/api-keys/"+created.ID, strings.NewReader(body)), tenant, "admin")
		return serve(http.MethodPatch, "/api-keys/{id}", h.Update, req)
	}

	for _, tc := range []struct{ body, want string }{
		{`{"strict_models":true}`, `"strict_models":true`},
		{`{"strict_models":false}`, `"strict_models":false`},
		{`{"strict_models":null}`, `"strict_models":null`},
	} {
		w := patch("tenant-a", tc.body)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("patch %s: %d %s, want %s", tc.body, w.Code, w.Body.String(), tc.want)
		}
	}
	if len(inv.hashes) != 3 {
		t.Errorf("key cache invalidations = %d, want 3", len(inv.hashes))
	}

	for _, bad := range []string{`{"strict_models":"true"}`, `{"strict_models":1}`} {
		if w := patch("tenant-a", bad); w.Code != http.StatusBadRequest {
			t.Errorf("patch %s: status %d, want 400", bad, w.Code)
		}
	}
	if w := patch("tenant-b", `{"strict_models":true}`); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant patch: status %d, want 404", w.Code)
	}

	// Create with an override, then rotate: the new key keeps it.
	w = serve(http.MethodPost, "/api-keys", h.Create,
		withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"s","role":"agent","strict_models":true}`)), "tenant-a", "admin"))
	var strictKey apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &strictKey); err != nil || strictKey.StrictModels == nil || !*strictKey.StrictModels {
		t.Fatalf("create with strict_models: %d %s", w.Code, w.Body.String())
	}
	w = serve(http.MethodPost, "/api-keys/{id}/rotate", h.Rotate,
		withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys/"+strictKey.ID+"/rotate", nil), "tenant-a", "admin"))
	if w.Code != http.StatusOK && w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"strict_models":true`) {
		t.Errorf("rotate: %d %s, want the override carried over", w.Code, w.Body.String())
	}
}
