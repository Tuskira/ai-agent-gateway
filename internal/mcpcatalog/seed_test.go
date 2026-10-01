package mcpcatalog

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func seeds() []config.MCPCatalogSeed {
	return []config.MCPCatalogSeed{
		{Slug: "a", Name: "A", URL: "https://a.example.com/mcp", Auth: config.MCPCatalogAuth{Kind: "none"}},
		{Slug: "b", Name: "B", URL: "https://b.example.com/mcp", URLOverridable: true, Auth: config.MCPCatalogAuth{
			Kind: "bearer", Fields: []config.MCPCatalogField{{Name: "token", Label: "Token", Secret: true, Required: true}},
		}},
	}
}

func TestSeed_IdempotentUpsertByslug(t *testing.T) {
	ctx := context.Background()
	st := dptest.New()
	cat := st.MCPCatalog()

	created, updated, err := Seed(ctx, cat, seeds(), nil)
	if err != nil || created != 2 || updated != 0 {
		t.Fatalf("first seed = %d/%d err %v, want 2/0", created, updated, err)
	}
	a, _ := cat.GetBySlug(ctx, "", "a")

	// A second run neither duplicates rows nor changes ids.
	created, updated, err = Seed(ctx, cat, seeds(), nil)
	if err != nil || created != 0 || updated != 2 {
		t.Fatalf("second seed = %d/%d err %v, want 0/2", created, updated, err)
	}
	if list, _ := cat.List(ctx, ""); len(list) != 2 {
		t.Fatalf("list has %d entries after re-seed, want 2", len(list))
	}
	if a2, _ := cat.GetBySlug(ctx, "", "a"); a2.ID != a.ID {
		t.Error("re-seed changed a row's id")
	}

	// Changing the file rewrites the row; unmentioned rows are left alone.
	changed := seeds()[:1]
	changed[0].Name = "A renamed"
	changed[0].Disabled = true
	if _, _, err := Seed(ctx, cat, changed, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := cat.GetBySlug(ctx, "", "a")
	if got.Name != "A renamed" || got.Enabled {
		t.Errorf("row not rewritten: %+v", got)
	}
	if b, err := cat.GetBySlug(ctx, "", "b"); err != nil || b.Auth.Kind != "bearer" || !b.URLOverridable || len(b.Auth.Fields) != 1 {
		t.Errorf("unmentioned row changed or lost: %+v (err %v)", b, err)
	}
}

// A seed rewrites platform rows only: a tenant's own entry with the same
// slug is never touched.
func TestSeed_LeavesTenantRowsAlone(t *testing.T) {
	ctx := context.Background()
	st := dptest.New()
	cat := st.MCPCatalog()
	own := &store.MCPCatalogEntry{TenantID: "tenant-1", Slug: "a", Name: "Ours", URL: "https://ours.example.com", Enabled: true}
	if err := cat.Create(ctx, own); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Seed(ctx, cat, seeds(), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Seed(ctx, cat, seeds(), nil); err != nil {
		t.Fatal(err)
	}
	if g, _ := cat.GetBySlug(ctx, "tenant-1", "a"); g.ID != own.ID || g.Name != "Ours" {
		t.Errorf("tenant row changed by seed: %+v", g)
	}
	if g, _ := cat.GetBySlug(ctx, "", "a"); g.TenantID != "" || g.Name != "A" {
		t.Errorf("platform row = %+v", g)
	}
}

// TestShippedDefaultSeedLoads loads configs/base/default.yml through the
// real loader, which validates the seed, and checks the shipped entries.
func TestShippedDefaultSeedLoads(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "base", "default.yml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("default.yml not found: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		// Load may need env (database DSN etc.); the seed itself is what matters here.
		t.Skipf("config.Load: %v", err)
	}
	want := map[string]string{"langfuse": "basic", "supabase": "bearer", "drawio": "none", "google-drive": "oauth", "atlassian": "basic",
		"context7": "bearer", "github": "bearer", "stripe": "bearer", "huggingface": "bearer", "deepwiki": "none"}
	got := map[string]string{}
	for _, s := range cfg.MCPCatalog.Seed {
		got[s.Slug] = s.Auth.Kind
		if err := config.ValidateMCPCatalogSeed(context.Background(), s); err != nil {
			t.Errorf("%s: %v", s.Slug, err)
		}
	}
	for slug, kind := range want {
		if got[slug] != kind {
			t.Errorf("seed %s kind = %q, want %q", slug, got[slug], kind)
		}
	}
}
