package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// newCatalogDeps is newTestDeps plus a real pkgauth.RoleAuthorizer (needed
// by Connect's Deps.Authorizer secondary checks) and one seeded provider
// with two models -- one enabled, one disabled.
func newCatalogDeps(t *testing.T) (deps Deps, fs *fakeStore, providerID string, enabledModelID, disabledModelID string) {
	t.Helper()
	deps = newTestDeps()
	deps.Authorizer = pkgauth.NewRoleAuthorizer()
	fs = deps.Store.(*fakeStore)

	p := &store.ModelCatalogProvider{Slug: "acme", DisplayName: "Acme AI", Vendor: "openai_compat", BaseURL: "https://api.acme.example/v1", Enabled: true}
	if err := fs.ModelCatalog().CreateProvider(context.Background(), p); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	m1 := &store.ModelCatalogModel{ProviderID: p.ID, ModelID: "acme/large-v1", DisplayName: "Acme Large", SuggestedName: "acme-large", Enabled: true, Capabilities: map[string]any{"tools": true}}
	if err := fs.ModelCatalog().CreateModel(context.Background(), m1); err != nil {
		t.Fatalf("seed model 1: %v", err)
	}
	m2 := &store.ModelCatalogModel{ProviderID: p.ID, ModelID: "acme/small-v1", SuggestedName: "acme-small", Enabled: false}
	if err := fs.ModelCatalog().CreateModel(context.Background(), m2); err != nil {
		t.Fatalf("seed model 2 (disabled): %v", err)
	}
	return deps, fs, p.ID, m1.ID, m2.ID
}

func doJSON(t *testing.T, method, pattern, path string, h http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	return serve(method, pattern, h, req)
}

// ---------------------------------------------------------------------------
// Providers
// ---------------------------------------------------------------------------

func TestModelCatalog_CreateProvider_WarnsOnMissingVersionSegment(t *testing.T) {
	deps, _, _, _, _ := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}

	body := `{"slug":"nowarn","display_name":"No Warn","base_url":"https://api.example.com/v1"}`
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers", strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers", "", h.CreateProvider, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var view catalogProviderView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none for a versioned base_url", view.Warnings)
	}

	body2 := `{"slug":"warns","display_name":"Warns","base_url":"https://api.example.com/"}`
	req2 := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers", strings.NewReader(body2)), "tenant-a", "admin")
	w2 := doJSON(t, http.MethodPost, "/model-catalog/providers", "", h.CreateProvider, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w2.Code, w2.Body.String())
	}
	var view2 catalogProviderView
	if err := json.Unmarshal(w2.Body.Bytes(), &view2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view2.BaseURL != "https://api.example.com" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", view2.BaseURL)
	}
	if len(view2.Warnings) != 1 {
		t.Errorf("Warnings = %v, want one entry for a base_url with no version segment", view2.Warnings)
	}
}

