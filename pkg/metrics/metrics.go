// Package metrics is the public seam for the gateway's own operational
// metrics: the Exporter a metrics backend provides, and the driver
// registry that binds a `metrics.driver` name to an implementation.
//
// The gateway records every metric through the OpenTelemetry metric API
// (go.opentelemetry.io/otel/metric): it asks the Exporter for a
// MeterProvider, creates its instruments from that, and never learns how
// the numbers leave the process. An Exporter decides that. A pull
// exporter (the shipped "prometheus" driver) also returns an
// http.Handler, which the gateway serves on its own metrics listener
// (metrics.address + metrics.path); a push exporter (OTLP, StatsD, a
// vendor agent) returns a nil Handler and ships the numbers itself, and
// the gateway then opens no listener.
//
// It exists so a metrics backend can live in a separate Go module. The
// gateway's instruments, their names and their attribute rules stay in
// internal/metrics; everything a backend has to supply is expressed here.
//
// Two drivers ship in this module. "none" is built into this package (the
// default: a no-op MeterProvider, no listener, no cost on the hot path).
// pkg/metrics/prometheus registers "prometheus" from an init(), so a
// binary blank-imports it exactly as cmd/gateway does. A third backend is
// added by implementing Exporter, registering a Factory with Register,
// and passing pkg/metrics/metricstest -- see CONTRIBUTING.md, "Adding a
// metrics exporter".
package metrics

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// DriverNone is the built-in driver that exports nothing.
const DriverNone = "none"

// Config is what Open hands a driver: the gateway's metrics.* config
// fields, verbatim. A driver reads what it needs and ignores the rest.
type Config struct {
	// Driver is the name the driver was opened under (metrics.driver).
	Driver string
	// Address is the gateway's metrics listener host:port
	// (metrics.address). The gateway, not the driver, opens the listener;
	// a driver only needs it for its own bookkeeping, if at all.
	Address string
	// Path is where the gateway serves Exporter.Handler (metrics.path).
	Path string
	// Namespace is the prefix every exported metric name must carry
	// (metrics.namespace, default "gateway"): instruments are created
	// without it ("http_requests"), and the driver applies it on export
	// ("gateway_http_requests_total"). A driver whose backend has no
	// notion of a prefix should still apply it to the name.
	Namespace string
	// Options carries driver-specific settings a third-party driver
	// needs beyond the fields above, verbatim from metrics.options. The
	// built-in drivers ignore it.
	Options map[string]string
}

// Exporter is one metrics backend, opened once per process.
//
// MeterProvider must return the same non-nil provider on every call;
// the gateway creates all its instruments from it, at startup and
// possibly later, from many goroutines.
//
// Handler returns the scrape surface of a pull exporter, which the
// gateway mounts at Config.Path on its metrics listener. A push exporter
// returns nil, and the gateway opens no listener.
//
// Shutdown flushes anything buffered and releases the exporter. The
// gateway calls it once at shutdown, but it must be idempotent: a second
// call returns nil. Instruments used after Shutdown must not panic.
type Exporter interface {
	MeterProvider() metric.MeterProvider
	Handler() http.Handler
	Shutdown(ctx context.Context) error
}

// Factory builds a driver's Exporter from cfg.
type Factory func(ctx context.Context, cfg Config) (Exporter, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

func init() {
	Register(DriverNone, func(context.Context, Config) (Exporter, error) {
		return noneExporter{}, nil
	})
}

// Register makes a driver available to Open under name. It panics on an
// empty name, a nil factory or a name registered twice -- programmer
// errors caught at init time, not conditions to recover from. Drivers
// call it from an init() so a blank import is enough to enable them.
func Register(name string, factory Factory) {
	if strings.TrimSpace(name) == "" {
		panic("metrics: Register called with an empty driver name")
	}
	if factory == nil {
		panic("metrics: Register called with a nil factory for driver " + name)
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic("metrics: Register called twice for driver " + name)
	}
	registry[name] = factory
}

// Drivers returns the registered driver names, sorted.
func Drivers() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Open builds the Exporter of the driver registered under name. An
// unknown name is an error that lists the drivers that are registered,
// since the usual cause is a missing blank import.
func Open(ctx context.Context, name string, cfg Config) (Exporter, error) {
	registryMu.RLock()
	factory, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("metrics: unknown driver %q (registered: %s; missing blank import?)",
			name, strings.Join(Drivers(), ", "))
	}

	exp, err := factory(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("metrics: open driver %q: %w", name, err)
	}
	if exp == nil {
		return nil, fmt.Errorf("metrics: driver %q returned no exporter", name)
	}
	return exp, nil
}

// noneExporter is the "none" driver: every instrument is a no-op and
// there is nothing to serve or flush.
type noneExporter struct{}

func (noneExporter) MeterProvider() metric.MeterProvider { return noop.NewMeterProvider() }
func (noneExporter) Handler() http.Handler               { return nil }
func (noneExporter) Shutdown(context.Context) error      { return nil }
