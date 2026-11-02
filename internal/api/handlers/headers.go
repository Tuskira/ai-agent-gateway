package handlers

import (
	"net/http"
	"sort"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	pkgheaders "github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
)

// Headers implements GET /api/v1/headers/providers.
type Headers struct{ Deps }

// builtinResolverTypes mirrors the four HeaderResolver types
// dataplaneheaders.NewRegistry always preloads (internal/dataplane/headers:
// static, token_field, incoming_field, external). Registry exposes no
// accessor for these (only Providers(), for ExternalProviders reachable
// through the "external" type), and this handler is read-only against
// that package per this task's scope, so the list is mirrored here rather
// than sourced live. If a fifth built-in resolver type is ever added,
// this list needs a matching update.
var builtinResolverTypes = []string{"static", "token_field", "incoming_field", "external"}

type providerView struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	ConfigSchema map[string]any `json:"config_schema"`
	// SourceSchema and Sources are set only for a provider whose headers
	// name a credential source: the source's config shape, and the
	// caller's tenant's enabled sources for this provider (for a console
	// dropdown).
	SourceSchema map[string]any `json:"source_schema,omitempty"`
	Sources      []string       `json:"sources,omitempty"`
}

type providersResponse struct {
	Providers     []providerView `json:"providers"`
	ResolverTypes []string       `json:"resolver_types"`
}

// List handles GET /api/v1/headers/providers.
func (h Headers) List(w http.ResponseWriter, r *http.Request) {
	providers := h.Headers.Providers()
	views := make([]providerView, 0, len(providers))
	var enabledSources map[string][]string
	for _, p := range providers {
		view := providerView{
			ID:           p.ProviderID(),
			Name:         p.ProviderName(),
			ConfigSchema: p.ConfigSchema(),
		}
		if sp, ok := p.(pkgheaders.SourceSchemaProvider); ok {
			if enabledSources == nil {
				var err error
				if enabledSources, err = h.enabledSources(r); err != nil {
					writeStoreErr(w, r, "list credential sources", err)
					return
				}
			}
			view.SourceSchema = sp.SourceSchema()
			view.Sources = enabledSources[p.ProviderID()]
			if view.Sources == nil {
				view.Sources = []string{}
			}
		}
		views = append(views, view)
	}
	httpx.WriteJSON(w, http.StatusOK, providersResponse{Providers: views, ResolverTypes: builtinResolverTypes})
}

// enabledSources returns the caller's tenant's enabled credential source
// names, by provider id, sorted.
func (h Headers) enabledSources(r *http.Request) (map[string][]string, error) {
	out := map[string][]string{}
	tid, ok := tenantID(r)
	if !ok || h.Store == nil {
		return out, nil
	}
	srcs, err := h.Store.CredentialSources().List(r.Context(), tid)
	if err != nil {
		return nil, err
	}
	for _, s := range srcs {
		if s.Enabled {
			out[s.Provider] = append(out[s.Provider], s.Name)
		}
	}
	for _, names := range out {
		sort.Strings(names)
	}
	return out, nil
}