func TestModelCatalog_CreateProvider_DuplicateSlugConflict(t *testing.T) {
	deps, _, _, _, _ := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}
	body := `{"slug":"acme","display_name":"Dup","base_url":"https://x.example/v1"}`
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers", strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers", "", h.CreateProvider, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestModelCatalog_UpdateProvider(t *testing.T) {
	deps, _, providerID, _, _ := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}
	body := `{"slug":"acme","display_name":"Acme AI v2","base_url":"https://api.acme.example/v2","enabled":false}`
	req := withPrincipal(httptest.NewRequest(http.MethodPut, "/model-catalog/providers/"+providerID, strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPut, "/model-catalog/providers/{id}", "", h.UpdateProvider, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var view catalogProviderView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if view.DisplayName != "Acme AI v2" || view.Enabled || len(view.Models) != 2 {
		t.Errorf("view = %+v, want updated fields and its 2 models still nested", view)
	}
}

func TestModelCatalog_DeleteProvider_UsageConfirmationAndForce(t *testing.T) {
	deps, fs, providerID, enabledModelID, _ := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}

	// Connect the enabled model for a tenant, so the provider has usage.
	if _, err := fs.ModelCatalog().Connect(context.Background(), "tenant-a",
		store.CatalogConnectCredential{New: &store.CatalogConnectNewCredential{Name: "acme-api-key", Type: "api_key", Ciphertext: []byte("c"), Nonce: []byte("n"), KeyID: "k1"}},
		[]store.CatalogConnectModel{{CatalogModelID: enabledModelID, Name: "acme-large", ModelID: "acme/large-v1", BaseURL: "https://api.acme.example/v1"}},
	); err != nil {
		t.Fatalf("seed connect: %v", err)
	}

	req := withPrincipal(httptest.NewRequest(http.MethodDelete, "/model-catalog/providers/"+providerID, nil), "tenant-a", "admin")
	w := doJSON(t, http.MethodDelete, "/model-catalog/providers/{id}", "", h.DeleteProvider, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (in use): %s", w.Code, w.Body.String())
	}
	var conflict struct {
		TenantModels int `json:"tenant_models"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &conflict)
	if conflict.TenantModels != 1 {
		t.Errorf("tenant_models = %d, want 1", conflict.TenantModels)
	}

	req2 := withPrincipal(httptest.NewRequest(http.MethodDelete, "/model-catalog/providers/"+providerID+"?force=true", nil), "tenant-a", "admin")
	w2 := doJSON(t, http.MethodDelete, "/model-catalog/providers/{id}", "", h.DeleteProvider, req2)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("force delete status = %d, want 204: %s", w2.Code, w2.Body.String())
	}
	if _, err := fs.ModelCatalog().GetProvider(context.Background(), providerID); err == nil {
		t.Error("provider still exists after force delete")
	}
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

func TestModelCatalog_CreateModel_DefaultSuggestedName(t *testing.T) {
	deps, _, providerID, _, _ := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}
	body := `{"model_id":"zai-org/GLM-5.3"}`
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/models", strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/models", "", h.CreateModel, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var view catalogModelView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if view.SuggestedName != "glm-5.3-acme" {
		t.Errorf("SuggestedName = %q, want %q", view.SuggestedName, "glm-5.3-acme")
	}
}

func TestModelCatalog_CreateModel_ValidationErrors(t *testing.T) {
	deps, _, providerID, _, _ := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}

	cases := []string{
		`{"model_id":"x","capabilities":{"unknown_field":true}}`,
		`{"model_id":"x","price":{"input":-1,"output":1}}`,
		`{"model_id":"x","suggested_name":"Bad Name!"}`,
		`{"model_id":""}`,
	}
	for _, body := range cases {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/models", strings.NewReader(body)), "tenant-a", "admin")
		w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/models", "", h.CreateModel, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d, want 400: %s", body, w.Code, w.Body.String())
		}
	}
}

func TestModelCatalog_UpdateModel(t *testing.T) {
	deps, _, _, enabledModelID, _ := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}
	body := `{"model_id":"acme/large-v2","display_name":"Acme Large v2","suggested_name":"acme-large-v2","enabled":false}`
	req := withPrincipal(httptest.NewRequest(http.MethodPut, "/model-catalog/models/"+enabledModelID, strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPut, "/model-catalog/models/{id}", "", h.UpdateModel, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var view catalogModelView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if view.ModelID != "acme/large-v2" || view.SuggestedName != "acme-large-v2" || view.Enabled {
		t.Errorf("view = %+v, want updated fields", view)
	}
}

func TestModelCatalog_DeleteModel_UsageConfirmationAndModelUsage(t *testing.T) {
	deps, fs, _, enabledModelID, _ := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}

	usageReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/model-catalog/models/"+enabledModelID+"/usage", nil), "tenant-a", "admin")
	w := doJSON(t, http.MethodGet, "/model-catalog/models/{id}/usage", "", h.ModelUsage, usageReq)
	if w.Code != http.StatusOK {
		t.Fatalf("usage status = %d: %s", w.Code, w.Body.String())
	}
	var usage struct {
		TenantModels int `json:"tenant_models"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &usage)
	if usage.TenantModels != 0 {
		t.Fatalf("tenant_models = %d, want 0 before any connect", usage.TenantModels)
	}

	if _, err := fs.ModelCatalog().Connect(context.Background(), "tenant-a",
		store.CatalogConnectCredential{New: &store.CatalogConnectNewCredential{Name: "acme-api-key", Type: "api_key", Ciphertext: []byte("c"), Nonce: []byte("n"), KeyID: "k1"}},
		[]store.CatalogConnectModel{{CatalogModelID: enabledModelID, Name: "acme-large", ModelID: "acme/large-v1", BaseURL: "https://api.acme.example/v1"}},
	); err != nil {
		t.Fatalf("seed connect: %v", err)
	}

	delReq := withPrincipal(httptest.NewRequest(http.MethodDelete, "/model-catalog/models/"+enabledModelID, nil), "tenant-a", "admin")
	wDel := doJSON(t, http.MethodDelete, "/model-catalog/models/{id}", "", h.DeleteModel, delReq)
	if wDel.Code != http.StatusConflict {
		t.Fatalf("delete status = %d, want 409: %s", wDel.Code, wDel.Body.String())
	}

	forceReq := withPrincipal(httptest.NewRequest(http.MethodDelete, "/model-catalog/models/"+enabledModelID+"?force=true", nil), "tenant-a", "admin")
	wForce := doJSON(t, http.MethodDelete, "/model-catalog/models/{id}", "", h.DeleteModel, forceReq)
	if wForce.Code != http.StatusNoContent {
		t.Fatalf("force delete status = %d: %s", wForce.Code, wForce.Body.String())
	}
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func TestModelCatalog_List_MergesTenantModel(t *testing.T) {
	deps, fs, _, enabledModelID, _ := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}

	res, err := fs.ModelCatalog().Connect(context.Background(), "tenant-a",
		store.CatalogConnectCredential{New: &store.CatalogConnectNewCredential{Name: "acme-api-key", Type: "api_key", Ciphertext: []byte("c"), Nonce: []byte("n"), KeyID: "k1"}},
		[]store.CatalogConnectModel{{CatalogModelID: enabledModelID, Name: "acme-large", ModelID: "acme/large-v1", BaseURL: "https://api.acme.example/v1"}},
	)
	if err != nil {
		t.Fatalf("seed connect: %v", err)
	}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/model-catalog", nil), "tenant-a", "admin")
	w := doJSON(t, http.MethodGet, "/model-catalog", "", h.List, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp catalogListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found, foundOther bool
	for _, p := range resp.Providers {
		for _, m := range p.Models {
			if m.ID == enabledModelID {
				found = true
				if m.TenantModelID == nil || *m.TenantModelID != res.Models[0].ModelID || m.TenantModelName == nil || *m.TenantModelName != "acme-large" {
					t.Errorf("connected model view = %+v, want tenant_model_id/name populated", m)
				}
			} else if p.Slug == "acme" {
				foundOther = true
				if m.TenantModelID != nil {
					t.Errorf("unconnected model %+v has a non-nil tenant_model_id", m)
				}
			}
		}
	}
	if !found || !foundOther {
		t.Fatalf("did not find both the connected and unconnected acme models in %+v", resp)
	}

	// A different tenant sees the same catalog, unconnected.
	req2 := withPrincipal(httptest.NewRequest(http.MethodGet, "/model-catalog", nil), "tenant-b", "admin")
	w2 := doJSON(t, http.MethodGet, "/model-catalog", "", h.List, req2)
	var resp2 catalogListResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	for _, p := range resp2.Providers {
		if p.Slug != "acme" {
			continue
		}
		for _, m := range p.Models {
			if m.TenantModelID != nil {
				t.Errorf("tenant-b sees a connection it never made: %+v", m)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Connect
// ---------------------------------------------------------------------------

func TestModelCatalog_Connect_NewCredentialHappyPath(t *testing.T) {
	deps, fs, providerID, enabledModelID, _ := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}
	inv := &recordingInvalidator{}
	h.ModelInvalidator = inv

	body := fmt.Sprintf(`{"credential":{"new":{"api_key":"sk-secret"}},"models":["%s"]}`, enabledModelID)
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/connect", strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/connect", "", h.Connect, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp catalogConnectResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Credential.Name != "acme-api-key" || !resp.Credential.Created {
		t.Errorf("credential = %+v, want default name and created=true", resp.Credential)
	}
	if len(resp.Models) != 1 || resp.Models[0].Status != "created" {
		t.Errorf("models = %+v, want one created row", resp.Models)
	}
	if inv.count() != 1 {
		t.Errorf("ModelInvalidator called %d times, want 1", inv.count())
	}

	tm, err := fs.Models().Get(context.Background(), "tenant-a", resp.Models[0].ModelID)
	if err != nil {
		t.Fatalf("get connected model: %v", err)
	}
	if tm.Targets[0].Credential != "acme-api-key" || tm.Targets[0].BaseURL != "https://api.acme.example/v1" || tm.CatalogModelID != enabledModelID || tm.Targets[0].Label != "acme" {
		t.Errorf("connected model = %+v, want a target wired to the new credential, provider base_url and label=<provider slug>", tm)
	}
	cred, err := fs.Credentials().Get(context.Background(), "tenant-a", "acme-api-key")
	if err != nil {
		t.Fatalf("get created credential: %v", err)
	}
	if cred.Type != "api_key" || len(cred.FieldNames) != 1 || cred.FieldNames[0] != "api_key" {
		t.Errorf("created credential = %+v, want type=api_key with a single api_key field", cred)
	}

	// Re-requesting is "exists", not a conflict.
	w2 := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/connect", "",
		h.Connect, withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/connect", strings.NewReader(fmt.Sprintf(`{"credential":{"name":"acme-api-key"},"models":["%s"]}`, enabledModelID))), "tenant-a", "admin"))
	if w2.Code != http.StatusCreated {
		t.Fatalf("re-request status = %d: %s", w2.Code, w2.Body.String())
	}
	var resp2 catalogConnectResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	if resp2.Credential.Created || len(resp2.Models) != 1 || resp2.Models[0].Status != "exists" {
		t.Errorf("re-request result = %+v, want reused credential and status=exists", resp2)
	}
}

func TestModelCatalog_Connect_DisabledProviderIsBadRequest(t *testing.T) {
	deps, fs, providerID, enabledModelID, _ := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}
	p, _ := fs.ModelCatalog().GetProvider(context.Background(), providerID)
	p.Enabled = false
	if err := fs.ModelCatalog().UpdateProvider(context.Background(), p); err != nil {
		t.Fatalf("disable provider: %v", err)
	}

	body := fmt.Sprintf(`{"credential":{"new":{"api_key":"sk-secret"}},"models":["%s"]}`, enabledModelID)
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/connect", strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/connect", "", h.Connect, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestModelCatalog_Connect_DisabledModelIsBadRequest(t *testing.T) {
	deps, _, providerID, _, disabledModelID := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}
	body := fmt.Sprintf(`{"credential":{"new":{"api_key":"sk-secret"}},"models":["%s"]}`, disabledModelID)
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/connect", strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/connect", "", h.Connect, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestModelCatalog_Connect_NameClashWithHandCreatedModelIs409(t *testing.T) {
	deps, fs, providerID, enabledModelID, _ := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}
	handMade := &store.Model{TenantID: "tenant-a", Name: "acme-large", Enabled: true, Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "x"}}}
	if err := fs.Models().Create(context.Background(), handMade); err != nil {
		t.Fatalf("seed hand-made model: %v", err)
	}

	body := fmt.Sprintf(`{"credential":{"new":{"api_key":"sk-secret"}},"models":["%s"]}`, enabledModelID)
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/connect", strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/connect", "", h.Connect, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestModelCatalog_Connect_RequiresCredentialCreatePermission(t *testing.T) {
	deps, _, providerID, enabledModelID, _ := newCatalogDeps(t)
	deps.Authorizer = pkgauth.NewRoleAuthorizer() // agent has *.read + mcp/llm, not credential.create
	h := ModelCatalog{Deps: deps}
	body := fmt.Sprintf(`{"credential":{"new":{"api_key":"sk-secret"}},"models":["%s"]}`, enabledModelID)
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/connect", strings.NewReader(body)), "tenant-a", "agent")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/connect", "", h.Connect, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (agent lacks credential.create): %s", w.Code, w.Body.String())
	}
}

func TestModelCatalog_Connect_ReusingExistingCredentialRequiresCredentialRead(t *testing.T) {
	deps, _, providerID, enabledModelID, _ := newCatalogDeps(t)
	// A role that grants credential.create (needed for the "new" branch
	// elsewhere) but not credential.read, to isolate the second check:
	// only "admin" and "agent" exist by default and neither isolates this,
	// so build a custom one.
	authz := pkgauth.NewRoleAuthorizer()
	authz.Rules["partial"] = []string{"credential.create", "model.create"}
	deps.Authorizer = authz
	h := ModelCatalog{Deps: deps}

	if _, err := deps.Secrets.Create(context.Background(), "tenant-a", "acme-api-key", "api_key", map[string]string{"api_key": "sk-secret"}, "test"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	body := fmt.Sprintf(`{"credential":{"name":"acme-api-key"},"models":["%s"]}`, enabledModelID)
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/connect", strings.NewReader(body)), "tenant-a", "partial")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/connect", "", h.Connect, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (partial role lacks credential.read): %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Test (provider connectivity check)
// ---------------------------------------------------------------------------

func modelsListJSON(ids ...string) string {
	var b strings.Builder
	b.WriteString(`{"object":"list","data":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%q,"object":"model"}`, id)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestModelCatalog_Test_OK(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsListJSON("acme/large-v1")))
	}))
	t.Cleanup(srv.Close)

	deps, fs, providerID, _, _ := newCatalogDeps(t)
	p, _ := fs.ModelCatalog().GetProvider(context.Background(), providerID)
	p.BaseURL = srv.URL
	if err := fs.ModelCatalog().UpdateProvider(context.Background(), p); err != nil {
		t.Fatalf("point provider at test server: %v", err)
	}
	h := ModelCatalog{Deps: deps}

	body := `{"api_key":"sk-test-key"}`
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/test", strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/test", "", h.Test, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-test-key") {
		t.Fatal("response echoed the api key")
	}
	if gotAuth != "Bearer sk-test-key" {
		t.Errorf("provider saw Authorization = %q", gotAuth)
	}
	var resp catalogTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.Status != 200 || len(resp.ModelsFound) != 1 || resp.ModelsFound[0] != "acme/large-v1" {
		t.Errorf("resp = %+v, want ok with one model found", resp)
	}
	// Keyed by the vendor model id, not the catalog row's uuid.
	if !resp.CatalogMatches["acme/large-v1"] || resp.CatalogMatches["acme/small-v1"] {
		t.Errorf("catalog_matches = %+v, want only acme/large-v1 matched", resp.CatalogMatches)
	}
}

