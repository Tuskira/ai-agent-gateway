package metrics

import (
	"database/sql"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres"
)

// wantSeries fails unless the scrape has a sample line "series value".
func wantSeries(t *testing.T, body, series, value string) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, series+" ") {
			if got := strings.TrimPrefix(line, series+" "); got != value {
				t.Errorf("%s = %s, want %s", series, got, value)
			}
			return
		}
	}
	t.Errorf("scrape has no series %s:\n%s", series, grep(body, "gateway_"))
}

func noSeries(t *testing.T, body, name string) {
	t.Helper()
	if strings.Contains(body, "\n"+name) {
		t.Errorf("scrape unexpectedly has %s:\n%s", name, grep(body, name))
	}
}

func grep(body, sub string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, sub) && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestAuthLimiterCollectors(t *testing.T) {
	m := newMetrics(t, promCfg("127.0.0.1:0"))
	mk := func(plane string) *auth.RateLimiter {
		rl := auth.NewRateLimiter(auth.RateLimiterConfig{
			Plane: plane, Enabled: true, MaxFailures: 2, Window: time.Minute, Lockout: time.Minute,
		})
		t.Cleanup(rl.Close)
		return rl
	}
	api, mcp, llm := mk("api"), mk("mcp"), mk("llm")
	if err := RegisterAuthLimiters(m, map[string]AuthLimiter{"api": api, "mcp": mcp, "llm": llm}); err != nil {
		t.Fatal(err)
	}

	body := scrape(t, m)
	for _, plane := range []string{"api", "mcp", "llm"} {
		wantSeries(t, body, `gateway_auth_locked_ips{plane="`+plane+`"}`, "0")
		wantSeries(t, body, `gateway_auth_failures_total{plane="`+plane+`"}`, "0")
	}

	mcp.ReportFailure("10.0.0.1")
	mcp.ReportFailure("10.0.0.1") // second failure trips the lockout
	mcp.ReportFailure("10.0.0.2")
	api.ReportFailure("10.0.0.3")

	body = scrape(t, m)
	wantSeries(t, body, `gateway_auth_locked_ips{plane="mcp"}`, "1")
	wantSeries(t, body, `gateway_auth_failures_total{plane="mcp"}`, "3")
	wantSeries(t, body, `gateway_auth_failures_total{plane="api"}`, "1")
	wantSeries(t, body, `gateway_auth_locked_ips{plane="api"}`, "0")
	wantSeries(t, body, `gateway_auth_failures_total{plane="llm"}`, "0")
}

type fakeLLMLimiter struct{ budget, rpm atomic.Uint64 }

func (f *fakeLLMLimiter) BudgetDenials() uint64 { return f.budget.Load() }
func (f *fakeLLMLimiter) RPMDenials() uint64    { return f.rpm.Load() }

func TestLLMLimitsCollector(t *testing.T) {
	m := newMetrics(t, promCfg("127.0.0.1:0"))
	l := &fakeLLMLimiter{}
	if err := RegisterLLMLimits(m, l); err != nil {
		t.Fatal(err)
	}
	wantSeries(t, scrape(t, m), `gateway_llm_limit_denials_total{reason="budget"}`, "0")

	l.budget.Store(2)
	l.rpm.Store(5)
	body := scrape(t, m)
	wantSeries(t, body, `gateway_llm_limit_denials_total{reason="budget"}`, "2")
	wantSeries(t, body, `gateway_llm_limit_denials_total{reason="rpm"}`, "5")
}

type fakeTee struct {
	sent, dropped, failed atomic.Uint64
	depth                 atomic.Int64
}

func (f *fakeTee) Sent() uint64    { return f.sent.Load() }
func (f *fakeTee) Dropped() uint64 { return f.dropped.Load() }
func (f *fakeTee) Failed() uint64  { return f.failed.Load() }
func (f *fakeTee) QueueDepth() int { return int(f.depth.Load()) }

