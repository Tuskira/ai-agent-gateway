package config

import (
	"time"

	"context"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard/netguardtest"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateMCPCatalogSeed(t *testing.T) {
	field := func(name string) MCPCatalogField { return MCPCatalogField{Name: name, Label: name} }
	ok := MCPCatalogSeed{Slug: "x", Name: "X", URL: "https://x.example.com/mcp", Auth: MCPCatalogAuth{Kind: "none"}}

	cases := []struct {
		name   string
		mutate func(*MCPCatalogSeed)
		ok     bool
	}{
		{"valid none", func(*MCPCatalogSeed) {}, true},
		{"bad slug", func(s *MCPCatalogSeed) { s.Slug = "Has Space" }, false},
		{"http url", func(s *MCPCatalogSeed) { s.URL = "http://x.example.com" }, false},
		{"unknown kind", func(s *MCPCatalogSeed) { s.Auth.Kind = "magic" }, false},
		{"empty default header", func(s *MCPCatalogSeed) { s.DefaultHeaders = map[string]string{"X-A": ""} }, false},
		{"line break in default header", func(s *MCPCatalogSeed) { s.DefaultHeaders = map[string]string{"X-A": "v\nX-B: 1"} }, false},
		{"none with credential field", func(s *MCPCatalogSeed) { s.Auth.Fields = []MCPCatalogField{field("a")} }, false},
		{"bearer needs one field", func(s *MCPCatalogSeed) { s.Auth.Kind = "bearer" }, false},
		{"bearer with one", func(s *MCPCatalogSeed) { s.Auth.Kind, s.Auth.Fields = "bearer", []MCPCatalogField{field("t")} }, true},
		{"basic needs two", func(s *MCPCatalogSeed) { s.Auth.Kind, s.Auth.Fields = "basic", []MCPCatalogField{field("a")} }, false},
		{"basic with two", func(s *MCPCatalogSeed) {
			s.Auth.Kind, s.Auth.Fields = "basic", []MCPCatalogField{field("a"), field("b")}
		}, true},
		{"query field is not a credential", func(s *MCPCatalogSeed) {
			q := field("p")
			q.Query = "project"
			s.Auth.Fields = []MCPCatalogField{q}
		}, true},
		{"secret query field", func(s *MCPCatalogSeed) {
			q := field("p")
			q.Query, q.Secret = "project", true
			s.Auth.Fields = []MCPCatalogField{q}
		}, false},
		{"duplicate field", func(s *MCPCatalogSeed) {
			s.Auth.Kind, s.Auth.Fields = "basic", []MCPCatalogField{field("a"), field("a")}
		}, false},
		{"header kind needs a header name", func(s *MCPCatalogSeed) { s.Auth.Kind, s.Auth.Fields = "header", []MCPCatalogField{field("k")} }, false},
		{"header kind with name", func(s *MCPCatalogSeed) {
			s.Auth.Kind, s.Auth.Fields = "header", []MCPCatalogField{field("k")}
			s.Auth.HeaderTemplate = &MCPCatalogHeaderSpec{Name: "X-Api-Key"}
		}, true},
		{"oauth without fields", func(s *MCPCatalogSeed) { s.Auth.Kind = "oauth" }, true},
		{"bad default header", func(s *MCPCatalogSeed) { s.DefaultHeaders = map[string]string{"Bad Name": "v"} }, false},
	}
	for _, tc := range cases {
		s := ok
		tc.mutate(&s)
		if err := ValidateMCPCatalogSeed(context.Background(), s); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestValidate_MCPCatalogDuplicateSlug(t *testing.T) {
	cfg := Default()
	s := MCPCatalogSeed{Slug: "x", Name: "X", URL: "https://x.example.com", Auth: MCPCatalogAuth{Kind: "none"}}
	cfg.MCPCatalog.Seed = []MCPCatalogSeed{s, s}
	if err := cfg.Validate(); err == nil {
		t.Error("duplicate slug passed validation")
	}
}

// TestLoad_MCPCatalogShippedByDefault: the shipped catalog is seeded
// without a config file, a file that leaves mcp_catalog out keeps it, and
// `seed: []` turns it off.
func TestLoad_MCPCatalogShippedByDefault(t *testing.T) {
	load := func(yml string) []MCPCatalogSeed {
		t.Helper()
		path := ""
		if yml != "" {
			path = filepath.Join(t.TempDir(), "c.yml")
			if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return cfg.MCPCatalog.Seed
	}
	if got := load(""); len(got) != 10 || got[0].Slug != "langfuse" {
		t.Errorf("no file: %d entries, want the 10 shipped", len(got))
	}
	if got := load("logging:\n  level: info\n"); len(got) != 10 {
		t.Errorf("file without mcp_catalog: %d entries, want 10", len(got))
	}
	if got := load("mcp_catalog:\n  seed: []\n"); len(got) != 0 {
		t.Errorf("seed: []: %d entries, want 0", len(got))
	}
}

// With DNS hanging, validating the shipped catalog seed (every config load,
// so every gateway command) is bounded by one lookup budget, not one per URL.
func TestLoad_SeedDNSIsBoundedAsAWhole(t *testing.T) {
	defer netguardtest.HangDNS()()
	start := time.Now()
	if _, err := Load(""); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("Load took %v with DNS hanging, want about one 2s lookup budget", d)
	}
}