func TestModelCatalog_Test_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	t.Cleanup(srv.Close)

	deps, fs, providerID, _, _ := newCatalogDeps(t)
	p, _ := fs.ModelCatalog().GetProvider(context.Background(), providerID)
	p.BaseURL = srv.URL
	_ = fs.ModelCatalog().UpdateProvider(context.Background(), p)
	h := ModelCatalog{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/test", strings.NewReader(`{"api_key":"bad-key"}`)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/test", "", h.Test, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the failure is reported in the body): %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "bad-key") {
		t.Fatal("response echoed the api key")
	}
	var resp catalogTestResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.OK || resp.Status != 401 || resp.Error == "" {
		t.Errorf("resp = %+v, want ok=false status=401 with an error", resp)
	}
}

func TestModelCatalog_Test_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	deps, fs, providerID, _, _ := newCatalogDeps(t)
	p, _ := fs.ModelCatalog().GetProvider(context.Background(), providerID)
	p.BaseURL = srv.URL
	_ = fs.ModelCatalog().UpdateProvider(context.Background(), p)
	h := ModelCatalog{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/test", strings.NewReader(`{"api_key":"k"}`)), "tenant-a", "admin")
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/test", "", h.Test, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp catalogTestResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.OK {
		t.Errorf("resp = %+v, want ok=false on timeout", resp)
	}
}

func TestModelCatalog_Test_DoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect target was called; Test must not follow redirects")
	}))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/models", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	deps, fs, providerID, _, _ := newCatalogDeps(t)
	p, _ := fs.ModelCatalog().GetProvider(context.Background(), providerID)
	p.BaseURL = srv.URL
	_ = fs.ModelCatalog().UpdateProvider(context.Background(), p)
	h := ModelCatalog{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/test", strings.NewReader(`{"api_key":"k"}`)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/test", "", h.Test, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp catalogTestResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.OK || resp.Status != http.StatusFound {
		t.Errorf("resp = %+v, want ok=false status=302 (redirect reported, not followed)", resp)
	}
}