func TestDetectionTeeCollector(t *testing.T) {
	m := newMetrics(t, promCfg("127.0.0.1:0"))
	tee := &fakeTee{}
	if err := RegisterDetectionTee(m, tee); err != nil {
		t.Fatal(err)
	}
	tee.sent.Store(10)
	tee.dropped.Store(3)
	tee.failed.Store(1)
	tee.depth.Store(4)
	body := scrape(t, m)
	wantSeries(t, body, `gateway_llm_detection_turns_total{result="sent"}`, "10")
	wantSeries(t, body, `gateway_llm_detection_turns_total{result="dropped"}`, "3")
	wantSeries(t, body, `gateway_llm_detection_turns_total{result="failed"}`, "1")
	wantSeries(t, body, `gateway_llm_detection_queue_depth`, "4")
}

func TestSinkDropsCollector(t *testing.T) {
	m := newMetrics(t, promCfg("127.0.0.1:0"))
	var stdout, ch atomic.Uint64
	if err := RegisterSinkDrops(m, map[string]func() uint64{
		"stdout": stdout.Load, "clickhouse": ch.Load, "absent": nil,
	}); err != nil {
		t.Fatal(err)
	}
	stdout.Store(7)
	body := scrape(t, m)
	wantSeries(t, body, `gateway_sink_records_dropped_total{sink="stdout"}`, "7")
	wantSeries(t, body, `gateway_sink_records_dropped_total{sink="clickhouse"}`, "0")
	noSeries(t, body, `gateway_sink_records_dropped_total{sink="absent"}`)
}

type fakeBodyStore struct{ off, fb atomic.Uint64 }

func (f *fakeBodyStore) Offloaded() uint64 { return f.off.Load() }
func (f *fakeBodyStore) Fallbacks() uint64 { return f.fb.Load() }

func TestBodyStoreCollector(t *testing.T) {
	m := newMetrics(t, promCfg("127.0.0.1:0"))
	b := &fakeBodyStore{}
	if err := RegisterBodyStore(m, b); err != nil {
		t.Fatal(err)
	}
	b.off.Store(12)
	b.fb.Store(2)
	body := scrape(t, m)
	wantSeries(t, body, `gateway_body_store_offloads_total`, "12")
	wantSeries(t, body, `gateway_body_store_fallbacks_total`, "2")
}

type fakeDB struct{ stats atomic.Pointer[sql.DBStats] }

func (f *fakeDB) Stats() sql.DBStats { return *f.stats.Load() }

func TestDBStatsCollector(t *testing.T) {
	m := newMetrics(t, promCfg("127.0.0.1:0"))
	db := &fakeDB{}
	db.stats.Store(&sql.DBStats{OpenConnections: 5, Idle: 3, InUse: 2, WaitDuration: 1500 * time.Millisecond})
	if err := RegisterDBStats(m, db); err != nil {
		t.Fatal(err)
	}
	body := scrape(t, m)
	wantSeries(t, body, `gateway_db_connections{state="open"}`, "5")
	wantSeries(t, body, `gateway_db_connections{state="idle"}`, "3")
	wantSeries(t, body, `gateway_db_connections{state="in_use"}`, "2")
	wantSeries(t, body, `gateway_db_wait_duration_seconds_total`, "1.5")
	if strings.Contains(body, "seconds_seconds") {
		t.Errorf("unit suffix doubled:\n%s", grep(body, "db_wait"))
	}

	db.stats.Store(&sql.DBStats{OpenConnections: 1, Idle: 1, WaitDuration: 2 * time.Second})
	body = scrape(t, m)
	wantSeries(t, body, `gateway_db_connections{state="open"}`, "1")
	wantSeries(t, body, `gateway_db_wait_duration_seconds_total`, "2")
}

