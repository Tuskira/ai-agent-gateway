package metrics_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/metrics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/metrics/metricstest"
)

func TestNoneConformance(t *testing.T) {
	metricstest.Run(t, "gateway", func(t *testing.T) metrics.Exporter {
		exp, err := metrics.Open(context.Background(), metrics.DriverNone, metrics.Config{Driver: metrics.DriverNone, Namespace: "gateway"})
		if err != nil {
			t.Fatal(err)
		}
		return exp
	})
}

func TestNoneIsNoop(t *testing.T) {
	exp, err := metrics.Open(context.Background(), metrics.DriverNone, metrics.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if exp.Handler() != nil {
		t.Error("none driver Handler() should be nil")
	}
	if _, ok := exp.MeterProvider().(noop.MeterProvider); !ok {
		t.Errorf("none driver MeterProvider() = %T, want noop.MeterProvider", exp.MeterProvider())
	}
}

func TestOpenUnknownDriverListsRegistered(t *testing.T) {
	_, err := metrics.Open(context.Background(), "nope", metrics.Config{})
	if err == nil {
		t.Fatal("Open(nope) = nil error")
	}
	for _, want := range []string{`unknown driver "nope"`, "none", "missing blank import?"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// seq makes test driver names unique, so the tests survive -count=N
// (the registry is process-global and Register panics on a repeat).
var seq atomic.Int64

func uniqueName(prefix string) string { return fmt.Sprintf("%s_%d", prefix, seq.Add(1)) }

type stubExporter struct{ cfg metrics.Config }

func (stubExporter) MeterProvider() metric.MeterProvider { return noop.NewMeterProvider() }
func (stubExporter) Handler() http.Handler               { return nil }
func (stubExporter) Shutdown(context.Context) error      { return nil }

func TestRegisterAndOpenPassesConfig(t *testing.T) {
	name := uniqueName("metrics_test_cfg")
	metrics.Register(name, func(_ context.Context, cfg metrics.Config) (metrics.Exporter, error) {
		return stubExporter{cfg: cfg}, nil
	})
	in := metrics.Config{Driver: name, Address: ":1", Path: "/m", Namespace: "ns", Options: map[string]string{"k": "v"}}
	exp, err := metrics.Open(context.Background(), name, in)
	if err != nil {
		t.Fatal(err)
	}
	got := exp.(stubExporter).cfg
	if got.Driver != in.Driver || got.Address != in.Address || got.Path != in.Path || got.Namespace != in.Namespace || got.Options["k"] != "v" {
		t.Errorf("factory got %+v, want %+v", got, in)
	}
	found := false
	for _, d := range metrics.Drivers() {
		found = found || d == name
	}
	if !found {
		t.Errorf("Drivers() = %v, missing %s", metrics.Drivers(), name)
	}
}

func TestOpenWrapsFactoryErrorAndNilExporter(t *testing.T) {
	boom := errors.New("boom")
	errName, nilName := uniqueName("metrics_test_err"), uniqueName("metrics_test_nil")
	metrics.Register(errName, func(context.Context, metrics.Config) (metrics.Exporter, error) { return nil, boom })
	metrics.Register(nilName, func(context.Context, metrics.Config) (metrics.Exporter, error) { return nil, nil })

	if _, err := metrics.Open(context.Background(), errName, metrics.Config{}); !errors.Is(err, boom) {
		t.Errorf("Open(err driver) = %v, want wrapping boom", err)
	}
	if _, err := metrics.Open(context.Background(), nilName, metrics.Config{}); err == nil ||
		!strings.Contains(err.Error(), "returned no exporter") {
		t.Errorf("Open(nil driver) = %v, want 'returned no exporter'", err)
	}
}

func TestRegisterPanics(t *testing.T) {
	f := func(context.Context, metrics.Config) (metrics.Exporter, error) { return stubExporter{}, nil }
	cases := map[string]func(){
		"empty name":  func() { metrics.Register(" ", f) },
		"nil factory": func() { metrics.Register("metrics_test_nilfactory", nil) },
		"duplicate":   func() { metrics.Register(metrics.DriverNone, f) },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Register did not panic")
				}
			}()
			fn()
		})
	}
}
