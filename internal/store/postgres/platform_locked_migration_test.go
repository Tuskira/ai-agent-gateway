package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// TestPlatformLockedMigration_StripsAllowLockedFromHeaders: a header saved
// before 000016 could carry any key, including the server-owned
// "allow_locked" stamp that lets a header resolve a platform-locked
// credential. The migration removes it from every stored header and leaves
// everything else as it was; running it again changes nothing.
func TestPlatformLockedMigration_StripsAllowLockedFromHeaders(t *testing.T) {
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping Postgres migration test")
	}
	ctx := context.Background()

	schema := "gwtest_plocked_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := sql.Open("pgx", rawURL)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema %q: %v", schema, err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema))
		admin.Close()
	})
	scopedURL, err := withSearchPath(rawURL, schema)
	if err != nil {
		t.Fatalf("build scoped DSN: %v", err)
	}
	db, err := sql.Open("pgx", scopedURL)
	if err != nil {
		t.Fatalf("open scoped connection: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	st := postgres.New(db)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tn := &store.Tenant{Slug: "plocked", Name: "Plocked"}
	if err := st.Tenants().Create(ctx, tn); err != nil {
		t.Fatal(err)
	}
	// Rows as an older gateway could have stored them.
	planted := &store.Connector{TenantID: tn.ID, Name: "planted", Slug: "planted", Endpoint: "https://a.example", TimeoutMS: 1000, Status: "unknown",
		Metadata: map[string]any{
			"description": "kept",
			"headers": map[string]any{
				"X-Planted": map[string]any{"type": "external", "provider": "secret_store", "allow_locked": true,
					"config": map[string]any{"credential": "c", "field": "f"}},
				"X-Plain": map[string]any{"type": "static", "value": "v"},
			},
		}}
	plain := &store.Connector{TenantID: tn.ID, Name: "plain", Slug: "plain", Endpoint: "https://b.example", TimeoutMS: 1000, Status: "unknown",
		Metadata: map[string]any{"headers": "not-an-object"}}
	for _, c := range []*store.Connector{planted, plain} {
		if err := st.Connectors().Create(ctx, c); err != nil {
			t.Fatalf("create %s: %v", c.Name, err)
		}
	}

	up, err := os.ReadFile(filepath.Join("migrations", "000016_platform_locked.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if _, err := db.ExecContext(ctx, string(up)); err != nil {
			t.Fatalf("run 000016 up.sql (pass %d): %v", i+1, err)
		}
	}

	got, err := st.Connectors().Get(ctx, tn.ID, planted.ID)
	if err != nil {
		t.Fatal(err)
	}
	hdrs := got.Metadata["headers"].(map[string]any)
	p := hdrs["X-Planted"].(map[string]any)
	if _, ok := p["allow_locked"]; ok {
		t.Errorf("allow_locked survived the migration: %v", p)
	}
	if p["provider"] != "secret_store" || p["config"].(map[string]any)["credential"] != "c" {
		t.Errorf("rest of the header changed: %v", p)
	}
	if hdrs["X-Plain"].(map[string]any)["value"] != "v" || got.Metadata["description"] != "kept" {
		t.Errorf("other metadata changed: %v", got.Metadata)
	}
	if other, err := st.Connectors().Get(ctx, tn.ID, plain.ID); err != nil || other.Metadata["headers"] != "not-an-object" {
		t.Errorf("connector without a headers object changed: %+v, %v", other, err)
	}
}
