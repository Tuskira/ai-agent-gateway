package metrics

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	// Registers driver "prometheus", as cmd/gateway's blank import does.
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/metrics/prometheus"
)

func promCfg(addr string) config.Metrics {
	c := config.Default().Metrics
	c.Driver = "prometheus"
	c.Address = addr
	return c
}

func newMetrics(t *testing.T, cfg config.Metrics) *Metrics {
	t.Helper()
	m, err := New(context.Background(), cfg, "1.2.3-test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

// freeAddr returns a loopback address that was free a moment ago.
// httpplane listens on its own, so the test cannot hand it a listener.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// client opens a fresh connection per request: http.Server.Shutdown
// waits up to 5s for a connection that never sent a request, which a
// pooling transport can leave behind when it races a dial.
var client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}

func get(t *testing.T, url string) (int, string, string) {
	t.Helper()
	res, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Content-Type"), string(body)
}

func TestRoutineServesMetricsAndHealth(t *testing.T) {
	addr := freeAddr(t)
	m := newMetrics(t, promCfg(addr))
	if !m.Enabled() {
		t.Fatal("Enabled() = false for prometheus")
	}

	c, err := m.Meter().Int64Counter("routine_test")
	if err != nil {
		t.Fatal(err)
	}
	c.Add(context.Background(), 4)

	r := m.Routine()
	if r == nil {
		t.Fatal("Routine() = nil for prometheus")
	}
	if r.Name() != "metrics" {
		t.Errorf("Name() = %q", r.Name())
	}
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- r.Run(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
		if err := <-runErr; err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	base := "http://" + addr
	var status int
	var ctype, body string
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := client.Get(base + "/health")
		if err == nil {
			_ = res.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics listener never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	status, ctype, body = get(t, base+"/metrics")
	if status != http.StatusOK || !strings.HasPrefix(ctype, "text/plain") {
		t.Fatalf("GET /metrics = %d %q", status, ctype)
	}
	wantBuild := `gateway_build_info{go_version="` + runtime.Version() + `",version="1.2.3-test"} 1`
	for _, want := range []string{wantBuild, "gateway_routine_test_total 4", "go_goroutines"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q:\n%s", want, body)
		}
	}

	status, ctype, body = get(t, base+"/health")
	if status != http.StatusOK || ctype != "application/json" || strings.TrimSpace(body) != `{"status":"ok"}` {
		t.Errorf("GET /health = %d %q %q", status, ctype, body)
	}

	if status, _, _ = get(t, base+"/other"); status != http.StatusNotFound {
		t.Errorf("GET /other = %d, want 404", status)
	}
}

func TestMuxCustomPath(t *testing.T) {
	m := newMetrics(t, promCfg("127.0.0.1:0"))
	mux := newMux("/prom metrics", m.Handler())

	for path, want := range map[string]int{"/prom%20metrics": 200, "/metrics": 404, "/health": 200} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/health", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /health = %d, want 404", rec.Code)
	}
}

func TestNoneDriverHasNoRoutine(t *testing.T) {
	m := newMetrics(t, config.Default().Metrics)
	if m.Enabled() {
		t.Error("Enabled() = true for none")
	}
	if m.Handler() != nil {
		t.Error("Handler() != nil for none")
	}
	if r := m.Routine(); r != nil {
		t.Errorf("Routine() = %v, want nil", r)
	}
	c, err := m.Meter().Int64Counter("noop")
	if err != nil {
		t.Fatal(err)
	}
	c.Add(context.Background(), 1)
}

func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics
	if m.Enabled() || m.Handler() != nil || m.Routine() != nil {
		t.Error("nil *Metrics should be disabled with no handler or routine")
	}
	c, err := m.Meter().Float64Histogram("nil_hist")
	if err != nil {
		t.Fatal(err)
	}
	c.Record(context.Background(), 1)
	if err := m.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestNewUnknownDriver(t *testing.T) {
	cfg := config.Default().Metrics
	cfg.Driver = "statsd"
	_, err := New(context.Background(), cfg, "dev", nil)
	if err == nil || !strings.Contains(err.Error(), `unknown driver "statsd"`) || !strings.Contains(err.Error(), "prometheus") {
		t.Fatalf("New(statsd) = %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	m, err := New(context.Background(), promCfg("127.0.0.1:0"), "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
