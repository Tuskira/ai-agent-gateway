//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard/netguardtest"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/gateway"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// runAddrs are the listeners a gateway.Run test serves on.
type runAddrs struct{ mcp, api, llm, metrics string }

// setupRunEnv points gateway.Run (with no config file) at a fresh database
// on the GATEWAY_TEST_DATABASE_URL server and at free loopback ports, all
// through GATEWAY_* environment variables, and enables every plane plus
// the Prometheus listener. The database is dropped afterwards. config.Load
// installs the config's egress policy process-wide, so the loopback
// exemption the rest of this package relies on is put back at cleanup.
func setupRunEnv(t *testing.T) runAddrs {
	t.Helper()
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping the gateway.Run end-to-end tests")
	}
	if os.Getenv("GATEWAY_MASTER_KEY") == "" {
		t.Skip("GATEWAY_MASTER_KEY not set; skipping the gateway.Run end-to-end tests")
	}
	t.Cleanup(netguardtest.AllowLoopback)

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	admin, err := sql.Open("pgx", rawURL)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	db := "gwrun_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	if _, err := admin.Exec(fmt.Sprintf(`CREATE DATABASE %q`, db)); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, db))
		admin.Close()
	})

	pw, _ := u.User.Password()
	ssl := u.Query().Get("sslmode")
	if ssl == "" {
		ssl = "disable"
	}
	a := runAddrs{mcp: freeRunAddr(t), api: freeRunAddr(t), llm: freeRunAddr(t), metrics: freeRunAddr(t)}
	for k, v := range map[string]string{
		"GATEWAY_DATABASE_HOST":        u.Hostname(),
		"GATEWAY_DATABASE_PORT":        u.Port(),
		"GATEWAY_DATABASE_USER":        u.User.Username(),
		"GATEWAY_DATABASE_PASSWORD":    pw,
		"GATEWAY_DATABASE_DATABASE":    db,
		"GATEWAY_DATABASE_SSL_MODE":    ssl,
		"GATEWAY_MCP_ADDRESS":          a.mcp,
		"GATEWAY_API_ADDRESS":          a.api,
		"GATEWAY_LLM_PROXY_ENABLED":    "true",
		"GATEWAY_LLM_PROXY_ADDRESS":    a.llm,
		"GATEWAY_METRICS_DRIVER":       "prometheus",
		"GATEWAY_METRICS_ADDRESS":      a.metrics,
		"GATEWAY_LOGGING_LEVEL":        "warn",
		"GATEWAY_SINKS_STDOUT_ENABLED": "false",
	} {
		t.Setenv(k, v)
	}
	return a
}

func freeRunAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// startRun runs gateway.Run in the background until the test ends, waits
// for every plane's /health, and returns a stop func that cancels the
// context and returns Run's error.
func startRun(t *testing.T, a runAddrs, opts gateway.Options) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gateway.Run(ctx, opts) }()

	var once sync.Once
	var runErr error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case runErr = <-done:
			case <-time.After(20 * time.Second):
				runErr = errors.New("gateway.Run did not return within 20s of cancel")
			}
		})
		return runErr
	}
	t.Cleanup(func() { _ = stop() })

	deadline := time.Now().Add(20 * time.Second)
	for _, h := range []string{a.mcp + "/health", a.api + "/api/v1/health", a.llm + "/health", a.metrics + "/health"} {
		for {
			select {
			case err := <-done:
				t.Fatalf("gateway.Run returned during startup: %v", err)
			default:
			}
			resp, err := http.Get("http://" + h)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("GET %s never answered 200 (last error %v)", h, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return stop
}

// Run with empty Options is the built-in gateway: every plane answers
// /health, and canceling the context shuts it down cleanly (nil error,
// listeners closed).
func TestGatewayRunEmptyOptionsStartsAndStops(t *testing.T) {
	a := setupRunEnv(t)
	stop := startRun(t, a, gateway.Options{})

	resp, err := http.Get("http://" + a.llm + "/health")
	if err != nil {
		t.Fatal(err)
	}
	var health map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&health)
	resp.Body.Close()
	if health["plane"] != "llm" || health["version"] != gateway.DefaultVersion {
		t.Errorf("llm /health = %v, want plane llm, version %q", health, gateway.DefaultVersion)
	}

	if err := stop(); err != nil {
		t.Fatalf("Run after cancel = %v, want nil", err)
	}
	for _, addr := range []string{a.mcp, a.api, a.llm, a.metrics} {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			t.Errorf("%s still accepts connections after Run returned", addr)
		}
	}
}

