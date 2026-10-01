package prices

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func TestLookup(t *testing.T) {
	if _, ok := Lookup("nebius"); !ok {
		t.Error(`Lookup("nebius") = false, want a registered Source`)
	}
	if _, ok := Lookup("together"); !ok {
		t.Error(`Lookup("together") = false, want a registered Source`)
	}
	if _, ok := Lookup("openai"); ok {
		t.Error(`Lookup("openai") = true, want no registered Source`)
	}
}

func TestNebiusRequiresAPIKey(t *testing.T) {
	s, _ := Lookup("nebius")
	if s.RequiresAPIKey() {
		t.Error("nebius Source.RequiresAPIKey() = true, want false (public feed)")
	}
}

func TestTogetherRequiresAPIKey(t *testing.T) {
	s, _ := Lookup("together")
	if !s.RequiresAPIKey() {
		t.Error("together Source.RequiresAPIKey() = false, want true")
	}
}

// nebiusFixture is a trimmed, structurally faithful sample of
// https://tokenfactory.nebius.com/api/public/models_info's documented
// shape (verified live -- see nebius.go's doc comment and
// nebius_live_test.go), with one single-flavour model and one
// multi-flavour model to exercise region selection.
const nebiusFixture = `[
  {
    "name": "GLM 5.3",
    "flavors": [
      {
        "model_id": "zai-org/GLM-5.3",
        "regions": [{"country_code": "US", "name": "us-north1"}],
        "input_price_per_million_tokens": 1.4,
        "output_price_per_million_tokens": 4.4
      }
    ]
  },
  {
    "name": "Kimi K3",
    "flavors": [
      {
        "model_id": "moonshotai/Kimi-K3",
        "regions": [{"country_code": "FR", "name": "eu-west2"}],
        "input_price_per_million_tokens": 3,
        "output_price_per_million_tokens": 15
      },
      {
        "model_id": "moonshotai/Kimi-K3",
        "regions": [{"country_code": "US", "name": "us-north1"}],
        "input_price_per_million_tokens": 3.2,
        "output_price_per_million_tokens": 16
      }
    ]
  }
]`

func TestNebiusFetch_PicksRegionMatchingFlavor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("nebius Fetch must not send an Authorization header (public endpoint)")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nebiusFixture))
	}))
	defer srv.Close()
	restoreNebiusURL := nebiusModelsInfoURL
	t.Cleanup(func() { nebiusModelsInfoURL = restoreNebiusURL })
	nebiusModelsInfoURL = srv.URL

	provider := &store.ModelCatalogProvider{Slug: "nebius", BaseURL: "https://api.tokenfactory.eu-west2.nebius.com/v1"}
	s, _ := Lookup("nebius")
	res, err := s.Fetch(context.Background(), provider, "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.SourceURL != srv.URL {
		t.Errorf("SourceURL = %q, want %q", res.SourceURL, srv.URL)
	}
	if p, ok := res.Prices["zai-org/GLM-5.3"]; !ok || p.Input != 1.4 || p.Output != 4.4 {
		t.Errorf("Prices[GLM-5.3] = %+v, %v, want {1.4 4.4}", p, ok)
	}
	// eu-west2 base_url -> the eu-west2 flavour (3/15), not us-north1 (3.2/16).
	if p, ok := res.Prices["moonshotai/Kimi-K3"]; !ok || p.Input != 3 || p.Output != 15 {
		t.Errorf("Prices[Kimi-K3] (eu-west2 base_url) = %+v, %v, want the eu-west2 flavour {3 15}", p, ok)
	}
}

func TestNebiusFetch_NoRegionFallsBackToFirstFlavor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(nebiusFixture))
	}))
	defer srv.Close()
	restoreNebiusURL := nebiusModelsInfoURL
	t.Cleanup(func() { nebiusModelsInfoURL = restoreNebiusURL })
	nebiusModelsInfoURL = srv.URL

	// A base_url with no region segment (the generic default host).
	provider := &store.ModelCatalogProvider{Slug: "nebius", BaseURL: "https://api.tokenfactory.nebius.com/v1"}
	s, _ := Lookup("nebius")
	res, err := s.Fetch(context.Background(), provider, "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if p := res.Prices["moonshotai/Kimi-K3"]; p.Input != 3 || p.Output != 15 {
		t.Errorf("Prices[Kimi-K3] (no region) = %+v, want the first flavour {3 15}", p)
	}
}

func TestNebiusRegionOf(t *testing.T) {
	cases := map[string]string{
		"https://api.tokenfactory.eu-west2.nebius.com/v1":  "eu-west2",
		"https://api.tokenfactory.us-north1.nebius.com/v1": "us-north1",
		"https://api.tokenfactory.nebius.com/v1":           "",
		"https://api.openai.com/v1":                        "",
	}
	for url, want := range cases {
		if got := nebiusRegionOf(url); got != want {
			t.Errorf("nebiusRegionOf(%q) = %q, want %q", url, got, want)
		}
	}
}

// togetherFixture mirrors docs.together.ai/reference/models' documented
// GET /v1/models response shape: a flat JSON array, pricing already in USD
// per million tokens.
const togetherFixture = `[
  {
    "id": "deepseek-ai/DeepSeek-V4-Flash-0731",
    "object": "model",
    "pricing": {"input": 0.14, "output": 0.28, "cached_input": 0.07}
  },
  {
    "id": "meta-llama/Llama-3.3-70B-Instruct",
    "object": "model",
    "pricing": {"input": 1.04, "output": 1.04}
  }
]`

func TestTogetherFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer test-key")
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("request path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(togetherFixture))
	}))
	defer srv.Close()

	provider := &store.ModelCatalogProvider{Slug: "together", BaseURL: srv.URL + "/v1"}
	s, _ := Lookup("together")
	res, err := s.Fetch(context.Background(), provider, "test-key")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.SourceURL != srv.URL+"/v1/models" {
		t.Errorf("SourceURL = %q, want %q", res.SourceURL, srv.URL+"/v1/models")
	}
	if p, ok := res.Prices["deepseek-ai/DeepSeek-V4-Flash-0731"]; !ok || p.Input != 0.14 || p.Output != 0.28 || p.CacheRead != 0.07 {
		t.Errorf("Prices[DeepSeek] = %+v, %v, want {0.14 0.28 cache_read=0.07}", p, ok)
	}
	if p, ok := res.Prices["meta-llama/Llama-3.3-70B-Instruct"]; !ok || p.Input != 1.04 || p.Output != 1.04 || p.CacheRead != 0 {
		t.Errorf("Prices[Llama] = %+v, %v, want {1.04 1.04 cache_read=0}", p, ok)
	}
}

func TestTogetherFetch_RequiresAPIKey(t *testing.T) {
	s, _ := Lookup("together")
	provider := &store.ModelCatalogProvider{Slug: "together", BaseURL: "https://api.together.ai/v1"}
	if _, err := s.Fetch(context.Background(), provider, ""); err == nil {
		t.Error("Fetch with an empty api key = nil error, want an error")
	}
}

func TestTogetherFetch_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	provider := &store.ModelCatalogProvider{Slug: "together", BaseURL: srv.URL}
	s, _ := Lookup("together")
	if _, err := s.Fetch(context.Background(), provider, "bad-key"); err == nil {
		t.Error("Fetch against a 401 = nil error, want an error")
	}
}
