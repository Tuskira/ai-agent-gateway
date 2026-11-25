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

// TestConnectorToolPrefixMigration takes a schema back to 000014 with the
// down file, puts connector rows in it (as a deployment upgrading from
// 000014 has), and checks that Migrate applies 000015 over them (rows read
// back with no prefix and keep working), that the down file removes the
// column and index again, and that 000015 re-applies cleanly after it.
func TestConnectorToolPrefixMigration(t *testing.T) {
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping Postgres migration test")
	}
	ctx := context.Background()

	schema := "gwtest_prefixmig_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	down, err := os.ReadFile(filepath.Join("migrations", "000015_connector_tool_prefix.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	rollBack := func() {
		t.Helper()
		if _, err := db.ExecContext(ctx, string(down)); err != nil {
			t.Fatalf("run 000015 down: %v", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 15`); err != nil {
			t.Fatalf("forget 000015: %v", err)
		}
		if columnExists(t, ctx, db, "connectors", "tool_prefix") || prefixIndexExists(t, ctx, db, schema) {
			t.Fatal("down left the tool_prefix column or its index behind")
		}
	}
	rollBack()

	// A deployment at 000014 with live and deleted connectors.
	tenant := &store.Tenant{Slug: "prefixmig", Name: "Prefix Migration"}
	if err := st.Tenants().Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	var liveID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO connectors (tenant_id, name, slug, endpoint, timeout_ms, status, capabilities, metadata)
		VALUES ($1, 'Live', 'live', 'https://live.example.com/mcp', 1000, 'unknown', '{}', '{}') RETURNING id`, tenant.ID).Scan(&liveID); err != nil {
		t.Fatalf("insert connector at 000014: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO connectors (tenant_id, name, slug, endpoint, timeout_ms, status, capabilities, metadata, deleted_at)
		VALUES ($1, 'Gone', 'gone', 'https://gone.example.com/mcp', 1000, 'unknown', '{}', '{}', now())`, tenant.ID); err != nil {
		t.Fatalf("insert deleted connector at 000014: %v", err)
	}

	for round := 1; round <= 2; round++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("round %d: migrate 000014 -> 000015: %v", round, err)
		}
		if !columnExists(t, ctx, db, "connectors", "tool_prefix") || !prefixIndexExists(t, ctx, db, schema) {
			t.Fatalf("round %d: 000015 did not add the column and its index", round)
		}
		got, err := st.Connectors().Get(ctx, tenant.ID, liveID)
		if err != nil || got.ToolPrefix != "" || got.Slug != "live" {
			t.Fatalf("round %d: pre-existing connector = %+v, %v; want slug live, no prefix", round, got, err)
		}
		got.ToolPrefix = "Live.Feed"
		if err := st.Connectors().Update(ctx, got); err != nil {
			t.Fatalf("round %d: set a prefix on a pre-existing connector: %v", round, err)
		}
		if _, err := st.Connectors().GetByToolPrefix(ctx, tenant.ID, "live.feed"); err != nil {
			t.Errorf("round %d: GetByToolPrefix: %v", round, err)
		}
		// The deleted row's slug never blocks a prefix.
		got.ToolPrefix = "gone"
		if err := st.Connectors().Update(ctx, got); err != nil {
			t.Errorf("round %d: prefix equal to a deleted connector's slug: %v", round, err)
		}
		if round == 1 {
			rollBack()
		}
	}
}

func prefixIndexExists(t *testing.T, ctx context.Context, db *sql.DB, schema string) bool {
	t.Helper()
	var ok bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes
		WHERE schemaname = $1 AND indexname = 'connectors_tenant_tool_prefix_live')`, schema).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}
