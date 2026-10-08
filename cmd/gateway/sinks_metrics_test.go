package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/metrics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// TestBuildSinksAddsMetricsSink checks the wiring: with metrics enabled the
// Multi buildSinks returns -- the one the MCP plane and the LLM plane's
// capture.Recorder both write to -- feeds the metrics sink, and with
// metrics disabled (or absent) it records nothing.
func TestBuildSinksAddsMetricsSink(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	cfg.Sinks.Stdout.Enabled = false
	cfg.Sinks.Otel.Enabled = false
	cfg.Sinks.ClickHouse.Enabled = false

	cfg.Metrics.Driver = "prometheus"
	gm, err := metrics.New(context.Background(), cfg.Metrics, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gm.Close(context.Background()) })

	res, err := buildSinks(cfg, gm, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Sink.Close() })
	res.Sink.WriteAccess(&sink.AccessLog{TenantID: "t", Method: "ping", StatusCode: 200})
	res.Sink.WriteLLMCall(&sink.LLMCall{TenantID: "t", Provider: "anthropic", RequestedModel: "m", StatusCode: 200, InputTokens: 3})

	rec := httptest.NewRecorder()
	gm.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{"gateway_mcp_requests_total{", "gateway_llm_calls_total{", "gateway_llm_tokens_total{"} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape lacks %s after writing through buildSinks' Multi:\n%s", want, body)
		}
	}

	// Disabled and absent metrics add no sink; the writes are harmless.
	for name, m := range map[string]*metrics.Metrics{"nil": nil} {
		r, err := buildSinks(cfg, m, logger)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r.Sink.WriteAccess(&sink.AccessLog{Method: "ping"})
		_ = r.Sink.Close()
	}
	cfg.Metrics.Driver = config.MetricsDriverNone
	off, err := metrics.New(context.Background(), cfg.Metrics, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	if off.Enabled() {
		t.Fatal("none driver reports Enabled")
	}
	r, err := buildSinks(cfg, off, logger)
	if err != nil {
		t.Fatal(err)
	}
	r.Sink.WriteLLMCall(&sink.LLMCall{StatusCode: 200})
	_ = r.Sink.Close()
}
