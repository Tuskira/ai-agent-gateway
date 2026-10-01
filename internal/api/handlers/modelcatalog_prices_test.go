package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/modelcatalog/prices"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// testNebiusSource is a handler-test double for the real "nebius"
// prices.Source: it fetches the SAME documented JSON shape
// (https://tokenfactory.nebius.com/api/public/models_info) from an
// httptest.Server, so PreviewPrices/ApplyPrices' own request handling gets
// exercised against a real round trip, without reimplementing nebius.go's
// region-matching (already covered by internal/modelcatalog/prices's own
// tests). Registered via prices.RegisterForTest.
type testNebiusSource struct{ url string }

func (testNebiusSource) RequiresAPIKey() bool { return false }

func (s testNebiusSource) Fetch(_ context.Context, _ *store.ModelCatalogProvider, _ string) (*prices.Result, error) {
	resp, err := http.Get(s.url) //nolint:noctx
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var raw []struct {
		Flavors []struct {
			ModelID                     string  `json:"model_id"`
			InputPricePerMillionTokens  float64 `json:"input_price_per_million_tokens"`
			OutputPricePerMillionTokens float64 `json:"output_price_per_million_tokens"`
		} `json:"flavors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := map[string]store.ModelPrice{}
	for _, m := range raw {
		for _, f := range m.Flavors {
			out[f.ModelID] = store.ModelPrice{Input: f.InputPricePerMillionTokens, Output: f.OutputPricePerMillionTokens}
		}
	}
	return &prices.Result{SourceURL: s.url, Prices: out}, nil
}

// newPricesDeps seeds a catalog provider under slug for a Preview/Apply
// test, with two models: one at modelID1/price1 (nil price1 means "no
// price yet"), one at modelID2/price2.
func newPricesDeps(t *testing.T, slug, baseURL string, price1, price2 *store.ModelPrice) (deps Deps, fs *fakeStore, providerID, model1ID, model2ID string) {
	t.Helper()
	deps = newTestDeps()
	fs = deps.Store.(*fakeStore)

	p := &store.ModelCatalogProvider{Slug: slug, DisplayName: strings.ToUpper(slug), Vendor: "openai_compat", BaseURL: baseURL, Enabled: true}
	if err := fs.ModelCatalog().CreateProvider(context.Background(), p); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	m1 := &store.ModelCatalogModel{ProviderID: p.ID, ModelID: "vendor/model-one", SuggestedName: slug + "-model-one", Price: price1, Enabled: true}
	if err := fs.ModelCatalog().CreateModel(context.Background(), m1); err != nil {
		t.Fatalf("seed model 1: %v", err)
	}
	m2 := &store.ModelCatalogModel{ProviderID: p.ID, ModelID: "vendor/model-two", SuggestedName: slug + "-model-two", Price: price2, Enabled: true}
	if err := fs.ModelCatalog().CreateModel(context.Background(), m2); err != nil {
		t.Fatalf("seed model 2: %v", err)
	}
	return deps, fs, p.ID, m1.ID, m2.ID
}

// togetherFixtureServer serves the documented
// docs.together.ai/reference/models GET /v1/models shape: "vendor/model-
// one" at a DIFFERENT price than its seeded catalog price (so Preview
// reports changed=true), plus one model absent from the catalog (so
// Preview's unmatched_provider_models counts it). "vendor/model-two" is
// deliberately NOT returned, so its item previews with new_price:null.
func togetherFixtureServer(t *testing.T, wantAuth string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("request path = %q, want /v1/models", r.URL.Path)
		}
		if wantAuth != "" {
			if got := r.Header.Get("Authorization"); got != "Bearer "+wantAuth {
				t.Errorf("Authorization = %q, want %q", got, "Bearer "+wantAuth)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"vendor/model-one","object":"model","pricing":{"input":2,"output":6,"cached_input":1}},
			{"id":"vendor/extra-model","object":"model","pricing":{"input":9,"output":9}}
		]`))
	}))
}

// ---------------------------------------------------------------------------
// PreviewPrices
// ---------------------------------------------------------------------------

func TestPreviewPrices_UnsupportedProvider(t *testing.T) {
	deps, _, providerID, _, _ := newCatalogDeps(t) // slug "acme" -- no registered Source
	h := ModelCatalog{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/prices/preview", strings.NewReader(`{}`)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/prices/preview", "", h.PreviewPrices, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "price refresh not supported for acme") {
		t.Errorf("body = %s, want a message naming the unsupported provider", w.Body.String())
	}
}

