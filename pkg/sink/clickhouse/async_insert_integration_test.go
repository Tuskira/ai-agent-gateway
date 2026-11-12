//go:build integration

package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// scratchDatabase points cfg at a new, empty database named after prefix
// and dropped when the test ends.
func scratchDatabase(t *testing.T, cfg Config, prefix string) Config {
	t.Helper()
	cfg.Database = fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	admin := adminConn(t, cfg)
	if err := admin.Exec(context.Background(), "CREATE DATABASE "+cfg.Database); err != nil {
		t.Fatalf("create database %s: %v", cfg.Database, err)
	}
	t.Cleanup(func() { _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+cfg.Database) })
	return cfg
}

func newTestSink(t *testing.T, cfg Config) *Sink {
	t.Helper()
	ls, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	s, ok := ls.(*Sink)
	if !ok {
		t.Fatalf("New() returned %T, want *Sink", ls)
	}
	return s
}

func countRows(t *testing.T, c conn, db, table string) uint64 {
	t.Helper()
	var n uint64
	if err := c.QueryRow(context.Background(), fmt.Sprintf("SELECT count() FROM %s.%s", db, table)).Scan(&n); err != nil {
		t.Fatalf("count %s.%s: %v", db, table, err)
	}
	return n
}

func accessRow(i int) *sink.AccessLog {
	return &sink.AccessLog{
		Timestamp: time.Now().UTC(), RequestID: fmt.Sprintf("req-%d", i), TenantID: "t-async",
		Principal: "p", Method: "tools/call", ToolName: "x__y", StatusCode: 200,
	}
}

func llmRow(i int) *sink.LLMCall {
	return &sink.LLMCall{
		Timestamp: time.Now().UTC(), RequestID: fmt.Sprintf("llm-%d", i), TenantID: "t-async",
		Principal: "p", Provider: "anthropic", Model: "m", Path: "/v1/messages", StatusCode: 200,
	}
}

type insertLogRow struct {
	asyncInsert, waitForAsync, busyTimeout, adaptive string
}

