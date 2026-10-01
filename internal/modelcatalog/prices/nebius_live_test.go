package prices

import (
	"context"
	"os"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// TestNebiusFetchLive calls the REAL, public
// https://tokenfactory.nebius.com/api/public/models_info -- no key needed,
// since the nebius Source requires none. Gated on NEBIUS_LIVE=1 (not a key
// env var, since this source is public) so a normal `go test ./...` run
// never depends on network access. Run with:
//
//	NEBIUS_LIVE=1 go test ./internal/modelcatalog/prices/... -run TestNebiusFetchLive -v
func TestNebiusFetchLive(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	liveGate(t)

	provider := &store.ModelCatalogProvider{
		Slug: "nebius", BaseURL: "https://api.tokenfactory.eu-west2.nebius.com/v1",
	}
	s, ok := Lookup("nebius")
	if !ok {
		t.Fatal(`Lookup("nebius") = false, want a registered Source`)
	}
	res, err := s.Fetch(context.Background(), provider, "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	t.Logf("fetched %d priced models from %s", len(res.Prices), res.SourceURL)

	for _, modelID := range []string{"moonshotai/Kimi-K3", "zai-org/GLM-5.3"} {
		p, ok := res.Prices[modelID]
		if !ok {
			t.Errorf("Prices[%q] missing from the live feed", modelID)
			continue
		}
		t.Logf("%s: input=$%.2f output=$%.2f per 1M tokens", modelID, p.Input, p.Output)
	}
}

func liveGate(t *testing.T) {
	t.Helper()
	if os.Getenv("NEBIUS_LIVE") != "1" {
		t.Skip("NEBIUS_LIVE not set to 1; skipping live Nebius pricing test")
	}
}
