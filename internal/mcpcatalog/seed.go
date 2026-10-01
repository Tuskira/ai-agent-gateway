// Package mcpcatalog seeds the platform MCP catalog from configuration.
package mcpcatalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Seed upserts cfg's mcp_catalog.seed entries into the catalog by slug: an
// existing live entry of the same slug is rewritten to match the file, a
// missing one is created. Entries the file does not mention are left alone
// -- the file is a seed, not the source of truth -- so an operator can add
// entries another way without the next restart deleting them. Idempotent;
// every replica may run it at boot. Returns how many entries were created
// and how many already existed (and were rewritten).
func Seed(ctx context.Context, cat store.MCPCatalogStore, seeds []config.MCPCatalogSeed, logger *slog.Logger) (created, updated int, err error) {
	for _, seed := range seeds {
		e := toEntry(seed)
		_, getErr := cat.GetBySlug(ctx, "", seed.Slug)
		exists := getErr == nil
		if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
			return created, updated, fmt.Errorf("seed mcp catalog %q: look up: %w", seed.Slug, getErr)
		}
		if err := cat.Upsert(ctx, e); err != nil {
			return created, updated, fmt.Errorf("seed mcp catalog %q: %w", seed.Slug, err)
		}
		if exists {
			updated++
		} else {
			created++
		}
	}
	if logger != nil && len(seeds) > 0 {
		logger.Info("mcp catalog seeded from config", "created", created, "updated", updated)
	}
	return created, updated, nil
}

func toEntry(s config.MCPCatalogSeed) *store.MCPCatalogEntry {
	e := &store.MCPCatalogEntry{
		Slug: s.Slug, Name: s.Name, Description: s.Description, Icon: s.Icon, Category: s.Category,
		URL: s.URL, URLOverridable: s.URLOverridable, Transport: s.Transport,
		DefaultHeaders: s.DefaultHeaders, SuggestedTools: s.SuggestedTools, DocsURL: s.DocsURL,
		Enabled: !s.Disabled,
		Auth:    store.MCPCatalogAuth{Kind: s.Auth.Kind, Fields: make([]store.MCPCatalogField, 0, len(s.Auth.Fields))},
	}
	for _, f := range s.Auth.Fields {
		e.Auth.Fields = append(e.Auth.Fields, store.MCPCatalogField(f))
	}
	if ht := s.Auth.HeaderTemplate; ht != nil {
		e.Auth.HeaderTemplate = &store.MCPCatalogHeader{Name: ht.Name, Prefix: ht.Prefix}
	}
	return e
}
