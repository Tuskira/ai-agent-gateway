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
	// NeedsSource is true for a provider whose headers must name a
	// credential source ("source" next to "provider"). For those,
	// SourceSchema is the source's config shape and Sources the caller's
	// tenant's enabled sources for this provider (for a console dropdown).
	NeedsSource  bool           `json:"needs_source"`
	SourceSchema map[string]any `json:"source_schema,omitempty"`
	Sources      []string       `json:"sources,omitempty"`
	// SourceRefs is Sources with each source's id, which
	// GET /credential-sources/{id}/options needs and which a caller
	// without admin.manage cannot get from GET /credential-sources.
	SourceRefs []sourceRef `json:"source_refs,omitempty"`
	// OptionFields names the config_schema fields the provider can list
	// values for through GET /credential-sources/{id}/options.
	OptionFields []string `json:"option_fields,omitempty"`
}

type sourceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type providersResponse struct {
	Providers     []providerView `json:"providers"`
	ResolverTypes []string       `json:"resolver_types"`
}

// List handles GET /api/v1/headers/providers.
func (h Headers) List(w http.ResponseWriter, r *http.Request) {
	providers := h.Headers.Providers()
	views := make([]providerView, 0, len(providers))
	var enabledSources map[string][]sourceRef
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
			view.NeedsSource = true
			view.SourceSchema = sp.SourceSchema()
			if ol, ok := p.(pkgheaders.SourceOptionLister); ok {
				view.OptionFields = ol.OptionFields()
			}
			view.Sources = []string{}
			view.SourceRefs = []sourceRef{}
			for _, ref := range enabledSources[p.ProviderID()] {
				view.Sources = append(view.Sources, ref.Name)
				view.SourceRefs = append(view.SourceRefs, ref)
			}
		}
		views = append(views, view)
	}
	httpx.WriteJSON(w, http.StatusOK, providersResponse{Providers: views, ResolverTypes: builtinResolverTypes})
}

// enabledSources returns the caller's tenant's enabled credential
// sources, by provider id, sorted by name.
func (h Headers) enabledSources(r *http.Request) (map[string][]sourceRef, error) {
	out := map[string][]sourceRef{}
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
			out[s.Provider] = append(out[s.Provider], sourceRef{ID: s.ID, Name: s.Name})
		}
	}
	for _, refs := range out {
		sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	}
	return out, nil
}
