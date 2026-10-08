//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/metrics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	// The prometheus driver, registered as cmd/gateway's blank import does.
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/metrics/prometheus"
)

// TestMCPPlaneHTTPMetricsEndToEnd serves the real MCP plane behind the HTTP
// metrics middleware, built from a real prometheus-driver Metrics exactly
// as cmd/gateway builds it, makes real calls over a socket against real
// Postgres and a real MCP backend, and scrapes the exposition text. The
// SSE stream is part of the check: the middleware's writer wrapper has to
// keep http.Flusher, or GET /mcp/stream cannot deliver a single frame.
func TestMCPPlaneHTTPMetricsEndToEnd(t *testing.T) {
	cfg := config.Default().Metrics
	cfg.Driver = "prometheus"
	gwMetrics, err := metrics.New(context.Background(), cfg, "e2e", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	t.Cleanup(func() { _ = gwMetrics.Close(context.Background()) })

	h, secretSvc := newFixture(t)
	addr, _ := startPlaneWrapped(t, h.store, secretSvc, gwMetrics.Instrument("mcp", metrics.MCPRoute))
	h.baseURL = "http://" + addr

	// Bounded, so a writer wrapper that loses http.Flusher fails the test
	// instead of leaving the stream request waiting for headers forever.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, decoded, raw := h.rpc(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}`),
	}, rpcOptions{})
	if decoded.Error != nil {
		t.Fatalf("initialize failed: %s", raw)
	}
	sessionID := resp.Header.Get(mcp.HeaderSessionID)

	if _, decoded, raw = h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsList},
		rpcOptions{SessionID: sessionID}); decoded.Error != nil {
		t.Fatalf("tools/list failed: %s", raw)
	}

	// An unauthenticated call is still counted, as a 401.
	if resp, _, _ = h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 3, Method: mcp.MethodToolsList},
		rpcOptions{NoAuth: true}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated tools/list = %d, want 401", resp.StatusCode)
	}

	// The SSE stream still streams through the wrapper: three progress
	// notifications from a slow tool reach the session's stream.
	frames := h.sessionStream(t, ctx, sessionID)
	if _, decoded, raw = h.rpc(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 4, Method: mcp.MethodToolsCall,
		Params: json.RawMessage(`{"name":"alpha__` + dptest.SlowTool + `","arguments":{"steps":3},"_meta":{"progressToken":"m"}}`),
	}, rpcOptions{SessionID: sessionID}); decoded.Error != nil {
		t.Fatalf("tools/call failed: %s", raw)
	}
	for want := 1; want <= 3; want++ {
		select {
		case frame := <-frames:
			t.Logf("stream <- %s", frame)
		case <-time.After(10 * time.Second):
			t.Fatalf("frame %d never reached the SSE stream: the middleware lost http.Flusher", want)
		}
	}

	// Scrape the same handler the metrics listener serves.
	scrape := func() string {
		rec := httptest.NewRecorder()
		gwMetrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}
	value := func(body, series string) float64 {
		m := regexp.MustCompile(regexp.QuoteMeta(series) + ` (\S+)`).FindStringSubmatch(body)
		if m == nil {
			return -1
		}
		v, _ := strconv.ParseFloat(m[1], 64)
		return v
	}

	body := scrape()
	for _, line := range regexp.MustCompile(`(?m)^gateway_http.*$`).FindAllString(body, -1) {
		t.Log(line)
	}

	// initialize + tools/list + tools/call, all authenticated POST /mcp.
	if got := value(body, `gateway_http_requests_total{method="POST",plane="mcp",route="/mcp",status="200"}`); got < 3 {
		t.Errorf("POST /mcp 200 = %v, want >= 3 (initialize, tools/list, tools/call)", got)
	}
	if got := value(body, `gateway_http_requests_total{method="POST",plane="mcp",route="/mcp",status="401"}`); got != 1 {
		t.Errorf("POST /mcp 401 = %v, want 1", got)
	}
	// The stream is recorded when it ends, so it is in flight now.
	if got := value(body, `gateway_http_requests_in_flight{plane="mcp"}`); got != 1 {
		t.Errorf("in flight = %v, want 1 (the open SSE stream)", got)
	}
	if got := value(body, `gateway_http_request_duration_seconds_count{method="POST",plane="mcp",route="/mcp"}`); got < 4 {
		t.Errorf("duration count = %v, want >= 4", got)
	}

	// Closing the stream records it under its own route.
	cancel()
	deadline := time.Now().Add(10 * time.Second)
	for value(scrape(), `gateway_http_requests_total{method="GET",plane="mcp",route="/mcp/stream",status="200"}`) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("closed SSE stream never recorded:\n%s", scrape())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := value(scrape(), `gateway_http_requests_in_flight{plane="mcp"}`); got != 0 {
		t.Errorf("in flight after the stream closed = %v, want 0", got)
	}
}
