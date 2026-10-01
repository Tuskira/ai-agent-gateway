package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// RunMCPCatalog runs only the MCP catalog conformance checks. The
// in-memory store in internal/dataplane/dptest is not run against the full
// suite, but its catalog facet is shared with the handler tests, so it
// gets this subset.
func RunMCPCatalog(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()
	t.Run("MCPCatalog", func(t *testing.T) { testMCPCatalog(t, newStore(t)) })
}

func catalogSlugs(list []*store.MCPCatalogEntry, prefix string) map[string]*store.MCPCatalogEntry {
	out := map[string]*store.MCPCatalogEntry{}
	for _, e := range list {
		if len(e.Slug) >= len(prefix) && e.Slug[:len(prefix)] == prefix {
			out[e.Slug] = e
		}
	}
	return out
}

func testMCPCatalog(t *testing.T, s store.Store) {
	ctx := context.Background()
	cat := s.MCPCatalog()
	tenantA := mustCreateTenant(t, ctx, s, "cat-a")
	tenantB := mustCreateTenant(t, ctx, s, "cat-b")

	pfx := unique("cat") + "-"
	slug := pfx + "main"
	e := &store.MCPCatalogEntry{
		Slug: slug, Name: "Example", Description: "An example server", Icon: "plug", Category: "testing",
		URL: "https://mcp.example.com/mcp", URLOverridable: true, Enabled: true,
		Auth: store.MCPCatalogAuth{
			Kind: "basic",
			Fields: []store.MCPCatalogField{
				{Name: "user", Label: "User", Required: true},
				{Name: "pass", Label: "Password", Secret: true, Required: true},
			},
		},
		DefaultHeaders: map[string]string{"X-Client": "gateway"},
		SuggestedTools: []string{"search"},
		DocsURL:        "https://example.com/docs",
	}

	// --- platform row: Upsert create, round-trip, rewrite in place ---
	if err := cat.Upsert(ctx, e); err != nil {
		t.Fatalf("Upsert(create): %v", err)
	}
	if e.ID == "" {
		t.Fatal("Upsert(create): ID not populated")
	}
	got, err := cat.GetBySlug(ctx, "", slug)
	if err != nil {
		t.Fatalf("GetBySlug(platform): %v", err)
	}
	if got.ID != e.ID || got.TenantID != "" || got.Transport != "streamable-http" || !got.URLOverridable || !got.Enabled {
		t.Errorf("GetBySlug = %+v", got)
	}
	if got.Auth.Kind != "basic" || len(got.Auth.Fields) != 2 || !got.Auth.Fields[1].Secret || !got.Auth.Fields[0].Required {
		t.Errorf("Auth did not round-trip: %+v", got.Auth)
	}
	if got.DefaultHeaders["X-Client"] != "gateway" || len(got.SuggestedTools) != 1 || got.DocsURL != "https://example.com/docs" {
		t.Errorf("headers/tools/docs did not round-trip: %+v", got)
	}

	e2 := &store.MCPCatalogEntry{Slug: slug, Name: "Renamed", URL: "https://mcp.example.com/v2", Enabled: false}
	if err := cat.Upsert(ctx, e2); err != nil {
		t.Fatalf("Upsert(update): %v", err)
	}
	if e2.ID != e.ID {
		t.Errorf("Upsert(update) changed id: %q -> %q", e.ID, e2.ID)
	}
	got, _ = cat.GetBySlug(ctx, "", slug)
	if got.Name != "Renamed" || got.URL != "https://mcp.example.com/v2" || got.Enabled || got.Auth.Kind != "none" {
		t.Errorf("after update = %+v", got)
	}
	// Restore for the visibility checks below.
	e2.Enabled, e2.Name = true, "Platform"
	if err := cat.Upsert(ctx, e2); err != nil {
		t.Fatal(err)
	}

	// --- visibility: every tenant sees the platform row; tenant rows are private ---
	for _, tid := range []string{tenantA.ID, tenantB.ID, ""} {
		if g, err := cat.GetBySlug(ctx, tid, slug); err != nil || g.ID != e.ID {
			t.Errorf("GetBySlug(%q, platform slug) = %v, %v", tid, g, err)
		}
		if g, err := cat.Get(ctx, tid, e.ID); err != nil || g.Slug != slug {
			t.Errorf("Get(%q, platform id) = %v, %v", tid, g, err)
		}
	}

	ta := &store.MCPCatalogEntry{TenantID: tenantA.ID, Slug: pfx + "a-only", Name: "A only", URL: "https://a.example.com/mcp", Enabled: true}
	if err := cat.Create(ctx, ta); err != nil {
		t.Fatalf("Create(tenant row): %v", err)
	}
	if ta.ID == "" || ta.TenantID != tenantA.ID {
		t.Fatalf("Create did not populate: %+v", ta)
	}
	if err := cat.Create(ctx, &store.MCPCatalogEntry{TenantID: tenantA.ID, Slug: pfx + "a-only", Name: "dup", URL: "https://x.example.com"}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate slug in one tenant = %v, want ErrConflict", err)
	}
	if err := cat.Create(ctx, &store.MCPCatalogEntry{Slug: slug, Name: "dup platform", URL: "https://x.example.com"}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate platform slug = %v, want ErrConflict", err)
	}
	if _, err := cat.GetBySlug(ctx, tenantB.ID, pfx+"a-only"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("tenant B reads tenant A's row: %v", err)
	}
	if _, err := cat.Get(ctx, tenantB.ID, ta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("tenant B Get tenant A's id: %v", err)
	}
	if _, err := cat.GetBySlug(ctx, "", pfx+"a-only"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("platform scope sees a tenant row: %v", err)
	}

	// --- tenant row shadows the platform row of the same slug, for that tenant only ---
	shadow := &store.MCPCatalogEntry{TenantID: tenantA.ID, Slug: slug, Name: "A's version", URL: "https://a.example.com/own", Enabled: true}
	if err := cat.Create(ctx, shadow); err != nil {
		t.Fatalf("Create(shadow): %v", err)
	}
	if g, _ := cat.GetBySlug(ctx, tenantA.ID, slug); g.ID != shadow.ID || g.Name != "A's version" {
		t.Errorf("tenant A GetBySlug = %+v, want its own row", g)
	}
	if g, _ := cat.GetBySlug(ctx, tenantB.ID, slug); g.ID != e.ID {
		t.Errorf("tenant B GetBySlug = %+v, want the platform row", g)
	}
	listA, _ := cat.List(ctx, tenantA.ID)
	bySlugA := catalogSlugs(listA, pfx)
	if len(bySlugA) != 2 || bySlugA[slug].ID != shadow.ID || bySlugA[pfx+"a-only"] == nil {
		t.Errorf("List(A) = %+v", bySlugA)
	}
	listB, _ := cat.List(ctx, tenantB.ID)
	bySlugB := catalogSlugs(listB, pfx)
	if len(bySlugB) != 1 || bySlugB[slug].ID != e.ID {
		t.Errorf("List(B) = %+v, want only the platform row", bySlugB)
	}
	listP, _ := cat.List(ctx, "")
	if p := catalogSlugs(listP, pfx); len(p) != 1 || p[slug].ID != e.ID {
		t.Errorf("List(platform) = %+v, want only the platform row", p)
	}

	// --- Update is scoped to the owning tenant (or platform) and keeps the slug ---
	upd := *ta
	upd.Name, upd.URL, upd.Slug = "A renamed", "https://a.example.com/v2", "ignored"
	if err := cat.Update(ctx, &upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if g, _ := cat.Get(ctx, tenantA.ID, ta.ID); g.Name != "A renamed" || g.URL != "https://a.example.com/v2" || g.Slug != pfx+"a-only" {
		t.Errorf("after Update = %+v", g)
	}
	wrong := upd
	wrong.TenantID = tenantB.ID
	if err := cat.Update(ctx, &wrong); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Update as another tenant = %v, want ErrNotFound", err)
	}
	asPlatform := upd
	asPlatform.TenantID = ""
	if err := cat.Update(ctx, &asPlatform); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Update of a tenant row as platform = %v, want ErrNotFound", err)
	}

	// --- a connector can point at an entry ---
	c := &store.Connector{
		TenantID: tenantA.ID, Name: "From catalog", Slug: unique("fromcat"), Endpoint: "https://mcp.example.com/v2",
		TimeoutMS: 30000, CatalogID: e.ID,
	}
	if err := s.Connectors().Create(ctx, c); err != nil {
		t.Fatalf("create connector with catalog_id: %v", err)
	}
	if gc, err := s.Connectors().Get(ctx, tenantA.ID, c.ID); err != nil || gc.CatalogID != e.ID {
		t.Fatalf("connector CatalogID = %v (err %v), want %q", gc, err, e.ID)
	}
	custom := &store.Connector{TenantID: tenantA.ID, Name: "Custom", Slug: unique("custom"), Endpoint: "https://x.example.com", TimeoutMS: 30000}
	if err := s.Connectors().Create(ctx, custom); err != nil {
		t.Fatalf("create custom connector: %v", err)
	}
	if gc, _ := s.Connectors().Get(ctx, tenantA.ID, custom.ID); gc.CatalogID != "" {
		t.Errorf("custom connector CatalogID = %q, want empty", gc.CatalogID)
	}

	// --- soft delete is scoped and frees the slug ---
	if err := cat.SoftDelete(ctx, tenantB.ID, ta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SoftDelete as another tenant = %v, want ErrNotFound", err)
	}
	if err := cat.SoftDelete(ctx, tenantA.ID, e.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SoftDelete of a platform row as a tenant = %v, want ErrNotFound", err)
	}
	if err := cat.SoftDelete(ctx, tenantA.ID, shadow.ID); err != nil {
		t.Fatalf("SoftDelete(shadow): %v", err)
	}
	if g, _ := cat.GetBySlug(ctx, tenantA.ID, slug); g.ID != e.ID {
		t.Errorf("after deleting the shadow, tenant A sees %+v, want the platform row again", g)
	}
	if err := cat.SoftDelete(ctx, tenantA.ID, shadow.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second SoftDelete = %v, want ErrNotFound", err)
	}
	if err := cat.Create(ctx, &store.MCPCatalogEntry{TenantID: tenantA.ID, Slug: slug, Name: "again", URL: "https://a.example.com/again"}); err != nil {
		t.Errorf("Create after soft delete (slug freed): %v", err)
	}
	if err := cat.SoftDelete(ctx, "", e.ID); err != nil {
		t.Fatalf("SoftDelete(platform): %v", err)
	}
	if _, err := cat.GetBySlug(ctx, tenantB.ID, slug); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetBySlug after platform delete = %v, want ErrNotFound", err)
	}
	re := &store.MCPCatalogEntry{Slug: slug, Name: "Again", URL: "https://mcp.example.com/mcp", Enabled: true}
	if err := cat.Upsert(ctx, re); err != nil {
		t.Fatalf("Upsert after delete: %v", err)
	}
	if re.ID == e.ID {
		t.Error("Upsert after soft delete reused the deleted row's id")
	}
}
