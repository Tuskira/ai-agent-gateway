package prices

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func init() { Register("nebius", nebiusSource{}) }

// nebiusModelsInfoURL is Nebius's PUBLIC pricing feed.
//
// Note the host: "tokenfactory.nebius.com", NOT
// "api.tokenfactory.nebius.com" or "api.tokenfactory.<region>.nebius.com"
// -- the OpenAI-compatible inference hosts every catalog provider's
// base_url points at (see docs/models.md's base URL table). The inference
// host's own /api/public/models_info 404s; docs.tokenfactory.nebius.com's
// api-reference does not document this endpoint at all. Verified live
// (HTTP 200, CORS "Access-Control-Allow-Origin: *", no auth needed) on
// 2026-10-01 -- see nebius_live_test.go.
//
// A var, not a const, so prices_test.go can point it at an httptest server
// instead of the real Nebius host.
var nebiusModelsInfoURL = "https://tokenfactory.nebius.com/api/public/models_info"

const (
	nebiusTimeout = 10 * time.Second
	nebiusMaxBody = 5 << 20 // 5 MiB
)

// nebiusRegion is one flavour's region entry.
type nebiusRegion struct {
	CountryCode string `json:"country_code"`
	Name        string `json:"name"`
}

// nebiusFlavor is one deployable variant of a model -- distinct region
// and/or quantization, each with its own price. Today every model in the
// feed has exactly one flavour, but the shape allows more.
type nebiusFlavor struct {
	ModelID                     string         `json:"model_id"`
	Regions                     []nebiusRegion `json:"regions"`
	InputPricePerMillionTokens  float64        `json:"input_price_per_million_tokens"`
	OutputPricePerMillionTokens float64        `json:"output_price_per_million_tokens"`
}

type nebiusModel struct {
	Flavors []nebiusFlavor `json:"flavors"`
}

type nebiusSource struct{}

func (nebiusSource) RequiresAPIKey() bool { return false }

// Fetch calls nebiusModelsInfoURL (no auth) and reduces each model's
// flavours to the one whose region matches provider.BaseURL's region
// segment, falling back to the first flavour when none match or the feed
// doesn't distinguish regions for that model.
func (nebiusSource) Fetch(ctx context.Context, provider *store.ModelCatalogProvider, _ string) (*Result, error) {
	client := &http.Client{
		Timeout:       nebiusTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ctx, cancel := context.WithTimeout(ctx, nebiusTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nebiusModelsInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("prices: nebius: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prices: nebius: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("prices: nebius: provider responded with HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, nebiusMaxBody))
	if err != nil {
		return nil, fmt.Errorf("prices: nebius: read response: %w", err)
	}
	var models []nebiusModel
	if err := json.Unmarshal(body, &models); err != nil {
		return nil, fmt.Errorf("prices: nebius: unexpected response shape: %w", err)
	}

	region := nebiusRegionOf(provider.BaseURL)
	out := make(map[string]store.ModelPrice)
	for _, m := range models {
		flavor, ok := pickNebiusFlavor(m.Flavors, region)
		if !ok || flavor.ModelID == "" {
			continue
		}
		out[flavor.ModelID] = store.ModelPrice{
			Input:  flavor.InputPricePerMillionTokens,
			Output: flavor.OutputPricePerMillionTokens,
		}
	}
	return &Result{SourceURL: nebiusModelsInfoURL, Prices: out}, nil
}

// nebiusRegionOf extracts the region segment from a Nebius inference
// base_url's host, e.g. "https://api.tokenfactory.eu-west2.nebius.com/v1"
// -> "eu-west2". "" for a base_url with no region segment (e.g. the
// default "https://api.tokenfactory.nebius.com/v1"), in which case Fetch
// falls back to each model's first flavour.
func nebiusRegionOf(baseURL string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	parts := strings.Split(host, ".")
	// api.tokenfactory.<region>.nebius.com
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "tokenfactory" && parts[3] == "nebius" {
		return parts[2]
	}
	return ""
}

// pickNebiusFlavor returns the flavour whose region matches (case-
// insensitive) region, else the first flavour. ok is false only when
// flavors is empty.
func pickNebiusFlavor(flavors []nebiusFlavor, region string) (flavor nebiusFlavor, ok bool) {
	if len(flavors) == 0 {
		return nebiusFlavor{}, false
	}
	if region != "" {
		for _, f := range flavors {
			for _, r := range f.Regions {
				if strings.EqualFold(r.Name, region) {
					return f, true
				}
			}
		}
	}
	return flavors[0], true
}