// insertQueryLog returns the settings of every finished INSERT into
// db.table recorded in system.query_log.
func insertQueryLog(t *testing.T, admin conn, db, table string) []insertLogRow {
	t.Helper()
	ctx := context.Background()
	if err := admin.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("SYSTEM FLUSH LOGS: %v", err)
	}
	rows, err := admin.Query(ctx, `
		SELECT Settings['async_insert'], Settings['wait_for_async_insert'],
		       -- 24.2+ records the alias under its canonical name
		       if(mapContains(Settings, 'async_insert_busy_timeout_max_ms'),
		          Settings['async_insert_busy_timeout_max_ms'], Settings['async_insert_busy_timeout_ms']),
		       Settings['async_insert_use_adaptive_busy_timeout']
		FROM system.query_log
		WHERE type = 'QueryFinish' AND query_kind = 'Insert' AND current_database = ?
		  AND has(tables, ?)`, db, db+"."+table)
	if err != nil {
		t.Fatalf("query_log: %v", err)
	}
	defer rows.Close()
	var out []insertLogRow
	for rows.Next() {
		var r insertLogRow
		if err := rows.Scan(&r.asyncInsert, &r.waitForAsync, &r.busyTimeout, &r.adaptive); err != nil {
			t.Fatalf("scan query_log: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// AC-1: a background flush is an asynchronous insert that waits for the
// write, so the rows are readable as soon as it returns.
func TestIntegration_AsyncInsertSettings(t *testing.T) {
	cfg := scratchDatabase(t, testConfig(t), "async_settings")
	cfg.FlushInterval = time.Hour
	cfg.BatchSize = 1000
	cfg.AsyncInsertBusyTimeout = 300 * time.Millisecond
	s := newTestSink(t, cfg)
	defer s.Close()
	admin := adminConn(t, cfg)

	start := time.Now()
	s.flush(batch{access: []*sink.AccessLog{accessRow(1), accessRow(2), accessRow(3)}})
	elapsed := time.Since(start)

	if got := countRows(t, admin, cfg.Database, "mcp_access_logs"); got != 3 {
		t.Fatalf("rows right after the flush returned = %d, want 3", got)
	}
	// wait_for_async_insert=1 with a fixed (non-adaptive) busy timeout:
	// the flush returns only after the server's buffer was written.
	if elapsed < 250*time.Millisecond {
		t.Errorf("flush returned after %s, want >= ~300ms busy timeout (did it wait for the async insert?)", elapsed)
	}

	logs := insertQueryLog(t, admin, cfg.Database, "mcp_access_logs")
	if len(logs) != 1 {
		t.Fatalf("query_log INSERTs into mcp_access_logs = %d, want 1: %+v", len(logs), logs)
	}
	got := logs[0]
	// query_log's Settings holds only settings that differ from the
	// user's defaults: wait_for_async_insert=1 is sent, but where 1 is
	// already the default (it is on stock ClickHouse) it isn't listed.
	if got.waitForAsync == "" {
		var def string
		if err := admin.QueryRow(context.Background(), "SELECT value FROM system.settings WHERE name = 'wait_for_async_insert'").Scan(&def); err != nil {
			t.Fatalf("read wait_for_async_insert default: %v", err)
		}
		if def != "1" {
			t.Errorf("wait_for_async_insert not in query_log and the server default is %q, want 1", def)
		}
		got.waitForAsync = def
	}
	want := insertLogRow{asyncInsert: "1", waitForAsync: "1", busyTimeout: "300", adaptive: "0"}
	if got != want {
		t.Errorf("INSERT settings = %+v, want %+v", got, want)
	}
	t.Logf("async flush of 3 rows returned after %s; query_log settings %+v", elapsed, logs[0])
}

// The escape hatch: SyncInsert sends a plain INSERT with no async-insert
// settings at all.
func TestIntegration_SyncInsertEscapeHatch(t *testing.T) {
	cfg := scratchDatabase(t, testConfig(t), "sync_insert")
	cfg.FlushInterval = time.Hour
	cfg.BatchSize = 1000
	cfg.SyncInsert = true
	s := newTestSink(t, cfg)
	defer s.Close()
	admin := adminConn(t, cfg)

	start := time.Now()
	s.flush(batch{access: []*sink.AccessLog{accessRow(1), accessRow(2)}, llm: []*sink.LLMCall{llmRow(1)}})
	elapsed := time.Since(start)

	if got := countRows(t, admin, cfg.Database, "mcp_access_logs"); got != 2 {
		t.Fatalf("access rows = %d, want 2", got)
	}
	if got := countRows(t, admin, cfg.Database, "llm_calls"); got != 1 {
		t.Fatalf("llm rows = %d, want 1", got)
	}
	for _, table := range []string{"mcp_access_logs", "llm_calls"} {
		logs := insertQueryLog(t, admin, cfg.Database, table)
		if len(logs) != 1 {
			t.Fatalf("query_log INSERTs into %s = %d, want 1", table, len(logs))
		}
		if logs[0] != (insertLogRow{}) {
			t.Errorf("%s INSERT carried async settings %+v, want none", table, logs[0])
		}
	}
	t.Logf("sync flush returned after %s", elapsed)
}

// Close writes what is buffered and waits for an in-flight async flush:
// every record written before Close is in ClickHouse once it returns,
// well inside a 30s termination grace period.
func TestIntegration_CloseDrains(t *testing.T) {
	cfg := scratchDatabase(t, testConfig(t), "close_drains")
	cfg.FlushInterval = time.Hour
	cfg.BatchSize = 3
	cfg.AsyncInsertBusyTimeout = 0 // the production default, not testConfig's
	s := newTestSink(t, cfg)
	admin := adminConn(t, cfg)

	// Three rows reach BatchSize: an async flush starts and waits out the
	// (default) busy timeout on the server...
	for i := 0; i < 3; i++ {
		s.WriteAccess(accessRow(i))
	}
	time.Sleep(200 * time.Millisecond)
	// ...while these stay buffered in the run loop.
	s.WriteAccess(accessRow(3))
	s.WriteAccess(accessRow(4))
	s.WriteLLMCall(llmRow(1))
	s.WriteLLMCall(llmRow(2))

	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	took := time.Since(start)

	if got := countRows(t, admin, cfg.Database, "mcp_access_logs"); got != 5 {
		t.Errorf("access rows after Close = %d, want 5", got)
	}
	if got := countRows(t, admin, cfg.Database, "llm_calls"); got != 2 {
		t.Errorf("llm rows after Close = %d, want 2", got)
	}
	if took > defaultAsyncInsertBusyTimeout+5*time.Second {
		t.Errorf("Close took %s, want <= busy timeout %s + 5s", took, defaultAsyncInsertBusyTimeout)
	}
	t.Logf("Close returned after %s (busy timeout %s)", took, defaultAsyncInsertBusyTimeout)
}

// An async insert whose caller gives up while it waits is still written
// when the server's buffer is flushed. This is why retryable never
// repeats an insert after a context deadline: it would duplicate rows.
func TestIntegration_AbandonedAsyncInsertStillLands(t *testing.T) {
	cfg := scratchDatabase(t, testConfig(t), "abandoned_async")
	cfg.AsyncInsertBusyTimeout = 3 * time.Second
	s := newTestSink(t, cfg)
	defer s.Close()
	admin := adminConn(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := s.writer.InsertAccess(s.insertCtx(ctx), []*sink.AccessLog{accessRow(1), accessRow(2)})
	if err == nil {
		t.Fatal("insert returned before its deadline; want it to still be waiting on the 3s busy timeout")
	}
	if retryable(err) {
		t.Errorf("retryable(%v) = true for an abandoned async insert, want false", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		n := countRows(t, admin, cfg.Database, "mcp_access_logs")
		if n == 2 {
			t.Logf("abandoned insert (%v) landed anyway", err)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("abandoned async insert: %d rows after 10s, want 2 (rows stay in the server buffer)", n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// retryable classifies real driver errors: a server exception that is not
// transient (missing table) is not retried; a connection that never
// opened (nothing sent) is.
func TestIntegration_RetryableClassifiesRealErrors(t *testing.T) {
	cfg := scratchDatabase(t, testConfig(t), "retryable")
	s := newTestSink(t, cfg)
	defer s.Close()
	admin := adminConn(t, cfg)

	if err := admin.Exec(context.Background(), "DROP TABLE "+cfg.Database+".llm_calls"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	err := s.writer.InsertLLM(s.insertCtx(context.Background()), []*sink.LLMCall{llmRow(1)})
	var ex *clickhouse.Exception
	if !errors.As(err, &ex) {
		t.Fatalf("insert into a dropped table: err = %v, want a *clickhouse.Exception", err)
	}
	if retryable(err) {
		t.Errorf("retryable(code %d) = true, want false", ex.Code)
	}

	// A port nothing listens on: the dial fails inside PrepareBatch.
	dead, err := clickhouse.Open(&clickhouse.Options{Addr: []string{"127.0.0.1:1"}, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dead.Close()
	err = (&chWriter{c: dead}).InsertAccess(context.Background(), []*sink.AccessLog{accessRow(1)})
	if err == nil || !retryable(err) {
		t.Errorf("insert over a refused connection: err = %v, retryable = %v; want a retryable error", err, retryable(err))
	}
}

// measureScenario is one sink configuration in the part-rate measurement.
type measureScenario struct {
	name          string
	syncInsert    bool
	flush         time.Duration
	busyTimeout   time.Duration
	serverDefault bool // async_insert + wait only: the server's own (adaptive) busy timeout
}

// TestIntegration_MeasureAsyncInsertPartRate simulates light production
// traffic -- six replicas writing ~0.7 records/s in total (70% access
// logs, 30% LLM calls) -- under several sink configurations at once, one
// database each, then reports new parts per table from system.part_log,
// rows per part, and how long a row took from Write to being in a part.
// It runs only when GATEWAY_TEST_CLICKHOUSE_MEASURE is set to a duration
// (e.g. 5m), since it takes that long.
func TestIntegration_MeasureAsyncInsertPartRate(t *testing.T) {
	durStr := os.Getenv("GATEWAY_TEST_CLICKHOUSE_MEASURE")
	if durStr == "" {
		t.Skip("GATEWAY_TEST_CLICKHOUSE_MEASURE not set; skipping the part-rate measurement")
	}
	dur, err := time.ParseDuration(durStr)
	if err != nil {
		t.Fatalf("GATEWAY_TEST_CLICKHOUSE_MEASURE: %v", err)
	}
	base := testConfig(t)
	base.BatchSize = 100
	base.BufferSize = 1000

	scenarios := []measureScenario{
		{name: "old_sync_5s", syncInsert: true, flush: 5 * time.Second},
		{name: "async_server_default_5s", flush: 5 * time.Second, serverDefault: true},
		{name: "async_5s_bt5s", flush: 5 * time.Second, busyTimeout: 5 * time.Second},
		{name: "async_5s_bt10s", flush: 5 * time.Second, busyTimeout: 10 * time.Second},
		{name: "async_2s_bt10s", flush: 2 * time.Second, busyTimeout: 10 * time.Second},
		{name: "async_5s_bt15s", flush: 5 * time.Second, busyTimeout: 15 * time.Second},
		{name: "new_defaults"}, // zero values: defaultFlushInterval, defaultAsyncInsertBusyTimeout
	}
	if only := os.Getenv("GATEWAY_TEST_CLICKHOUSE_MEASURE_ONLY"); only != "" {
		var keep []measureScenario
		for _, sc := range scenarios {
			if strings.Contains(","+only+",", ","+sc.name+",") {
				keep = append(keep, sc)
			}
		}
		scenarios = keep
	}
	const replicas = 6
	const totalRate = 0.7 // records per second, all replicas together

	admin := adminConn(t, base)
	type running struct {
		sc    measureScenario
		db    string
		sinks []*Sink
	}
	var runs []running
	for _, sc := range scenarios {
		cfg := scratchDatabase(t, base, "measure_"+sc.name)
		cfg.SyncInsert = sc.syncInsert
		cfg.FlushInterval = sc.flush
		cfg.AsyncInsertBusyTimeout = sc.busyTimeout
		r := running{sc: sc, db: cfg.Database}
		for i := 0; i < replicas; i++ {
			s := newTestSink(t, cfg)
			if sc.serverDefault {
				s.settings = clickhouse.Settings{"async_insert": 1, "wait_for_async_insert": 1}
			}
			r.sinks = append(r.sinks, s)
		}
		// Keep every part a flush creates, so each row's part (and its
		// creation time) can be looked up afterwards.
		for _, table := range []string{"mcp_access_logs", "llm_calls"} {
			if err := admin.Exec(context.Background(), "SYSTEM STOP MERGES "+cfg.Database+"."+table); err != nil {
				t.Fatalf("stop merges: %v", err)
			}
		}
		runs = append(runs, r)
	}

	start := time.Now()
	end := start.Add(dur)
	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Add(1)
		go func(r running) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano()))
			for i := 0; ; i++ {
				// Poisson arrivals at totalRate, each to a random replica.
				time.Sleep(time.Duration(rng.ExpFloat64() / totalRate * float64(time.Second)))
				if time.Now().After(end) {
					return
				}
				s := r.sinks[rng.Intn(replicas)]
				if rng.Float64() < 0.7 {
					s.WriteAccess(accessRow(i))
				} else {
					s.WriteLLMCall(llmRow(i))
				}
			}
		}(r)
	}
	wg.Wait()
	// Let the last flushes and server buffers settle before closing, so
	// Close's final flush (its own part) has nothing left to write.
	time.Sleep(25 * time.Second)
	windowEnd := time.Now()
	for _, r := range runs {
		for _, s := range r.sinks {
			_ = s.Close()
		}
	}
	if err := admin.Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flush logs: %v", err)
	}

	hours := dur.Hours()
	t.Logf("traffic: %d replicas, %.1f rows/s total, %s; parts counted up to %s after the last write", replicas, totalRate, dur, windowEnd.Sub(end).Round(time.Second))
	t.Logf("%-26s %-16s %7s %7s %8s %9s %10s %9s %9s %9s", "scenario", "table", "inserts", "parts", "rows", "rows/part", "parts/hour", "p50 delay", "p95 delay", "max delay")
	for _, r := range runs {
		for _, table := range []string{"mcp_access_logs", "llm_calls"} {
			var inserts uint64
			if err := admin.QueryRow(context.Background(), `
				SELECT count() FROM system.query_log
				WHERE type = 'QueryFinish' AND query_kind = 'Insert' AND has(tables, ?) AND event_time_microseconds < ?`,
				r.db+"."+table, windowEnd).Scan(&inserts); err != nil {
				t.Fatalf("query_log: %v", err)
			}
			var parts, rows uint64
			if err := admin.QueryRow(context.Background(), `
				SELECT count(), sum(rows) FROM system.part_log
				WHERE event_type = 'NewPart' AND database = ? AND table = ? AND event_time_microseconds < ?`,
				r.db, table, windowEnd).Scan(&parts, &rows); err != nil {
				t.Fatalf("part_log: %v", err)
			}
			var q []float64
			if err := admin.QueryRow(context.Background(), fmt.Sprintf(`
				SELECT quantiles(0.5, 0.95, 1)(dateDiff('millisecond', t.timestamp, p.created) / 1000.0)
				FROM %s.%s AS t
				INNER JOIN (
					SELECT part_name, event_time_microseconds AS created FROM system.part_log
					WHERE event_type = 'NewPart' AND database = '%s' AND table = '%s'
				) AS p ON t._part = p.part_name`, r.db, table, r.db, table)).Scan(&q); err != nil {
				t.Fatalf("delay: %v", err)
			}
			perPart := 0.0
			if parts > 0 {
				perPart = float64(rows) / float64(parts)
			}
			t.Logf("%-26s %-16s %7d %7d %8d %9.1f %10.0f %8.1fs %8.1fs %8.1fs", r.sc.name, table, inserts, parts, rows, perPart,
				float64(parts)/hours, q[0], q[1], q[2])
		}
	}
}