// Against a real pool: the postgres store satisfies DBStatser and the
// gauges report its live connections.
func TestDBStatsCollectorRealPool(t *testing.T) {
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping the real pool collector test")
	}
	db, err := sql.Open("pgx", rawURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	var st DBStatser = postgres.New(db)

	m := newMetrics(t, promCfg("127.0.0.1:0"))
	if err := RegisterDBStats(m, st); err != nil {
		t.Fatal(err)
	}
	body := scrape(t, m)
	open := regexp.MustCompile(`(?m)^gateway_db_connections\{state="open"\} (\d+)$`).FindStringSubmatch(body)
	if open == nil || open[1] == "0" {
		t.Errorf("open connections not reported or zero:\n%s", grep(body, "gateway_db_"))
	}
}

func TestSessionsCollector(t *testing.T) {
	m := newMetrics(t, promCfg("127.0.0.1:0"))
	var active atomic.Int64
	var created atomic.Uint64
	if err := RegisterSessions(m, func() int { return int(active.Load()) }, created.Load); err != nil {
		t.Fatal(err)
	}

	active.Store(3)
	created.Store(9)
	body := scrape(t, m)
	wantSeries(t, body, `gateway_mcp_sessions_active`, "3")
	wantSeries(t, body, `gateway_mcp_sessions_created_total`, "9")

	// A driver that cannot count (redis) reports -1: no gauge sample, and
	// above all not a "-1" one. The created counter is unaffected.
	active.Store(-1)
	body = scrape(t, m)
	noSeries(t, body, "gateway_mcp_sessions_active")
	wantSeries(t, body, `gateway_mcp_sessions_created_total`, "9")
}

func TestToolCacheLookupHook(t *testing.T) {
	m := newMetrics(t, promCfg("127.0.0.1:0"))
	hook, err := ToolCacheLookupHook(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"hit", "hit", "hit", "stale", "miss", "miss", "garbage", ""} {
		hook(r)
	}
	body := scrape(t, m)
	wantSeries(t, body, `gateway_mcp_tool_cache_lookups_total{result="hit"}`, "3")
	wantSeries(t, body, `gateway_mcp_tool_cache_lookups_total{result="stale"}`, "1")
	wantSeries(t, body, `gateway_mcp_tool_cache_lookups_total{result="miss"}`, "2")
	// Only the three known results become label values: a caller bug
	// cannot mint series.
	if n := strings.Count(grep(body, "gateway_mcp_tool_cache_lookups_total"), "\n") + 1; n != 3 {
		t.Errorf("want exactly 3 series:\n%s", grep(body, "gateway_mcp_tool_cache_lookups"))
	}
}

// With the none driver, or no Metrics at all, every Register succeeds and
// records nothing: main.go calls them unconditionally.
func TestCollectorsAreNoopsWhenDisabled(t *testing.T) {
	for name, m := range map[string]*Metrics{
		"nil":  nil,
		"none": newMetrics(t, config.Default().Metrics),
	} {
		t.Run(name, func(t *testing.T) {
			rl := auth.NewRateLimiter(auth.RateLimiterConfig{Plane: "api"})
			t.Cleanup(rl.Close)
			db := &fakeDB{}
			db.stats.Store(&sql.DBStats{})
			var n atomic.Uint64
			for _, err := range []error{
				RegisterAuthLimiters(m, map[string]AuthLimiter{"api": rl}),
				RegisterLLMLimits(m, &fakeLLMLimiter{}),
				RegisterDetectionTee(m, &fakeTee{}),
				RegisterSinkDrops(m, map[string]func() uint64{"stdout": n.Load}),
				RegisterBodyStore(m, &fakeBodyStore{}),
				RegisterDBStats(m, db),
				RegisterSessions(m, func() int { return 0 }, n.Load),
			} {
				if err != nil {
					t.Error(err)
				}
			}
			hook, err := ToolCacheLookupHook(m)
			if err != nil {
				t.Fatal(err)
			}
			hook("hit")
		})
	}
}

func TestClampSaturates(t *testing.T) {
	if got := clamp(1<<64 - 1); got != 1<<63-1 {
		t.Errorf("clamp(max uint64) = %d", got)
	}
	if got := clamp(42); got != 42 {
		t.Errorf("clamp(42) = %d", got)
	}
}
