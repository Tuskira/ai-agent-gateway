//go:build e2e

package e2e

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/capture"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/llmplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/metrics"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	// The prometheus driver, registered as cmd/gateway's blank import does.
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/metrics/prometheus"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/stdout"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// TestLLMPlaneMetricsRealVendor sends one real chat completion through the
// LLM plane, wired as cmd/gateway wires it (the HTTP metrics middleware
// outside the auth middleware, the metrics sink on the capture recorder's
// analytics tee, a real prometheus-driver Metrics), and scrapes the
// exposition text. It follows the LLMTEST_* convention of the provider
// conformance tests and skips when they are unset:
//
//	LLMTEST_OPENAI_COMPAT_BASE_URL=https://api.openai.com/v1 \
//	LLMTEST_OPENAI_COMPAT_MODEL=gpt-4o-mini \
//	LLMTEST_OPENAI_COMPAT_API_KEY=... \
//	GATEWAY_TEST_DATABASE_URL=... go test -tags e2e ./test/e2e -run TestLLMPlaneMetricsRealVendor
func TestLLMPlaneMetricsRealVendor(t *testing.T) {
	base := os.Getenv("LLMTEST_OPENAI_COMPAT_BASE_URL")
	if base == "" {
		t.Skip("LLMTEST_OPENAI_COMPAT_BASE_URL not set; skipping the real-vendor LLM metrics test")
	}
	apiKey := os.Getenv("LLMTEST_OPENAI_COMPAT_API_KEY")
	if apiKey == "" {
		t.Skip("LLMTEST_OPENAI_COMPAT_API_KEY not set; skipping the real-vendor LLM metrics test")
	}
	model := os.Getenv("LLMTEST_OPENAI_COMPAT_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping the LLM metrics end-to-end test")
	}
	ctx := context.Background()

	cfg := config.Default()
	mcfg := cfg.Metrics
	mcfg.Driver = "prometheus"
	gwMetrics, err := metrics.New(ctx, mcfg, "e2e", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	t.Cleanup(func() { _ = gwMetrics.Close(context.Background()) })

	st := newIsolatedStore(t, rawURL)
	tenant := &store.Tenant{Slug: "e2e-llm-metrics-" + uuid.NewString()[:8], Name: "E2E LLM Metrics Tenant"}
	if err := st.Tenants().Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	agentKey := newAPIKey(t, st, tenant.ID, "e2e-agent", "agent")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authMiddleware := internalauth.Middleware(apikey.New(st.APIKeys(), apikey.Options{}), logger)

	// Durable capture stays out of this test (it asserts on the metrics
	// only), so the batch sink discards; the metrics sink rides the
	// recorder's analytics tee exactly as in cmd/gateway via sink.Multi.
	batch := stdout.New(io.Discard)
	t.Cleanup(func() { _ = batch.Close() })
	recorder := capture.NewRecorder(batch, sink.Multi(gwMetrics.Sink()), nil, 0)

	// The OpenAI provider's base_url carries no version segment, while the
	// LLMTEST convention (and the SDKs) include it.
	openaiBase := strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
	core, err := llmplane.Handler(llmplane.Config{
		UpstreamBaseURL:         cfg.LLMProxy.UpstreamBaseURL,
		MaxRequestBytes:         cfg.LLMProxy.Limits.MaxRequestBytes,
		MaxStreamDuration:       cfg.LLMProxy.Limits.MaxStreamDuration,
		MaxConcurrentPerTenant:  cfg.LLMProxy.Limits.MaxConcurrentPerTenant,
		Authorizer:              pkgauth.NewRoleAuthorizer(),
		OpenAIEnabled:           true,
		OpenAIBaseURL:           openaiBase,
		MaxCaptureRequestBytes:  cfg.LLMProxy.Capture.MaxRequestBytes,
		MaxCaptureResponseBytes: cfg.LLMProxy.Capture.MaxResponseBytes,
		Pricing:                 pricing.Default,
	}, recorder)
	if err != nil {
		t.Fatalf("build llm plane: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok","plane":"llm"}`)
	})
	mux.Handle("/", authMiddleware(core))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler:     gwMetrics.Instrument("llm", metrics.LLMRoute)(mux),
		ReadTimeout: 30 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})

	body := `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"Reply with the single word: pong"}]}`
	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/openai/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Key", agentKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /openai/v1/chat/completions: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("real vendor call through the gateway = %d %s", resp.StatusCode, out)
	}

	scrape := func() string {
		rec := httptest.NewRecorder()
		gwMetrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}
	// The HTTP counter is recorded when the handler returns, the call
	// counter when the capture row is written; wait for both.
	callsSeries := `gateway_llm_calls_total{model="` + model + `",provider="openai",status="200",stream="false",tenant="` + tenant.ID + `"}`
	deadline := time.Now().Add(10 * time.Second)
	for metricValue(scrape(), callsSeries) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached 1:\n%s", callsSeries, scrape())
		}
		time.Sleep(50 * time.Millisecond)
	}
	text := scrape()
	for _, line := range regexp.MustCompile(`(?m)^gateway_(llm_|http_requests_total).*$`).FindAllString(text, -1) {
		if !strings.Contains(line, "_bucket{") {
			t.Log(line)
		}
	}

	if got := metricValue(text, callsSeries); got != 1 {
		t.Errorf("%s = %v, want 1", callsSeries, got)
	}
	tokens := func(kind string) float64 {
		return metricValue(text, `gateway_llm_tokens_total{kind="`+kind+`",model="`+model+`",provider="openai",tenant="`+tenant.ID+`"}`)
	}
	if got := tokens("input"); got <= 0 {
		t.Errorf("input tokens = %v, want > 0", got)
	}
	if got := tokens("output"); got <= 0 {
		t.Errorf("output tokens = %v, want > 0", got)
	}
	if got := metricValue(text, `gateway_llm_call_duration_seconds_count{model="`+model+`",provider="openai"}`); got != 1 {
		t.Errorf("call duration count = %v, want 1", got)
	}
	if got := metricValue(text, `gateway_http_requests_total{method="POST",plane="llm",route="openai",status="200"}`); got != 1 {
		t.Errorf("http_requests_total{plane=llm,route=openai,status=200} = %v, want 1", got)
	}
}

// metricValue returns the value of the exposition line for series (the
// metric name and its exact label set), or -1 when it is absent.
func metricValue(body, series string) float64 {
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)`).FindStringSubmatch(body)
	if m == nil {
		return -1
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	return v
}
