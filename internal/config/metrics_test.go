package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefault_Metrics(t *testing.T) {
	m := Default().Metrics
	if m.Driver != MetricsDriverNone || m.Address != ":9464" || m.Path != "/metrics" || m.Namespace != "gateway" || m.Options != nil {
		t.Errorf("Metrics defaults = %+v", m)
	}
}

func TestValidate_Metrics(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Metrics)
		wantErr string
	}{
		{name: "default none", mutate: func(*Metrics) {}},
		{name: "prometheus with defaults", mutate: func(m *Metrics) { m.Driver = "prometheus" }},
		{name: "third-party driver name", mutate: func(m *Metrics) { m.Driver = "otlp" }},
		{name: "empty driver", mutate: func(m *Metrics) { m.Driver = "" }, wantErr: "metrics: driver is required"},
		{name: "blank driver", mutate: func(m *Metrics) { m.Driver = "  " }, wantErr: "metrics: driver is required"},
		{
			name:   "none ignores the listener fields",
			mutate: func(m *Metrics) { m.Address, m.Path, m.Namespace = "", "metrics", "9bad" },
		},
		{
			name:    "empty address",
			mutate:  func(m *Metrics) { m.Driver, m.Address = "prometheus", "" },
			wantErr: "metrics: address is required",
		},
		{
			name:    "path without slash",
			mutate:  func(m *Metrics) { m.Driver, m.Path = "prometheus", "metrics" },
			wantErr: "metrics: path must start with",
		},
		{
			name:    "empty path",
			mutate:  func(m *Metrics) { m.Driver, m.Path = "prometheus", "" },
			wantErr: "metrics: path must start with",
		},
		{
			name:    "namespace starting with digit",
			mutate:  func(m *Metrics) { m.Driver, m.Namespace = "prometheus", "9gw" },
			wantErr: "metrics: namespace must match",
		},
		{
			name:    "namespace with dash",
			mutate:  func(m *Metrics) { m.Driver, m.Namespace = "prometheus", "ai-gw" },
			wantErr: "metrics: namespace must match",
		},
		{
			name:    "empty namespace",
			mutate:  func(m *Metrics) { m.Driver, m.Namespace = "prometheus", "" },
			wantErr: "metrics: namespace must match",
		},
		{
			name:   "underscore namespace",
			mutate: func(m *Metrics) { m.Driver, m.Namespace = "prometheus", "_ai_gw2" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg.Metrics)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// The metrics section gets env names by the same derivation as every other
// scalar-leaf section (see overlayStruct's doc comment).
func TestLoad_EnvOverlay_Metrics(t *testing.T) {
	t.Setenv("GATEWAY_METRICS_DRIVER", "prometheus")
	t.Setenv("GATEWAY_METRICS_ADDRESS", "127.0.0.1:9999")
	t.Setenv("GATEWAY_METRICS_PATH", "/prom")
	t.Setenv("GATEWAY_METRICS_NAMESPACE", "aigw")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := Metrics{Driver: "prometheus", Address: "127.0.0.1:9999", Path: "/prom", Namespace: "aigw"}
	if cfg.Metrics.Driver != want.Driver || cfg.Metrics.Address != want.Address ||
		cfg.Metrics.Path != want.Path || cfg.Metrics.Namespace != want.Namespace {
		t.Errorf("Metrics = %+v, want %+v", cfg.Metrics, want)
	}
}

func TestLoad_EnvOverlay_MetricsInvalidFailsStartup(t *testing.T) {
	t.Setenv("GATEWAY_METRICS_DRIVER", "prometheus")
	t.Setenv("GATEWAY_METRICS_PATH", "metrics")

	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "metrics: path") {
		t.Fatalf("Load() error = %v, want metrics path error", err)
	}
}

func TestLoad_YAML_Metrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yml")
	yml := `metrics:
  driver: prometheus
  address: "127.0.0.1:19464"
  path: /prom
  namespace: aigw
  options:
    push_interval: 15s
`
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	m := cfg.Metrics
	if m.Driver != "prometheus" || m.Address != "127.0.0.1:19464" || m.Path != "/prom" || m.Namespace != "aigw" ||
		m.Options["push_interval"] != "15s" {
		t.Errorf("Metrics = %+v", m)
	}
}