func TestPreviewPrices_Together_ComputesDiffAndUnmatchedCount(t *testing.T) {
	srv := togetherFixtureServer(t, "test-key")
	defer srv.Close()

	oldPrice1 := &store.ModelPrice{Input: 1, Output: 1}
	deps, _, providerID, model1ID, model2ID := newPricesDeps(t, "together", srv.URL+"/v1", oldPrice1, nil)
	h := ModelCatalog{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/prices/preview", strings.NewReader(`{"api_key":"test-key"}`)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/prices/preview", "", h.PreviewPrices, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "test-key") {
		t.Fatal("response body echoed the api key")
	}

	var resp catalogPricesPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SourceURL != srv.URL+"/v1/models" {
		t.Errorf("SourceURL = %q, want %q", resp.SourceURL, srv.URL+"/v1/models")
	}
	if resp.UnmatchedProviderModels != 1 {
		t.Errorf("UnmatchedProviderModels = %d, want 1 (vendor/extra-model)", resp.UnmatchedProviderModels)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("Items = %+v, want 2 (one per catalog model)", resp.Items)
	}
	byID := map[string]catalogPriceItemView{}
	for _, it := range resp.Items {
		byID[it.CatalogModelID] = it
	}
	item1 := byID[model1ID]
	if item1.ModelID != "vendor/model-one" || item1.NewPrice == nil || item1.NewPrice.Input != 2 || item1.NewPrice.Output != 6 || item1.NewPrice.CacheRead != 1 || !item1.Changed {
		t.Errorf("item for model-one = %+v, want new_price {2,6,cache_read=1} and changed=true", item1)
	}
	item2 := byID[model2ID]
	if item2.NewPrice != nil || item2.Changed {
		t.Errorf("item for model-two = %+v, want new_price=null and changed=false (not in the feed)", item2)
	}
}

