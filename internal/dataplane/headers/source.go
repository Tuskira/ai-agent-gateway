package headers

import (
	"context"
	"errors"

	pkgheaders "github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// StoreSourceLookup returns a SourceLookup reading credential sources from
// st. A nil st yields a lookup that finds nothing.
func StoreSourceLookup(st store.CredentialSourceStore) SourceLookup {
	return func(ctx context.Context, tenantID, name string) (*pkgheaders.Source, error) {
		if st == nil {
			return nil, ErrSourceNotFound
		}
		src, err := st.GetByName(ctx, tenantID, name)
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrSourceNotFound
		}
		if err != nil {
			return nil, err
		}
		return SourceFromStore(src), nil
	}
}

// SourceFromStore converts a stored credential source to the provider-facing
// pkg/headers.Source.
func SourceFromStore(src *store.CredentialSource) *pkgheaders.Source {
	cfg := make(map[string]any, len(src.Config))
	for k, v := range src.Config {
		cfg[k] = v
	}
	return &pkgheaders.Source{
		ID:       src.ID,
		TenantID: src.TenantID,
		Name:     src.Name,
		Provider: src.Provider,
		Enabled:  src.Enabled,
		Config:   cfg,
	}
}
