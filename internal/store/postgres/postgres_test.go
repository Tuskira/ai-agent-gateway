package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store/storetest"
)

// TestConformance runs the pkg/store/storetest conformance suite against a
// real Postgres, connecting via GATEWAY_TEST_DATABASE_URL. It is skipped
// when that env var is unset (e.g. plain `go test ./...` in CI without a
// database available).
//
// Each subtest gets its own randomly named schema (search_path scoped via
// the connection's DSN), migrated fresh, and dropped on cleanup — so
// subtests never see each other's rows even though they share one
// physical database.
func TestConformance(t *testing.T) {
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping Postgres conformance test")
	}

	storetest.Run(t, func(t *testing.T) store.Store {
		return newIsolatedStore(t, rawURL)
	})
}

func newIsolatedStore(t *testing.T, rawURL string) store.Store {
	t.Helper()
	ctx := context.Background()

	schema := "gwtest_" + strings.ReplaceAll(uuid.NewString(), "-", "")

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
		t.Fatalf("migrate schema %q: %v", schema, err)
	}
	return st
}

// withSearchPath returns rawURL with a search_path query parameter set to
// schema. pgx's stdlib driver applies unrecognized DSN query parameters as
// Postgres run-time parameters on every connection it opens for the pool
// (not just the first), so this scopes every query issued through the
// returned DSN to schema without any per-call session state to manage.
func withSearchPath(rawURL, schema string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