// headerAuth accepts any request carrying X-Run-Test as an admin+agent of
// tenant, with keyID as its API key id; anything else is not its credential.
type headerAuth struct{ tenant, keyID string }

func (headerAuth) Name() string { return "run-test-header" }

func (h headerAuth) Authenticate(_ context.Context, r *http.Request) (*pkgauth.Principal, error) {
	if r.Header.Get("X-Run-Test") == "" {
		return nil, pkgauth.ErrNoCredential
	}
	return &pkgauth.Principal{Subject: h.keyID, TenantID: h.tenant, KeyID: h.keyID, AuthMethod: "run-test", Roles: []string{"admin", "agent"}}, nil
}

// recordLogSink keeps the LLM calls it is handed.
type recordLogSink struct {
	mu     sync.Mutex
	calls  []*sink.LLMCall
	closed bool
}

func (s *recordLogSink) WriteAccess(*sink.AccessLog) {}
func (s *recordLogSink) WriteLLMCall(c *sink.LLMCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
}
func (s *recordLogSink) Close() error { s.mu.Lock(); s.closed = true; s.mu.Unlock(); return nil }
func (s *recordLogSink) snapshot() ([]*sink.LLMCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*sink.LLMCall(nil), s.calls...), s.closed
}

// recordBatchSink is a capture store that keeps every batch.
type recordBatchSink struct{ recordLogSink }

func (s *recordBatchSink) WriteBatch(_ context.Context, calls []*sink.LLMCall) error {
	for _, c := range calls {
		s.WriteLLMCall(c)
	}
	return nil
}

// limitsLookup returns one key with a max_tokens limit, counting lookups.
type limitsLookup struct {
	maxTokens int
	n         atomic.Int32
}

func (l *limitsLookup) GetByID(_ context.Context, tenantID, id string) (*store.APIKey, error) {
	l.n.Add(1)
	return &store.APIKey{ID: id, TenantID: tenantID, Role: "agent", Limits: &store.Limits{MaxTokens: &l.maxTokens}}, nil
}

// overviewReader answers the analytics Overview and LLM Logs; the other
// Reader methods are not reached by this test.
type overviewReader struct {
	analytics.Reader
	overviews atomic.Int32
}

func (r *overviewReader) Overview(context.Context, string, analytics.Period) (*analytics.Overview, error) {
	r.overviews.Add(1)
	return &analytics.Overview{}, nil
}

func (r *overviewReader) ListLLMCalls(context.Context, string, analytics.LLMCallFilter) ([]sink.LLMCall, int, error) {
	return []sink.LLMCall{{RequestID: "from-supplied-reader"}}, 1, nil
}

