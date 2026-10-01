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

// TestModelCatalogMigration_IdempotentOnRerun proves migration 6's own SQL
// file (not just Store.Migrate's version-tracked application of it, which
// by construction only ever runs a given version once and so never
// exercises this path) can be executed against an already-migrated
// database a second time without error and without duplicating the
// default catalog it seeds -- migration 000006_model_catalog.up.sql's own
// doc comment promises exactly this ("re-running this file by hand ... is
// a no-op, not an error"). It also confirms pre-existing tenant data
// (created once the schema is already at 000005, before 000006 ever ran)
// survives both the first application and the re-run untouched.
func TestModelCatalogMigration_IdempotentOnRerun(t *testing.T) {
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping Postgres migration test")
	}
	ctx := context.Background()

	schema := "gwtest_migcheck_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	// Brings the schema to 000005 AND applies 000006 once, tracked in
	// schema_migrations -- i.e. exactly the "already at 000005, now
	// migrated to current" state a real deployment reaches.
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Pre-existing tenant data (as a real deployment would have before
	// upgrading further) that the re-run below must leave untouched.
	tenant := &store.Tenant{Slug: "migcheck", Name: "Migration Check"}
	if err := st.Tenants().Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	preExisting := &store.Model{
		TenantID: tenant.ID, Name: "pre-existing", Enabled: true,
		Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "claude-sonnet-4-5"}},
	}
	if err := st.Models().Create(ctx, preExisting); err != nil {
		t.Fatalf("create pre-existing model: %v", err)
	}

	providersBefore := countRows(t, ctx, db, "model_catalog_providers")
	modelsBefore := countRows(t, ctx, db, "model_catalog_models")
	if providersBefore == 0 || modelsBefore == 0 {
		t.Fatalf("expected the default catalog already seeded by Migrate, got %d providers / %d models", providersBefore, modelsBefore)
	}

	sqlBytes, err := os.ReadFile(filepath.Join("migrations", "000006_model_catalog.up.sql"))
	if err != nil {
		t.Fatalf("read migration 6 up.sql: %v", err)
	}

	// Re-run the raw file directly, bypassing schema_migrations entirely
	// -- this is the actual "run twice" case; Migrate() itself would skip
	// it as already-applied and never touch the SQL again.
	if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("re-running migration 6 up.sql: %v", err)
	}
	// A second re-run, for good measure (three total applications).
	if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("re-running migration 6 up.sql a second time: %v", err)
	}

	if got := countRows(t, ctx, db, "model_catalog_providers"); got != providersBefore {
		t.Errorf("model_catalog_providers count after re-running the migration = %d, want unchanged %d", got, providersBefore)
	}
	if got := countRows(t, ctx, db, "model_catalog_models"); got != modelsBefore {
		t.Errorf("model_catalog_models count after re-running the migration = %d, want unchanged %d", got, modelsBefore)
	}

	reGot, err := st.Models().Get(ctx, tenant.ID, preExisting.ID)
	if err != nil || reGot.Name != "pre-existing" {
		t.Errorf("pre-existing model after re-running the migration = %+v, %v, want unchanged", reGot, err)
	}
}

func countRows(t *testing.T, ctx context.Context, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
