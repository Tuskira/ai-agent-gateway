// Package prometheus is the "prometheus" metrics driver: a pull exporter
// that serves the gateway's metrics in the Prometheus text exposition
// format for a scraper to read.
//
// It bridges the OpenTelemetry instruments the gateway records through
// (pkg/metrics) to a private Prometheus registry with the OpenTelemetry
// Prometheus exporter, and adds the standard Go runtime (go_*) and
// process (process_*) collectors to the same registry. The registry is
// private to each Exporter, never prometheus.DefaultRegisterer, so two
// Exporters in one process (tests) do not collide.
//
// Naming: metric and label names are escaped to underscores and the
// Prometheus type suffixes are appended -- a counter "http_requests"
// becomes "<namespace>_http_requests_total", a histogram
// "http_request_duration_seconds" becomes
// "<namespace>_http_request_duration_seconds_bucket" (and _sum, _count).
// A unit given with metric.WithUnit is appended only when the name does
// not already end with it, so name instruments with their unit
// ("_seconds", "_bytes") and either omit WithUnit or pass the matching
// UCUM unit ("s", "By"). The OpenTelemetry scope labels and the
// target_info series are not exported.
//
// The driver registers itself from an init(); blank-import this package
// to make metrics.driver: prometheus available, as cmd/gateway does.
package prometheus

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/metrics"
)

// DriverName is the metrics.driver value that selects this driver.
const DriverName = "prometheus"

func init() {
	metrics.Register(DriverName, func(ctx context.Context, cfg metrics.Config) (metrics.Exporter, error) {
		return New(ctx, cfg)
	})
}

// Exporter is the prometheus driver's metrics.Exporter.
type Exporter struct {
	provider *sdkmetric.MeterProvider
	handler  http.Handler

	shutdownOnce sync.Once
	shutdownErr  error
}

var _ metrics.Exporter = (*Exporter)(nil)

// New builds an Exporter over a fresh private registry. cfg.Namespace
// prefixes every metric the gateway records (not the go_* and process_*
// collector series, which keep their standard names). Address, Path and
// Options are not read: the gateway serves Handler itself.
func New(_ context.Context, cfg metrics.Config) (*Exporter, error) {
	reg := prom.NewRegistry()
	if err := reg.Register(collectors.NewGoCollector()); err != nil {
		return nil, fmt.Errorf("prometheus: register go collector: %w", err)
	}
	if err := reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		return nil, fmt.Errorf("prometheus: register process collector: %w", err)
	}

	exp, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		otelprom.WithNamespace(cfg.Namespace),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
		otelprom.WithoutScopeInfo(),
		otelprom.WithoutTargetInfo(),
	)
	if err != nil {
		return nil, fmt.Errorf("prometheus: build exporter: %w", err)
	}

	return &Exporter{
		provider: sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp)),
		handler:  promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
	}, nil
}

// MeterProvider returns the OpenTelemetry provider the gateway creates its
// instruments from. It is the same value on every call.
func (e *Exporter) MeterProvider() metric.MeterProvider { return e.provider }

// Handler serves the registry in the Prometheus text format.
func (e *Exporter) Handler() http.Handler { return e.handler }

// Shutdown shuts the meter provider down once; later calls return nil.
// Instruments recorded after Shutdown are dropped.
func (e *Exporter) Shutdown(ctx context.Context) error {
	first := false
	e.shutdownOnce.Do(func() {
		first = true
		e.shutdownErr = e.provider.Shutdown(ctx)
	})
	if !first {
		return nil
	}
	return e.shutdownErr
}