// Every Options field reaches the running gateway: the extra authenticator
// sits after the API-key one, the program's pricer, capture store, log
// sink, key lookup and analytics reader are the ones used, and Run leaves
// the supplied sinks open for their owner.
func TestGatewayRunOptionsArePluggedIn(t *testing.T) {
	a := setupRunEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_run","stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":100}}`)
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("GATEWAY_LLM_PROXY_UPSTREAM_BASE_URL", upstream.URL)
	t.Setenv("GATEWAY_EGRESS_ALLOWED_CIDRS", "127.0.0.0/8")

	tenant := uuid.NewString()
	logSink, batch := &recordLogSink{}, &recordBatchSink{}
	keys := &limitsLookup{maxTokens: 64}
	reader := &overviewReader{}
	stop := startRun(t, a, gateway.Options{
		Version:        "run-test",
		Authenticators: []pkgauth.Authenticator{headerAuth{tenant: tenant, keyID: "key-run-test"}},
		LogSinks:       []sink.LogSink{logSink},
		BatchSink:      batch,
		Analytics:      reader,
		Pricer:         pricing.NewCardFromRates(map[string]pricing.Rate{"plug-model": {Input: 7, Output: 70}}),
		KeyLookup:      keys,
	})

	send := func(method, url string, hdr map[string]string, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	ours := map[string]string{"X-Run-Test": "1", "Content-Type": "application/json"}

	// Authenticator order: no credential is 401; ours alone is accepted; a
	// gateway API key is still decided by the API-key authenticator first,
	// so a bogus gk_ key is refused even with our header present.
	if code, _ := send("GET", "http://"+a.api+"/api/v1/analytics/llm-logs", nil, ""); code != http.StatusUnauthorized {
		t.Errorf("no credential: status %d, want 401", code)
	}
	code, body := send("GET", "http://"+a.api+"/api/v1/analytics/llm-logs", ours, "")
	if code != http.StatusOK || !strings.Contains(body, "from-supplied-reader") {
		t.Errorf("llm-logs with the supplied authenticator: %d %s, want 200 from the supplied reader", code, body)
	}
	bogus := map[string]string{"X-Run-Test": "1", "Authorization": "Bearer gk_bogusbogusbogusbogus"}
	if code, _ := send("GET", "http://"+a.api+"/api/v1/analytics/llm-logs", bogus, ""); code != http.StatusUnauthorized {
		t.Errorf("bogus gk_ key plus our header: status %d, want 401 (API key decided first)", code)
	}

	// Analytics reader and the custom pricing source.
	code, body = send("GET", "http://"+a.api+"/api/v1/analytics/overview?range=24h", ours, "")
	if code != http.StatusOK || !strings.Contains(body, `"pricingSource":"custom"`) || reader.overviews.Load() != 1 {
		t.Errorf("overview: %d %s (reader calls %d), want 200 with pricingSource custom from the supplied reader", code, body, reader.overviews.Load())
	}

	// KeyLookup: the supplied key's max_tokens limit refuses a larger request.
	code, body = send("POST", "http://"+a.llm+"/v1/messages", ours, `{"model":"plug-model","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusBadRequest || keys.n.Load() == 0 {
		t.Errorf("max_tokens over the supplied key's limit: %d %s (lookups %d), want 400", code, body, keys.n.Load())
	}

	// A call under the limit is relayed, priced by the supplied Pricer, and
	// captured to the supplied BatchSink and LogSink (which also hold the
	// refused call above: denials are captured too).
	code, body = send("POST", "http://"+a.llm+"/v1/messages", ours, `{"model":"plug-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK {
		t.Fatalf("llm call: %d %s, want 200", code, body)
	}
	want := (1000*7.0 + 100*70.0) / 1e6
	relayed := func(calls []*sink.LLMCall) *sink.LLMCall {
		for _, c := range calls {
			if c.StatusCode == http.StatusOK {
				return c
			}
		}
		return nil
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		bc, _ := batch.snapshot()
		lc, _ := logSink.snapshot()
		b, l := relayed(bc), relayed(lc)
		if b != nil && l != nil {
			for name, c := range map[string]*sink.LLMCall{"BatchSink": b, "LogSink": l} {
				if c.TenantID != tenant || c.CostUSD == nil || *c.CostUSD < want-1e-12 || *c.CostUSD > want+1e-12 {
					t.Errorf("%s got tenant %q cost %v, want tenant %q cost %v", name, c.TenantID, c.CostUSD, tenant, want)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no relayed call captured: %d batch / %d log-sink calls", len(bc), len(lc))
		}
		time.Sleep(50 * time.Millisecond)
	}

	resp, err := http.Get("http://" + a.llm + "/health")
	if err != nil {
		t.Fatal(err)
	}
	var health map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&health)
	resp.Body.Close()
	if health["version"] != "run-test" {
		t.Errorf("llm /health version = %v, want Options.Version", health["version"])
	}

	if err := stop(); err != nil {
		t.Fatalf("Run after cancel = %v, want nil", err)
	}
	if _, closed := logSink.snapshot(); closed {
		t.Error("Run closed the supplied LogSink; the caller owns it")
	}
	if _, closed := batch.snapshot(); closed {
		t.Error("Run closed the supplied BatchSink; the caller owns it")
	}
}
