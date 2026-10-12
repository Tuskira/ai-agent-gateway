package prometheus_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/metrics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/metrics/metricstest"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/metrics/prometheus"
)

func open(t *testing.T, namespace string) metrics.Exporter {
	t.Helper()
	// Through the registry, as the gateway opens it, so the init()
	// registration is covered too.
	exp, err := metrics.Open(context.Background(), prometheus.DriverName,
		metrics.Config{Driver: prometheus.DriverName, Address: ":9464", Path: "/metrics", Namespace: namespace})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(context.Background()) })
	return exp
}

func TestConformance(t *testing.T) {
	metricstest.Run(t, "gateway", func(t *testing.T) metrics.Exporter { return open(t, "gateway") })
}

func scrape(t *testing.T, exp metrics.Exporter) string {
	t.Helper()
	rec := httptest.NewRecorder()
	exp.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestScrapeIncludesRuntimeAndProcessCollectors(t *testing.T) {
	body := scrape(t, open(t, "gateway"))
	if !strings.Contains(body, "\ngo_goroutines ") {
		t.Errorf("scrape lacks go_goroutines:\n%s", body)
	}
	// The process collector reports nothing on platforms without procfs
	// support; it does on Linux (CI) and on macOS.
	if !strings.Contains(body, "process_") {
		t.Errorf("scrape lacks process_* series:\n%s", body)
	}
	if strings.Contains(body, "target_info") || strings.Contains(body, "otel_scope") {
		t.Errorf("scrape carries OTel target/scope info, want none:\n%s", body)
	}
}

func TestNamingCounterAndHistogram(t *testing.T) {
	exp := open(t, "gateway")
	m := exp.MeterProvider().Meter("test")
	ctx := context.Background()

	foo, err := m.Int64Counter("foo")
	if err != nil {
		t.Fatal(err)
	}
	foo.Add(ctx, 2, metric.WithAttributes(attribute.String("plane", "mcp")))

	bar, err := m.Float64Histogram("bar_seconds", metric.WithUnit("s"))
	if err != nil {
		t.Fatal(err)
	}
	bar.Record(ctx, 0.2)

	body := scrape(t, exp)
	for _, want := range []string{
		`gateway_foo_total{plane="mcp"} 2`,
		"# TYPE gateway_foo_total counter",
		"gateway_bar_seconds_bucket{",
		"gateway_bar_seconds_sum 0.2",
		"gateway_bar_seconds_count 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape lacks %q:\n%s", want, body)
		}
	}
	// The unit is already in the name, so it must not be appended again.
	if strings.Contains(body, "bar_seconds_seconds") {
		t.Errorf("unit suffix duplicated:\n%s", body)
	}
}

func TestNamespaceIsApplied(t *testing.T) {
	exp := open(t, "aigw")
	c, err := exp.MeterProvider().Meter("test").Int64Counter("llm_tokens")
	if err != nil {
		t.Fatal(err)
	}
	c.Add(context.Background(), 5)
	body := scrape(t, exp)
	if !strings.Contains(body, "aigw_llm_tokens_total 5") {
		t.Errorf("scrape lacks aigw_llm_tokens_total 5:\n%s", body)
	}
	if strings.Contains(body, "gateway_llm_tokens") {
		t.Errorf("default namespace leaked:\n%s", body)
	}
}

func TestExplicitBucketBoundariesHonored(t *testing.T) {
	exp := open(t, "gateway")
	h, err := exp.MeterProvider().Meter("test").Float64Histogram("baz_seconds",
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 2.5))
	if err != nil {
		t.Fatal(err)
	}
	h.Record(context.Background(), 0.3)

	body := scrape(t, exp)
	re := regexp.MustCompile(`gateway_baz_seconds_bucket\{le="([^"]+)"\} (\d+)`)
	var got []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		got = append(got, m[1]+"="+m[2])
	}
	want := []string{"0.1=0", "0.5=1", "2.5=1", "+Inf=1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("buckets = %v, want %v\n%s", got, want, body)
	}
}

func TestExportersAreIsolated(t *testing.T) {
	a, b := open(t, "gateway"), open(t, "gateway")
	c, err := a.MeterProvider().Meter("test").Int64Counter("only_in_a")
	if err != nil {
		t.Fatal(err)
	}
	c.Add(context.Background(), 1)
	if strings.Contains(scrape(t, b), "only_in_a") {
		t.Error("a counter recorded on one Exporter appeared on another")
	}
}