func TestPreviewPrices_Together_MissingKeyIsBadRequest(t *testing.T) {
	deps, _, providerID, _, _ := newPricesDeps(t, "together", "https://api.together.ai/v1", nil, nil)
	h := ModelCatalog{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/prices/preview", strings.NewReader(`{}`)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/prices/preview", "", h.PreviewPrices, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestPreviewPrices_BothCredentialAndAPIKeyIsBadRequest(t *testing.T) {
	deps, _, providerID, _, _ := newPricesDeps(t, "together", "https://api.together.ai/v1", nil, nil)
	h := ModelCatalog{Deps: deps}

	body := `{"credential":"c","api_key":"k"}`
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/prices/preview", strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/prices/preview", "", h.PreviewPrices, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestPreviewPrices_Together_WithExistingCredential(t *testing.T) {
	srv := togetherFixtureServer(t, "ck-secret")
	defer srv.Close()

	deps, fs, providerID, _, _ := newPricesDeps(t, "together", srv.URL+"/v1", nil, nil)
	h := ModelCatalog{Deps: deps}

	ciphertext, nonce, keyID, fieldNames, err := deps.Secrets.Seal(map[string]string{"api_key": "ck-secret"})
	if err != nil {
		t.Fatalf("seal credential: %v", err)
	}
	cred := &store.Credential{TenantID: "tenant-a", Name: "together-api-key", Type: "api_key", Ciphertext: ciphertext, Nonce: nonce, KeyID: keyID, FieldNames: fieldNames}
	if err := fs.Credentials().Create(context.Background(), cred); err != nil {
		t.Fatalf("create credential: %v", err)
	}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/prices/preview", strings.NewReader(`{"credential":"together-api-key"}`)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/prices/preview", "", h.PreviewPrices, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestPreviewPrices_Together_FetchFailureIsBadGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	deps, _, providerID, _, _ := newPricesDeps(t, "together", srv.URL, nil, nil)
	h := ModelCatalog{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/prices/preview", strings.NewReader(`{"api_key":"bad-key"}`)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/prices/preview", "", h.PreviewPrices, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "bad-key") {
		t.Fatal("response body echoed the api key")
	}
}

func TestPreviewPrices_Nebius_NoKeyNeeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"flavors":[{"model_id":"vendor/model-one","input_price_per_million_tokens":3,"output_price_per_million_tokens":15}]}]`))
	}))
	defer srv.Close()
	restore := prices.RegisterForTest("nebius", testNebiusSource{url: srv.URL})
	defer restore()

	deps, _, providerID, model1ID, _ := newPricesDeps(t, "nebius", "https://api.tokenfactory.eu-west2.nebius.com/v1", nil, nil)
	h := ModelCatalog{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/prices/preview", strings.NewReader(`{}`)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/prices/preview", "", h.PreviewPrices, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (nebius needs no key): %s", w.Code, w.Body.String())
	}
	var resp catalogPricesPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var item1 *catalogPriceItemView
	for i := range resp.Items {
		if resp.Items[i].CatalogModelID == model1ID {
			item1 = &resp.Items[i]
		}
	}
	if item1 == nil || item1.NewPrice == nil || item1.NewPrice.Input != 3 || item1.NewPrice.Output != 15 {
		t.Errorf("item for model-one = %+v, want new_price {3,15}", item1)
	}
}

// ---------------------------------------------------------------------------
// ApplyPrices
// ---------------------------------------------------------------------------

func TestApplyPrices_HappyPath(t *testing.T) {
	deps, fs, providerID, model1ID, model2ID := newPricesDeps(t, "together", "https://api.together.ai/v1", nil, nil)
	h := ModelCatalog{Deps: deps}
	inv := &recordingInvalidator{}
	h.ModelInvalidator = inv

	body := fmt.Sprintf(`{"items":[{"catalog_model_id":%q,"price":{"input":2,"output":6}},{"catalog_model_id":%q,"price":{"input":1,"output":2}}],"update_tenant_models":false}`, model1ID, model2ID)
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/prices/apply", strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/prices/apply", "", h.ApplyPrices, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp catalogPricesApplyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CatalogUpdated != 2 || resp.TenantUpdated != 0 {
		t.Errorf("resp = %+v, want catalog_updated=2 tenant_updated=0", resp)
	}
	if inv.count() != 0 {
		t.Errorf("ModelInvalidator called %d times, want 0 (update_tenant_models was false)", inv.count())
	}
	m1, err := fs.ModelCatalog().GetModel(context.Background(), model1ID)
	if err != nil || m1.Price == nil || m1.Price.Input != 2 || m1.Price.Output != 6 {
		t.Errorf("GetModel(model1) = %+v, %v, want price {2,6}", m1, err)
	}
}

func TestApplyPrices_UpdateTenantModels_InvalidatesAffectedTenant(t *testing.T) {
	deps, fs, providerID, model1ID, _ := newPricesDeps(t, "together", "https://api.together.ai/v1", &store.ModelPrice{Input: 1, Output: 2}, nil)
	h := ModelCatalog{Deps: deps}
	inv := &recordingInvalidator{}
	h.ModelInvalidator = inv

	// A tenant model connected from model1, still at model1's old price.
	tm := &store.Model{
		TenantID: "tenant-b", Name: "connected-model", Enabled: true,
		Targets:        []store.ModelTarget{{Vendor: "openai_compat", Model: "vendor/model-one", BaseURL: "https://api.together.ai/v1", Label: "together"}},
		Price:          &store.ModelPrice{Input: 1, Output: 2},
		CatalogModelID: model1ID,
	}
	if err := fs.Models().Create(context.Background(), tm); err != nil {
		t.Fatalf("seed connected tenant model: %v", err)
	}

	body := fmt.Sprintf(`{"items":[{"catalog_model_id":%q,"price":{"input":3,"output":4}}],"update_tenant_models":true}`, model1ID)
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/prices/apply", strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/prices/apply", "", h.ApplyPrices, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp catalogPricesApplyResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.TenantUpdated != 1 {
		t.Errorf("resp = %+v, want tenant_updated=1", resp)
	}
	if inv.count() != 1 {
		t.Errorf("ModelInvalidator called %d times, want 1 (tenant-b)", inv.count())
	}
	gotTM, err := fs.Models().Get(context.Background(), "tenant-b", tm.ID)
	if err != nil || gotTM.Price == nil || gotTM.Price.Input != 3 || gotTM.Price.Output != 4 {
		t.Errorf("tenant model after apply = %+v, %v, want refreshed price {3,4}", gotTM, err)
	}
}

func TestApplyPrices_UnknownCatalogModelIsBadRequest(t *testing.T) {
	deps, _, providerID, _, _ := newPricesDeps(t, "together", "https://api.together.ai/v1", nil, nil)
	h := ModelCatalog{Deps: deps}

	body := `{"items":[{"catalog_model_id":"no-such-model","price":{"input":1,"output":1}}]}`
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/prices/apply", strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/prices/apply", "", h.ApplyPrices, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestApplyPrices_NegativePriceIsBadRequest(t *testing.T) {
	deps, _, providerID, model1ID, _ := newPricesDeps(t, "together", "https://api.together.ai/v1", nil, nil)
	h := ModelCatalog{Deps: deps}

	body := fmt.Sprintf(`{"items":[{"catalog_model_id":%q,"price":{"input":-1,"output":1}}]}`, model1ID)
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/prices/apply", strings.NewReader(body)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/prices/apply", "", h.ApplyPrices, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestApplyPrices_EmptyItemsIsBadRequest(t *testing.T) {
	deps, _, providerID, _, _ := newPricesDeps(t, "together", "https://api.together.ai/v1", nil, nil)
	h := ModelCatalog{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/prices/apply", strings.NewReader(`{"items":[]}`)), "tenant-a", "admin")
	w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/prices/apply", "", h.ApplyPrices, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}
