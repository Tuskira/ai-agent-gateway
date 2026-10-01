package handlers

import (
	"net/http"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
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
}

type providersResponse struct {
	Providers     []providerView `json:"providers"`
	ResolverTypes []string       `json:"resolver_types"`
}

// List handles GET /api/v1/headers/providers.
func (h Headers) List(w http.ResponseWriter, r *http.Request) {
	providers := h.Headers.Providers()
	views := make([]providerView, 0, len(providers))
	for _, p := range providers {
		views = append(views, providerView{
			ID:           p.ProviderID(),
			Name:         p.ProviderName(),
			ConfigSchema: p.ConfigSchema(),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, providersResponse{Providers: views, ResolverTypes: builtinResolverTypes})
}
