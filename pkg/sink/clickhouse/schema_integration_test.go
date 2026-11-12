//go:build integration

// Schema-ledger tests against a real ClickHouse; see integration_test.go's
// header for how to run them.
package clickhouse

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// freshDatabase points cfg at a new, empty database unique to this test and
// drops it when the test ends.
func freshDatabase(t *testing.T, cfg Config) Config {
	t.Helper()
	cfg.Database = "schema_ledger_" + strings.ReplaceAll(strings.ToLower(uniqueTenantID(t)), "-", "_")
	admin := adminConn(t, cfg)
	if err := admin.Exec(context.Background(), "CREATE DATABASE "+cfg.Database); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+cfg.Database) })
	return cfg
}

// adminConn is a connection to the server's default database, for setup
// and for reading system.query_log.
func adminConn(t *testing.T, cfg Config) conn {
	t.Helper()
	c, err := clickhouse.Open(Config{Host: cfg.Host, Port: cfg.Port, Database: "default", Username: cfg.Username, Password: cfg.Password}.withDefaults().options())
	if err != nil {
		t.Fatalf("open admin conn: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// serverNow is the ClickHouse server's clock, so query_log windows do not
// depend on the test host's clock agreeing with the server's.
func serverNow(t *testing.T, admin conn) time.Time {
	t.Helper()
	var now time.Time
	if err := admin.QueryRow(context.Background(), "SELECT now64(6)").Scan(&now); err != nil {
		t.Fatalf("server now: %v", err)
	}
	return now
}

// ddlSince counts, from ClickHouse's own system.query_log, the DDL
// statements (CREATE / ALTER / DROP) that touched db since `since`.
func ddlSince(t *testing.T, admin conn, db string, since time.Time) uint64 {
	t.Helper()
	ctx := context.Background()
	if err := admin.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("SYSTEM FLUSH LOGS: %v", err)
	}
	var n uint64
	err := admin.QueryRow(ctx, `
SELECT count(DISTINCT query_id) FROM system.query_log
WHERE toUnixTimestamp64Micro(event_time_microseconds) >= ?
	AND query_kind IN ('Create', 'Alter', 'Drop')
	AND (current_database = ? OR has(databases, ?))`, since.UnixMicro(), db, db).Scan(&n)
	if err != nil {
		t.Fatalf("count DDL in query_log: %v", err)
	}
	return n
}

func ledgerRows(t *testing.T, c conn, fp string) uint64 {
	t.Helper()
	var n uint64
	if err := c.QueryRow(context.Background(), "SELECT count() FROM "+ledgerTable+" WHERE fingerprint = ?", fp).Scan(&n); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	return n
}

func newAndClose(t *testing.T, cfg Config) {
	t.Helper()
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// TestIntegration_SchemaLedgerSkipsDDLOnSecondStart: the first New on an
// empty database applies the schema and records its fingerprint; a second
// New on the same database issues no DDL at all.
func TestIntegration_SchemaLedgerSkipsDDLOnSecondStart(t *testing.T) {
	cfg := freshDatabase(t, testConfig(t))
	admin := adminConn(t, cfg)
	fp := schemaFingerprint(schemaStatements())

	firstStart := serverNow(t, admin)
	newAndClose(t, cfg)
	c := openRawConn(t, cfg)
	if n := ledgerRows(t, c, fp); n != 1 {
		t.Fatalf("ledger rows for %s after first New = %d, want 1", fp, n)
	}
	var version string
	if err := c.QueryRow(context.Background(), "SELECT gateway_version FROM "+ledgerTable+" WHERE fingerprint = ?", fp).Scan(&version); err != nil {
		t.Fatalf("read ledger version: %v", err)
	}
	// Positive control: the query_log query does see the first start's DDL.
	firstDDL := ddlSince(t, admin, cfg.Database, firstStart)
	if want := uint64(len(schemaStatements())); firstDDL != want {
		t.Fatalf("DDL statements logged for first New = %d, want %d", firstDDL, want)
	}

	secondStart := serverNow(t, admin)
	newAndClose(t, cfg)
	secondDDL := ddlSince(t, admin, cfg.Database, secondStart)
	t.Logf("query_log DDL count: first New = %d, second New = %d", firstDDL, secondDDL)
	if secondDDL != 0 {
		t.Fatalf("second New issued %d DDL statements, want 0", secondDDL)
	}
	if n := ledgerRows(t, c, fp); n != 1 {
		t.Fatalf("ledger rows after second New = %d, want still 1", n)
	}
}

// TestIntegration_SchemaConcurrentStarts: six gateways starting at once on
// an empty database all come up.
func TestIntegration_SchemaConcurrentStarts(t *testing.T) {
	cfg := freshDatabase(t, testConfig(t))
	const replicas = 6
	var wg sync.WaitGroup
	errs := make([]error, replicas)
	for i := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := New(cfg, nil)
			if err == nil {
				err = s.Close()
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d: New() error = %v", i, err)
		}
	}
	n := ledgerRows(t, openRawConn(t, cfg), schemaFingerprint(schemaStatements()))
	t.Logf("ledger rows after %d concurrent starts = %d", replicas, n)
	if n < 1 || n > replicas {
		t.Fatalf("ledger rows = %d, want 1..%d", n, replicas)
	}
}

// TestIntegration_SchemaMigrateThenStart: `gateway migrate`'s ClickHouse
// step records the schema, after which New issues no DDL; running Migrate
// again re-applies (it is unconditional) and still succeeds.
func TestIntegration_SchemaMigrateThenStart(t *testing.T) {
	cfg := freshDatabase(t, testConfig(t))
	cfg.Version = "v-test"
	admin := adminConn(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fp := schemaFingerprint(schemaStatements())

	if err := Migrate(ctx, cfg, nil); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	c := openRawConn(t, cfg)
	var version string
	if err := c.QueryRow(ctx, "SELECT gateway_version FROM "+ledgerTable+" WHERE fingerprint = ?", fp).Scan(&version); err != nil {
		t.Fatalf("read ledger after Migrate: %v", err)
	}
	if version != "v-test" {
		t.Fatalf("ledger gateway_version = %q, want v-test", version)
	}

	start := serverNow(t, admin)
	newAndClose(t, cfg)
	n := ddlSince(t, admin, cfg.Database, start)
	t.Logf("query_log DDL count for New after Migrate = %d", n)
	if n != 0 {
		t.Fatalf("New after Migrate issued %d DDL statements, want 0", n)
	}

	if err := Migrate(ctx, cfg, nil); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
	if n := ledgerRows(t, c, fp); n != 2 {
		t.Fatalf("ledger rows after two Migrate runs = %d, want 2", n)
	}
}

// TestIntegration_SchemaChangedFingerprintReapplies: a ledger holding only
// an older fingerprint (a release with a different schema) makes New apply
// the current schema and record it.
func TestIntegration_SchemaChangedFingerprintReapplies(t *testing.T) {
	cfg := freshDatabase(t, testConfig(t))
	ctx := context.Background()
	c := openRawConn(t, cfg)
	if err := c.Exec(ctx, migrationLedger); err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	if err := c.Exec(ctx, "INSERT INTO "+ledgerTable+" (fingerprint, gateway_version, statements, applied_at) SELECT 'older', 'v-old', 1, now64(3)"); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	newAndClose(t, cfg)
	if n := ledgerRows(t, c, schemaFingerprint(schemaStatements())); n != 1 {
		t.Fatalf("ledger rows for current fingerprint = %d, want 1", n)
	}
	var tables uint64
	if err := c.QueryRow(ctx, "SELECT count() FROM system.tables WHERE database = ? AND name IN ('mcp_access_logs', 'llm_calls', 'llm_usage_canonical')", cfg.Database).Scan(&tables); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if tables != 3 {
		t.Fatalf("schema objects = %d, want 3", tables)
	}
}
