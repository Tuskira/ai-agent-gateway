package dptest

import (
	"context"
	"sort"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// MCPCatalog implements store.Store with a full in-memory catalog that
// mirrors the semantics pkg/store/storetest asserts (platform + tenant
// rows, tenant wins on a shared slug, slug unique among live rows per
// scope, soft delete hides).
func (s *Store) MCPCatalog() store.MCPCatalogStore { return (*mcpCatalogStore)(s) }

type mcpCatalogStore Store

func normalizeCatalogEntry(e *store.MCPCatalogEntry) {
	if e.Transport == "" {
		e.Transport = "streamable-http"
	}
	if e.Auth.Kind == "" {
		e.Auth.Kind = "none"
	}
	if e.Auth.Fields == nil {
		e.Auth.Fields = []store.MCPCatalogField{}
	}
	if e.DefaultHeaders == nil {
		e.DefaultHeaders = map[string]string{}
	}
	if e.SuggestedTools == nil {
		e.SuggestedTools = []string{}
	}
}

// visibleTo reports whether tenantID reads e: its own row or a platform one.
func visibleTo(e *store.MCPCatalogEntry, tenantID string) bool {
	return e.DeletedAt == nil && (e.TenantID == "" || e.TenantID == tenantID)
}

// liveInScope finds the live row with slug owned by exactly tenantID.
// Callers hold s.mu.
func (s *Store) liveInScope(tenantID, slug string) *store.MCPCatalogEntry {
	for _, e := range s.catalog {
		if e.DeletedAt == nil && e.TenantID == tenantID && e.Slug == slug {
			return e
		}
	}
	return nil
}

func (c *mcpCatalogStore) List(_ context.Context, tenantID string) ([]*store.MCPCatalogEntry, error) {
	s := (*Store)(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	bySlug := map[string]*store.MCPCatalogEntry{}
	for _, e := range s.catalog {
		if !visibleTo(e, tenantID) {
			continue
		}
		if cur, ok := bySlug[e.Slug]; !ok || (cur.TenantID == "" && e.TenantID != "") {
			bySlug[e.Slug] = e
		}
	}
	out := make([]*store.MCPCatalogEntry, 0, len(bySlug))
	for _, e := range bySlug {
		cp := *e
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Slug < out[j].Slug
	})
	return out, nil
}

func (c *mcpCatalogStore) Get(_ context.Context, tenantID, id string) (*store.MCPCatalogEntry, error) {
	s := (*Store)(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.catalog[id]
	if !ok || !visibleTo(e, tenantID) {
		return nil, store.ErrNotFound
	}
	cp := *e
	return &cp, nil
}

func (c *mcpCatalogStore) GetBySlug(_ context.Context, tenantID, slug string) (*store.MCPCatalogEntry, error) {
	s := (*Store)(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	if tenantID != "" {
		if e := s.liveInScope(tenantID, slug); e != nil {
			cp := *e
			return &cp, nil
		}
	}
	if e := s.liveInScope("", slug); e != nil {
		cp := *e
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (c *mcpCatalogStore) Create(_ context.Context, e *store.MCPCatalogEntry) error {
	s := (*Store)(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveInScope(e.TenantID, e.Slug) != nil {
		return store.ErrConflict
	}
	s.insertCatalog(e)
	return nil
}

func (s *Store) insertCatalog(e *store.MCPCatalogEntry) {
	normalizeCatalogEntry(e)
	now := time.Now().UTC()
	e.ID = s.id("catalog")
	e.CreatedAt, e.UpdatedAt = now, now
	e.DeletedAt = nil
	cp := *e
	s.catalog[e.ID] = &cp
}

func (c *mcpCatalogStore) Upsert(_ context.Context, e *store.MCPCatalogEntry) error {
	s := (*Store)(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	existing := s.liveInScope(e.TenantID, e.Slug)
	if existing == nil {
		s.insertCatalog(e)
		return nil
	}
	normalizeCatalogEntry(e)
	id, created := existing.ID, existing.CreatedAt
	*existing = *e
	existing.ID, existing.CreatedAt, existing.UpdatedAt, existing.DeletedAt = id, created, time.Now().UTC(), nil
	e.ID, e.CreatedAt, e.UpdatedAt = id, created, existing.UpdatedAt
	return nil
}

func (c *mcpCatalogStore) Update(_ context.Context, e *store.MCPCatalogEntry) error {
	s := (*Store)(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.catalog[e.ID]
	if !ok || existing.DeletedAt != nil || existing.TenantID != e.TenantID {
		return store.ErrNotFound
	}
	normalizeCatalogEntry(e)
	slug, created := existing.Slug, existing.CreatedAt
	*existing = *e
	existing.Slug, existing.CreatedAt, existing.UpdatedAt = slug, created, time.Now().UTC()
	e.Slug, e.UpdatedAt = slug, existing.UpdatedAt
	return nil
}

func (c *mcpCatalogStore) SoftDelete(_ context.Context, tenantID, id string) error {
	s := (*Store)(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.catalog[id]
	if !ok || e.DeletedAt != nil || e.TenantID != tenantID {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	e.DeletedAt = &now
	return nil
}
