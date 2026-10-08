package postgres_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres"
)

// Stats exposes the live pool: after a query the pool holds at least one
// open connection, and none is in use once the query returned. It also
// proves the store satisfies the Stats() interface cmd/gateway asserts.
func TestStoreStats(t *testing.T) {
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping Postgres pool stats test")
	}
	db, err := sql.Open("pgx", rawURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(7)

	var st interface{ Stats() sql.DBStats } = postgres.New(db)
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := st.Stats()
	if got.MaxOpenConnections != 7 {
		t.Errorf("MaxOpenConnections = %d, want 7 (Stats must read the store's own pool)", got.MaxOpenConnections)
	}
	if got.OpenConnections < 1 || got.InUse != 0 {
		t.Errorf("after a ping: open %d in_use %d, want open >= 1 and in_use 0", got.OpenConnections, got.InUse)
	}
}
