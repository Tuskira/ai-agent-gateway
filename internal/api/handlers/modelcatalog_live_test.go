package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// TestCatalogLiveNebius calls ModelCatalog.Test (the real handler code
// path -- no shortcuts to callProviderModels) against the REAL Nebius AI
// Studio API, using the default catalog's own nebius provider row
// (base_url, model ids) exactly as migration 000006_model_catalog seeds
// it. Gated on NEBIUS_API_KEY; skipped (not failed) when unset, so a
// normal `go test ./...` run never depends on network access or a live
// key. The key itself is never logged -- only ok/status/model ids, none
// of which can contain it.
func TestCatalogLiveNebius(t *testing.T) {
	key := os.Getenv("NEBIUS_API_KEY")
	if key == "" {
		t.Skip("NEBIUS_API_KEY not set; skipping live Nebius test")
	}

	deps := newTestDeps()
	fs := deps.Store.(*fakeStore)
	p := &store.ModelCatalogProvider{
		Slug: "nebius", DisplayName: "Nebius AI Studio", Vendor: "openai_compat",
		BaseURL: "https://api.tokenfactory.eu-west2.nebius.com/v1", Enabled: true,
	}
	if err := fs.ModelCatalog().CreateProvider(context.Background(), p); err != nil {
		t.Fatalf("seed nebius provider: %v", err)
	}
	for _, m := range []struct{ modelID, suggested string }{
		{"moonshotai/Kimi-K3", "kimi-k3-nebius"},
		{"zai-org/GLM-5.3", "glm-5.3-nebius"},
	} {
		cm := &store.ModelCatalogModel{ProviderID: p.ID, ModelID: m.modelID, SuggestedName: m.suggested, Enabled: true}
		if err := fs.ModelCatalog().CreateModel(context.Background(), cm); err != nil {
			t.Fatalf("seed catalog model %s: %v", m.modelID, err)
		}
	}

	h := ModelCatalog{Deps: deps}
	body := `{"api_key":"` + key + `"}`
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+p.ID+"/test", strings.NewReader(body)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/model-catalog/providers/{id}/test", h.Test, req)

	if strings.Contains(w.Body.String(), key) {
		t.Fatal("response body echoed the Nebius API key")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("handler status = %d, want 200 (failures are reported in the body): %s", w.Code, w.Body.String())
	}

	var resp catalogTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	t.Logf("Nebius /test result: ok=%v status=%d error=%q catalog_matches=%v", resp.OK, resp.Status, resp.Error, resp.CatalogMatches)
	t.Logf("Nebius models_found (ids only): %v", resp.ModelsFound)
}
