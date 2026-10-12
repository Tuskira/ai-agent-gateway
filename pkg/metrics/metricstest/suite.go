// Package metricstest is the conformance suite every metrics.Exporter
// driver must pass.
//
// A new driver is added by implementing metrics.Exporter (see
// pkg/metrics), registering its Factory via metrics.Register in an
// init(), and then wiring this suite up as its test:
//
//	func TestConformance(t *testing.T) {
//		metricstest.Run(t, "gateway", func(t *testing.T) metrics.Exporter {
//			exp, err := metrics.Open(context.Background(), "mydriver",
//				metrics.Config{Driver: "mydriver", Namespace: "gateway"})
//			if err != nil {
//				t.Fatal(err)
//			}
//			return exp
//		})
//	}
//
// The namespace argument is the metrics.Config.Namespace the open
// function builds its Exporter with; the suite needs it because it checks
// that a scraped instrument carries the namespace prefix. open is called
// once per subtest and must return a fresh Exporter each time.
//
// Run exercises the contract documented on metrics.Exporter: a non-nil,
// stable MeterProvider; instruments that can be created and recorded
// from many goroutines; for a pull driver (non-nil Handler), a GET that
// answers 200 text/plain and exposes a recorded counter under
// "<namespace>_<name>"; and an idempotent Shutdown after which recording
// does not panic. A push driver (nil Handler) skips the scrape check.
//
// These tests run against the real backend -- there is no fake metrics
// collector in this module -- so a push driver's test should skip when
// its collector is not reachable.
package metricstest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/metric"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/metrics"
)

// meterName is the instrumentation scope the suite records under.
const meterName = "github.com/Tuskira/tusk-ai-secured-gateway/pkg/metrics/metricstest"

// Run drives the conformance suite against Exporters built by open,
// which is called once per subtest. namespace is the
// metrics.Config.Namespace open builds them with.
func Run(t *testing.T, namespace string, open func(t *testing.T) metrics.Exporter) {
	t.Helper()

	t.Run("MeterProviderIsStable", func(t *testing.T) { testProvider(t, openExp(t, open)) })
	t.Run("CounterRecords", func(t *testing.T) { testCounter(t, openExp(t, open)) })
	t.Run("ConcurrentRecording", func(t *testing.T) { testConcurrent(t, openExp(t, open)) })
	t.Run("ScrapeExposesNamespacedCounter", func(t *testing.T) { testScrape(t, namespace, openExp(t, open)) })
	t.Run("ShutdownIsIdempotent", func(t *testing.T) { testShutdown(t, open(t)) })
}

// openExp opens an Exporter and shuts it down when the subtest ends.
func openExp(t *testing.T, open func(t *testing.T) metrics.Exporter) metrics.Exporter {
	t.Helper()
	exp := open(t)
	if exp == nil {
		t.Fatal("open returned a nil Exporter")
	}
	t.Cleanup(func() {
		if err := exp.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return exp
}

func testProvider(t *testing.T, exp metrics.Exporter) {
	mp := exp.MeterProvider()
	if mp == nil {
		t.Fatal("MeterProvider() = nil")
	}
	if again := exp.MeterProvider(); again != mp {
		t.Errorf("MeterProvider() returned a different provider on the second call")
	}
	if exp.MeterProvider().Meter(meterName) == nil {
		t.Fatal("Meter() = nil")
	}
}

func testCounter(t *testing.T, exp metrics.Exporter) {
	m := exp.MeterProvider().Meter(meterName)

	c, err := m.Int64Counter("metricstest_counter", metric.WithDescription("conformance counter"))
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	c.Add(context.Background(), 3)

	f, err := m.Float64Counter("metricstest_float_counter")
	if err != nil {
		t.Fatalf("Float64Counter: %v", err)
	}
	f.Add(context.Background(), 0.25)

	h, err := m.Float64Histogram("metricstest_duration_seconds", metric.WithUnit("s"))
	if err != nil {
		t.Fatalf("Float64Histogram: %v", err)
	}
	h.Record(context.Background(), 0.01)

	g, err := m.Int64UpDownCounter("metricstest_in_flight")
	if err != nil {
		t.Fatalf("Int64UpDownCounter: %v", err)
	}
	g.Add(context.Background(), 1)
	g.Add(context.Background(), -1)

	_, err = m.Int64ObservableGauge("metricstest_gauge", metric.WithInt64Callback(
		func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(1)
			return nil
		}))
	if err != nil {
		t.Fatalf("Int64ObservableGauge: %v", err)
	}
}

func testConcurrent(t *testing.T, exp metrics.Exporter) {
	m := exp.MeterProvider().Meter(meterName)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			c, err := m.Int64Counter("metricstest_concurrent")
			if err != nil {
				t.Errorf("goroutine %d: Int64Counter: %v", i, err)
				return
			}
			for range 100 {
				c.Add(context.Background(), 1)
			}
		})
	}
	wg.Wait()
	scrape(t, exp) // a concurrent scrape after recording must not race either
}

func testScrape(t *testing.T, namespace string, exp metrics.Exporter) {
	if exp.Handler() == nil {
		t.Skip("push driver: Handler() is nil, nothing to scrape")
	}
	c, err := exp.MeterProvider().Meter(meterName).Int64Counter("metricstest_scraped")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	c.Add(context.Background(), 7)

	body := scrape(t, exp)
	want := namespace + "_metricstest_scraped"
	if !strings.Contains(body, want) {
		t.Fatalf("scrape body does not contain %q:\n%s", want, body)
	}
}

func testShutdown(t *testing.T, exp metrics.Exporter) {
	c, err := exp.MeterProvider().Meter(meterName).Int64Counter("metricstest_after_shutdown")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	if err := exp.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := exp.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	c.Add(context.Background(), 1) // must not panic
}

// scrape GETs the exporter's Handler, when it has one, and returns the
// body after checking the status and content type.
func scrape(t *testing.T, exp metrics.Exporter) string {
	t.Helper()
	h := exp.Handler()
	if h == nil {
		return ""
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("scrape status = %d, body:\n%s", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("scrape Content-Type = %q, want text/plain", ct)
	}
	return string(body)
}
