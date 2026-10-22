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

// TestPlatformAdminMigration_DownRefusesWhileRoleInUse proves what
// migrations/000010_platform_admin.down.sql's header promises: after
// Migrate (which applies 000010), a console user may hold role
// platform-admin and an audit row may carry actor_tenant_id; the hand
// rollback then fails while such a user exists (the narrowed CHECK is
// validated against existing rows), and succeeds once the role is no
// longer in use, dropping the audit column again.
func TestPlatformAdminMigration_DownRefusesWhileRoleInUse(t *testing.T) {
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping Postgres migration test")
	}
	ctx := context.Background()

	schema := "gwtest_padmin_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

	home := &store.Tenant{Slug: "padmin-home", Name: "Home"}
	other := &store.Tenant{Slug: "padmin-other", Name: "Other"}
	for _, tn := range []*store.Tenant{home, other} {
		if err := st.Tenants().Create(ctx, tn); err != nil {
			t.Fatalf("create tenant %s: %v", tn.Slug, err)
		}
	}

	// 000010 widened the CHECK: the role is storable.
	root := &store.User{TenantID: home.ID, Username: "root", PasswordHash: "x", Role: store.UserRolePlatformAdmin}
	if err := st.Users().Create(ctx, root); err != nil {
		t.Fatalf("create platform-admin user after migration: %v", err)
	}
	// A role outside the closed set is still refused by the constraint.
	bogus := &store.User{TenantID: home.ID, Username: "bogus", PasswordHash: "x", Role: "superuser"}
	if err := st.Users().Create(ctx, bogus); err == nil {
		t.Error("users.role CHECK accepted an unknown role after migration 000010")
	}

	// 000010 added actor_tenant_id: a cross-tenant audit row round-trips.
	e := &store.AuthAuditEntry{TenantID: other.ID, ActorTenantID: home.ID, ActorKind: "user", ActorID: root.ID, Action: "user_create"}
	if err := st.AuthAudit().Append(ctx, e); err != nil {
		t.Fatalf("append audit with actor_tenant_id: %v", err)
	}
	if rows, _, err := st.AuthAudit().List(ctx, other.ID, store.AuthAuditListOptions{}); err != nil || len(rows) != 1 || rows[0].ActorTenantID != home.ID {
		t.Fatalf("audit list = %+v, %v; want one row with actor_tenant_id %q", rows, err, home.ID)
	}

	down, err := os.ReadFile(filepath.Join("migrations", "000010_platform_admin.down.sql"))
	if err != nil {
		t.Fatalf("read 000010 down.sql: %v", err)
	}

	// Rolling back while a platform-admin user exists must fail, and must
	// leave the schema as it was (the file runs in one implicit
	// transaction through ExecContext, so a failed CHECK rolls back the
	// DROP CONSTRAINT and the column drop too).
	if _, err := db.ExecContext(ctx, string(down)); err == nil {
		t.Fatal("000010 down.sql succeeded while a platform-admin user exists; its header promises it fails")
	}
	if !columnExists(t, ctx, db, "auth_audit", "actor_tenant_id") {
		t.Error("failed down migration dropped auth_audit.actor_tenant_id anyway")
	}
	if _, err := st.Users().Get(ctx, home.ID, root.ID); err != nil {
		t.Errorf("platform-admin user unreadable after the failed rollback: %v", err)
	}

	// Demote the user as the header says, and the rollback goes through.
	root.Role = store.UserRoleAdmin
	if err := st.Users().Update(ctx, root); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(down)); err != nil {
		t.Fatalf("000010 down.sql after demoting the platform-admin: %v", err)
	}
	if columnExists(t, ctx, db, "auth_audit", "actor_tenant_id") {
		t.Error("auth_audit.actor_tenant_id still present after the down migration")
	}
	again := &store.User{TenantID: home.ID, Username: "root2", PasswordHash: "x", Role: store.UserRolePlatformAdmin}
	if err := st.Users().Create(ctx, again); err == nil {
		t.Error("users.role CHECK accepted platform-admin after the down migration narrowed it")
	}
}

func columnExists(t *testing.T, ctx context.Context, db *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	const q = `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`
	if err := db.QueryRowContext(ctx, q, table, column).Scan(&n); err != nil {
		t.Fatalf("lookup column %s.%s: %v", table, column, err)
	}
	return n == 1
}
