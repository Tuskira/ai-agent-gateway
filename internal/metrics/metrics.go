// Package metrics owns the gateway's operational metrics: it opens the
// pkg/metrics exporter named by metrics.driver, hands out the one Meter
// every gateway instrument is created from, and builds the listener a
// pull exporter (Prometheus) is scraped on.
//
// Instrument naming rules (the exporter applies the namespace, "gateway"
// by default, and the type suffix):
//   - Name instruments without a namespace and without "_total":
//     "http_requests", "llm_tokens". The Prometheus driver exports a
//     counter as gateway_http_requests_total.
//   - Put the unit in the name ("http_request_duration_seconds",
//     "body_bytes"). Either omit metric.WithUnit or pass the matching
//     UCUM unit ("s", "By"); a mismatched unit is appended to the name.
//   - Attribute values must be bounded: never a key id, request id,
//     session id, user, raw path or a model id taken from a URL.
//
// A nil *Metrics is valid: its Meter is a no-op, Handler and Routine are
// nil, and Close does nothing. Code that takes a *Metrics can therefore
// be built in tests without one.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/httpplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/supervisor"
	pkgmetrics "github.com/Tuskira/tusk-ai-secured-gateway/pkg/metrics"
)

// MeterName is the instrumentation scope of every gateway instrument.
const MeterName = "github.com/Tuskira/tusk-ai-secured-gateway"

// Listener timeouts. A scrape is a short GET, so the write side is
// bounded too, unlike the streaming planes.
const (
	readTimeout  = 30 * time.Second
	writeTimeout = 30 * time.Second
	idleTimeout  = 120 * time.Second
)

// Metrics is the process's metrics exporter and Meter. Build one with New
// in cmd/gateway's run() and pass it to whatever records metrics.
type Metrics struct {
	cfg    config.Metrics
	exp    pkgmetrics.Exporter
	meter  metric.Meter
	logger *slog.Logger

	http httpState // HTTP instruments, created on first Instrument call
}

// New opens the exporter named by cfg.Driver (which must be registered
// with pkg/metrics, usually by a blank import in cmd/gateway) and
// registers the gateway_build_info gauge, labelled with version and the
// Go toolchain version.
func New(ctx context.Context, cfg config.Metrics, version string, logger *slog.Logger) (*Metrics, error) {
	if logger == nil {
		logger = slog.Default()
	}
	exp, err := pkgmetrics.Open(ctx, cfg.Driver, pkgmetrics.Config{
		Driver:    cfg.Driver,
		Address:   cfg.Address,
		Path:      cfg.Path,
		Namespace: cfg.Namespace,
		Options:   cfg.Options,
	})
	if err != nil {
		return nil, err
	}

	m := &Metrics{
		cfg:    cfg,
		exp:    exp,
		meter:  exp.MeterProvider().Meter(MeterName, metric.WithInstrumentationVersion(version)),
		logger: logger,
	}

	buildAttrs := metric.WithAttributes(
		attribute.String("version", version),
		attribute.String("go_version", runtime.Version()),
	)
	if _, err := m.meter.Int64ObservableGauge("build_info",
		metric.WithDescription("Always 1; labels carry the gateway build version and Go version."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(1, buildAttrs)
			return nil
		}),
	); err != nil {
		_ = exp.Shutdown(ctx)
		return nil, fmt.Errorf("metrics: register build_info: %w", err)
	}
	return m, nil
}

// Meter returns the Meter every gateway instrument is created from. On a
// nil *Metrics it is a no-op Meter.
func (m *Metrics) Meter() metric.Meter {
	if m == nil {
		return noop.NewMeterProvider().Meter(MeterName)
	}
	return m.meter
}

// Enabled reports whether a real driver (anything but "none") is in use.
// Callers may use it to skip work whose only purpose is a metric.
func (m *Metrics) Enabled() bool {
	return m != nil && m.cfg.Driver != config.MetricsDriverNone
}

// Handler is the exporter's scrape surface, or nil for a push driver,
// the "none" driver and a nil *Metrics.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return nil
	}
	return m.exp.Handler()
}

// Close shuts the exporter down, flushing anything a push driver has
// buffered. It is safe to call more than once.
func (m *Metrics) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	return m.exp.Shutdown(ctx)
}

// Routine returns the metrics listener as a supervisor.Routine: an
// unauthenticated HTTP server on metrics.address that serves Handler at
// metrics.path and a liveness probe at GET /health. It returns nil when
// there is nothing to serve (Handler is nil), and the caller then adds no
// routine.
func (m *Metrics) Routine() supervisor.Routine {
	h := m.Handler()
	if h == nil {
		return nil
	}
	return httpplane.New("metrics", m.cfg.Address, newMux(m.cfg.Path, h),
		readTimeout, writeTimeout, idleTimeout, m.logger)
}

// newMux routes by exact path rather than through http.ServeMux, so no
// metrics.path value can be misread as a ServeMux pattern.
func newMux(path string, metricsHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == path:
			metricsHandler.ServeHTTP(w, r)
		case r.URL.Path == "/health" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	})
}
