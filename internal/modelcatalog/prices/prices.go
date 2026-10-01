// Package prices fetches a model catalog provider's CURRENT prices from
// that provider's own pricing API -- never a guess, never pkg/pricing's
// static rate card (which prices actual LLM-plane calls against a vendor
// wire format, a different concern). It backs
// POST /api/v1/model-catalog/providers/{id}/prices/preview and .../apply
// (internal/api/handlers/modelcatalog.go).
//
// A Source is registered per CATALOG PROVIDER SLUG ("nebius", "together"),
// not per vendor: both are vendor "openai_compat" (see
// store.ModelCatalogProvider.Vendor) but have entirely unrelated pricing
// APIs. A provider slug with no registered Source (every provider besides
// the two above, today) is simply unsupported -- the handler reports that
// with a 400, not a panic or a guessed price.
package prices

import (
	"context"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Result is one Source.Fetch call's outcome.
type Result struct {
	// SourceURL is the exact URL fetched -- no key, no query secret --
	// echoed back in the preview response for transparency.
	SourceURL string
	// Prices is keyed by the vendor's own model id
	// (store.ModelCatalogModel.ModelID, e.g. "zai-org/GLM-5.3"), never the
	// catalog row's uuid. A model_id absent from this map means the
	// provider's pricing feed didn't mention it.
	Prices map[string]store.ModelPrice
}

// Source fetches a catalog provider's current prices from the provider's
// OWN pricing API.
type Source interface {
	// Fetch calls the provider's pricing endpoint. provider carries both
	// Slug and BaseURL -- Nebius uses BaseURL's region to pick the right
	// flavour when its feed distinguishes regions. apiKey is "" when the
	// source's endpoint needs none (see RequiresAPIKey); passing one
	// anyway is harmless.
	Fetch(ctx context.Context, provider *store.ModelCatalogProvider, apiKey string) (*Result, error)
	// RequiresAPIKey reports whether Fetch needs a non-empty apiKey. The
	// handler uses this to decide whether a request with neither
	// credential nor api_key is a 400 or a legitimate "this source is
	// public" call.
	RequiresAPIKey() bool
}

// registry maps a catalog provider slug to its Source. Populated by each
// source file's init() -- see nebius.go, together.go.
var registry = map[string]Source{}

// Register adds a Source for a catalog provider slug. Not safe for
// concurrent use with Lookup; called only from init().
func Register(slug string, s Source) { registry[slug] = s }

// Lookup returns the Source registered for slug, if any.
func Lookup(slug string) (Source, bool) {
	s, ok := registry[slug]
	return s, ok
}

// RegisterForTest swaps in s for slug and returns a func restoring
// whatever was registered before (or removing slug entirely if nothing
// was). For OTHER PACKAGES' tests only -- e.g.
// internal/api/handlers' PreviewPrices/ApplyPrices handler tests, which
// want "nebius" or "together" to hit an httptest.Server without
// reimplementing nebius.go/together.go's own parsing in the test double.
// Not safe for concurrent use; production code never calls this.
func RegisterForTest(slug string, s Source) (restore func()) {
	prev, had := registry[slug]
	registry[slug] = s
	return func() {
		if had {
			registry[slug] = prev
		} else {
			delete(registry, slug)
		}
	}
}
