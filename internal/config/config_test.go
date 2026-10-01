package config

import (
	"context"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard/netguardtest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg.MCP.Address != "0.0.0.0:8080" || !cfg.MCP.Enabled {
		t.Errorf("MCP defaults = %+v", cfg.MCP)
	}
	if cfg.API.Address != "0.0.0.0:8081" || !cfg.API.Enabled {
		t.Errorf("API defaults = %+v", cfg.API)
	}
	if cfg.LLMProxy.Address != "0.0.0.0:8082" || cfg.LLMProxy.Enabled {
		t.Errorf("LLMProxy defaults = %+v", cfg.LLMProxy)
	}
	if cfg.LLMProxy.UpstreamBaseURL != "https://api.anthropic.com" {
		t.Errorf("LLMProxy.UpstreamBaseURL = %q", cfg.LLMProxy.UpstreamBaseURL)
	}
	if cfg.LLMProxy.Providers.OpenAI.InjectStreamUsage {
		t.Error("LLMProxy.Providers.OpenAI.InjectStreamUsage default should be false (opt-in: it rewrites the upstream body)")
	}
	if cfg.LLMProxy.PricingFile != "" {
		t.Errorf("LLMProxy.PricingFile default should be empty (embedded card only), got %q", cfg.LLMProxy.PricingFile)
	}
	if cfg.Database.Host != "localhost" || cfg.Database.Port != 5432 || cfg.Database.SSLMode != "require" {
		t.Errorf("Database defaults = %+v", cfg.Database)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("Logging.Level = %q", cfg.Logging.Level)
	}
	if !cfg.Sinks.Stdout.Enabled {
		t.Error("Sinks.Stdout.Enabled default should be true")
	}
	if cfg.Sinks.Otel.Enabled || cfg.Sinks.Otel.Protocol != "http" {
		t.Errorf("Sinks.Otel defaults = %+v", cfg.Sinks.Otel)
	}
	if cfg.Sinks.ClickHouse.Enabled || cfg.Sinks.ClickHouse.Port != 9000 || cfg.Sinks.ClickHouse.BatchSize != 100 ||
		cfg.Sinks.ClickHouse.FlushInterval != 5*time.Second || cfg.Sinks.ClickHouse.BufferSize != 1000 {
		t.Errorf("Sinks.ClickHouse defaults = %+v", cfg.Sinks.ClickHouse)
	}
	if cfg.Ingest.Enabled || cfg.Ingest.MaxBodyBytes != 32<<20 || cfg.Ingest.MaxRecords != 1000 || cfg.Ingest.RatePerMinute != 120 {
		t.Errorf("Ingest defaults = %+v", cfg.Ingest)
	}

	if !cfg.Auth.ConsoleAPIKeyLogin || cfg.Auth.CookieSecure != "auto" ||
		cfg.Auth.SessionIdle != 8*time.Hour || cfg.Auth.SessionMax != 24*time.Hour {
		t.Errorf("Auth console-login defaults = %+v", cfg.Auth)
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Default() config should validate cleanly: %v", err)
	}
}

func TestLoad_EnvOverlay_ConsoleLogin(t *testing.T) {
	t.Setenv("GATEWAY_AUTH_CONSOLE_API_KEY_LOGIN", "false")
	t.Setenv("GATEWAY_AUTH_COOKIE_SECURE", "true")
	t.Setenv("GATEWAY_AUTH_SESSION_IDLE", "2h")
	t.Setenv("GATEWAY_AUTH_SESSION_MAX", "12h")
	t.Setenv("GATEWAY_AUTH_DEFAULT_TENANT", "acme")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.ConsoleAPIKeyLogin || cfg.Auth.CookieSecure != "true" || cfg.Auth.SessionIdle != 2*time.Hour ||
		cfg.Auth.SessionMax != 12*time.Hour || cfg.Auth.DefaultTenant != "acme" {
		t.Errorf("Auth = %+v, want env values applied", cfg.Auth)
	}
}

func TestLoad_EmptyPathSkipsFile(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error = %v", err)
	}
	if cfg.Service.Name != Default().Service.Name {
		t.Errorf("expected default service name, got %q", cfg.Service.Name)
	}
}

func TestLoad_MissingExplicitPathIsError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yml"))
	if err == nil {
		t.Fatal("expected error for missing explicit config path, got nil")
	}
}

func TestLoad_YAMLOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	yamlContent := `
service:
  name: test-gateway
  version: "1.2.3"
mcp:
  address: "0.0.0.0:9090"
api:
  enabled: false
database:
  host: db.internal
  port: 5433
logging:
  level: debug
  development: true
`
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Service.Name != "test-gateway" || cfg.Service.Version != "1.2.3" {
		t.Errorf("Service = %+v", cfg.Service)
	}
	if cfg.MCP.Address != "0.0.0.0:9090" {
		t.Errorf("MCP.Address = %q", cfg.MCP.Address)
	}
	if cfg.API.Enabled {
		t.Error("API.Enabled should be false")
	}
	if cfg.Database.Host != "db.internal" || cfg.Database.Port != 5433 {
		t.Errorf("Database = %+v", cfg.Database)
	}
	// Fields not present in the YAML should keep their defaults.
	if cfg.Database.User != "gateway" {
		t.Errorf("Database.User should keep default, got %q", cfg.Database.User)
	}
	if cfg.Logging.Level != "debug" || !cfg.Logging.Development {
		t.Errorf("Logging = %+v", cfg.Logging)
	}
}

// TestLoad_YAMLOverlay_LLMProxyPricingFile pins the YAML path for
// llm_proxy.pricing_file (env var coverage is TestLoad_EnvOverlay).
func TestLoad_YAMLOverlay_LLMProxyPricingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	yamlContent := `
llm_proxy:
  enabled: true
  pricing_file: "/opt/gateway/prices-override.json"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.LLMProxy.PricingFile != "/opt/gateway/prices-override.json" {
		t.Errorf("LLMProxy.PricingFile = %q", cfg.LLMProxy.PricingFile)
	}
}

// TestLoad_CustomAuthRoles pins that auth.roles loads as a plain
// map[string][]string via YAML (there's no env-var path for it -- see
// Auth.Roles' doc comment -- so this is the only load path to test).
func TestLoad_CustomAuthRoles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	yamlContent := `
auth:
  roles:
    reader: ["*.read"]
    billing-admin: ["billing.*", "*.read"]
`
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got := cfg.Auth.Roles["reader"]; len(got) != 1 || got[0] != "*.read" {
		t.Errorf("Auth.Roles[reader] = %v, want [*.read]", got)
	}
	if got := cfg.Auth.Roles["billing-admin"]; len(got) != 2 || got[0] != "billing.*" || got[1] != "*.read" {
		t.Errorf("Auth.Roles[billing-admin] = %v, want [billing.* *.read]", got)
	}
	// Built-in api_keys/dev_mode defaults are untouched by the merge.
	if !cfg.Auth.APIKeys.Enabled {
		t.Error("Auth.APIKeys.Enabled should keep its default of true")
	}
}

func TestLoad_EnvOverlay(t *testing.T) {
	t.Setenv("GATEWAY_MCP_ADDRESS", ":9090")
	t.Setenv("GATEWAY_LLM_PROXY_ENABLED", "true")
	t.Setenv("GATEWAY_LLM_PROXY_PRICING_FILE", "/etc/gateway/prices-override.json")
	t.Setenv("GATEWAY_DATABASE_HOST", "postgres")
	t.Setenv("GATEWAY_DATABASE_PORT", "5555")
	t.Setenv("GATEWAY_MCP_READ_TIMEOUT", "45s")
	t.Setenv("GATEWAY_DATABASE_MIGRATE", "true")
	t.Setenv("GATEWAY_SKILLS_SEED_DIR", "/etc/gateway/skills")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.MCP.Address != ":9090" {
		t.Errorf("MCP.Address = %q", cfg.MCP.Address)
	}
	if !cfg.LLMProxy.Enabled {
		t.Error("LLMProxy.Enabled should be true")
	}
	if cfg.LLMProxy.PricingFile != "/etc/gateway/prices-override.json" {
		t.Errorf("LLMProxy.PricingFile = %q", cfg.LLMProxy.PricingFile)
	}
	if cfg.Database.Host != "postgres" {
		t.Errorf("Database.Host = %q", cfg.Database.Host)
	}
	if cfg.Database.Port != 5555 {
		t.Errorf("Database.Port = %d", cfg.Database.Port)
	}
	if cfg.MCP.ReadTimeout != 45*time.Second {
		t.Errorf("MCP.ReadTimeout = %v", cfg.MCP.ReadTimeout)
	}
	if !cfg.Database.Migrate {
		t.Error("Database.Migrate should be true")
	}
	if cfg.Skills.SeedDir != "/etc/gateway/skills" {
		t.Errorf("Skills.SeedDir = %q", cfg.Skills.SeedDir)
	}
}

// The ingest section gets env names by the same derivation as every other
// scalar-leaf section (see overlayStruct's doc comment).
func TestLoad_EnvOverlay_Ingest(t *testing.T) {
	t.Setenv("GATEWAY_INGEST_ENABLED", "true")
	t.Setenv("GATEWAY_INGEST_MAX_BODY_BYTES", "1048576")
	t.Setenv("GATEWAY_INGEST_MAX_RECORDS", "50")
	t.Setenv("GATEWAY_INGEST_RATE_PER_MINUTE", "30")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Ingest.Enabled || cfg.Ingest.MaxBodyBytes != 1048576 || cfg.Ingest.MaxRecords != 50 || cfg.Ingest.RatePerMinute != 30 {
		t.Errorf("Ingest = %+v", cfg.Ingest)
	}
}

// The nested llm_proxy.capture.body_store section gets env names by the same
// derivation as every other section.
func TestLoad_EnvOverlay_BodyStore(t *testing.T) {
	t.Setenv("GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_TYPE", "s3")
	t.Setenv("GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_INLINE_MAX_BYTES", "4096")
	t.Setenv("GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_S3_BUCKET", "bodies")
	t.Setenv("GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_S3_FORCE_PATH_STYLE", "true")
	t.Setenv("GATEWAY_LLM_PROXY_CAPTURE_BODY_STORE_S3_SECRET_ACCESS_KEY", "sekret")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	bs := cfg.LLMProxy.Capture.BodyStore
	if bs.Type != "s3" || bs.InlineMaxBytes != 4096 || bs.S3.Bucket != "bodies" || !bs.S3.ForcePathStyle ||
		bs.S3.SecretAccessKey != "sekret" || bs.S3.Prefix != "llm-bodies" {
		t.Errorf("BodyStore = %+v", bs)
	}
}

func TestLoad_EnvOverlayInvalidValue(t *testing.T) {
	t.Setenv("GATEWAY_DATABASE_PORT", "not-a-number")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error for invalid int env var")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid default",
			mutate:  func(c *Config) {},
			wantErr: false,
		},
		{
			name: "empty address on enabled plane",
			mutate: func(c *Config) {
				c.MCP.Address = ""
			},
			wantErr: true,
		},
		{
			name: "two enabled planes on same address",
			mutate: func(c *Config) {
				c.API.Address = c.MCP.Address
			},
			wantErr: true,
		},
		{
			name: "disabled plane may share an address",
			mutate: func(c *Config) {
				c.LLMProxy.Address = c.MCP.Address // LLMProxy disabled by default
			},
			wantErr: false,
		},
		{
			name:    "cookie_secure true is valid",
			mutate:  func(c *Config) { c.Auth.CookieSecure = "true" },
			wantErr: false,
		},
		{
			name:    "invalid cookie_secure",
			mutate:  func(c *Config) { c.Auth.CookieSecure = "sometimes" },
			wantErr: true,
		},
		{
			name:    "zero session_idle",
			mutate:  func(c *Config) { c.Auth.SessionIdle = 0 },
			wantErr: true,
		},
		{
			name:    "zero session_max",
			mutate:  func(c *Config) { c.Auth.SessionMax = 0 },
			wantErr: true,
		},
		{
			name:    "session_idle longer than session_max",
			mutate:  func(c *Config) { c.Auth.SessionIdle = 48 * time.Hour },
			wantErr: true,
		},
		{
			name: "invalid log level",
			mutate: func(c *Config) {
				c.Logging.Level = "verbose"
			},
			wantErr: true,
		},
		{
			name: "invalid upstream url",
			mutate: func(c *Config) {
				c.LLMProxy.UpstreamBaseURL = "not a url"
			},
			wantErr: true,
		},
		{
			name: "empty upstream url",
			mutate: func(c *Config) {
				c.LLMProxy.UpstreamBaseURL = ""
			},
			wantErr: true,
		},
		{
			name: "otel sink enabled without endpoint",
			mutate: func(c *Config) {
				c.Sinks.Otel.Enabled = true
			},
			wantErr: true,
		},
		{
			name: "otel sink enabled with endpoint and bad protocol",
			mutate: func(c *Config) {
				c.Sinks.Otel.Enabled = true
				c.Sinks.Otel.Endpoint = "localhost:4318"
				c.Sinks.Otel.Protocol = "websocket"
			},
			wantErr: true,
		},
		{
			name: "otel sink enabled with endpoint is valid",
			mutate: func(c *Config) {
				c.Sinks.Otel.Enabled = true
				c.Sinks.Otel.Endpoint = "localhost:4318"
			},
			wantErr: false,
		},
		{
			name: "clickhouse sink enabled without host",
			mutate: func(c *Config) {
				c.Sinks.ClickHouse.Enabled = true
			},
			wantErr: true,
		},
		{
			name: "clickhouse sink enabled with host is valid",
			mutate: func(c *Config) {
				c.Sinks.ClickHouse.Enabled = true
				c.Sinks.ClickHouse.Host = "localhost"
			},
			wantErr: false,
		},
		{
			name: "ingest enabled with valid defaults",
			mutate: func(c *Config) {
				c.Ingest.Enabled = true
			},
			wantErr: false,
		},
		{
			name: "ingest enabled with non-positive max_body_bytes",
			mutate: func(c *Config) {
				c.Ingest.Enabled = true
				c.Ingest.MaxBodyBytes = 0
			},
			wantErr: true,
		},
		{
			name: "ingest enabled with non-positive max_records",
			mutate: func(c *Config) {
				c.Ingest.Enabled = true
				c.Ingest.MaxRecords = 0
			},
			wantErr: true,
		},
		{
			name: "ingest enabled with non-positive rate_per_minute",
			mutate: func(c *Config) {
				c.Ingest.Enabled = true
				c.Ingest.RatePerMinute = 0
			},
			wantErr: true,
		},
		{
			name: "ingest disabled ignores otherwise-invalid fields",
			mutate: func(c *Config) {
				c.Ingest.MaxBodyBytes = 0
				c.Ingest.MaxRecords = 0
				c.Ingest.RatePerMinute = 0
			},
			wantErr: false,
		},
		{
			name:    "unknown body store type",
			mutate:  func(c *Config) { c.LLMProxy.Capture.BodyStore.Type = "gcs" },
			wantErr: true,
		},
		{
			name:    "filesystem body store without root",
			mutate:  func(c *Config) { c.LLMProxy.Capture.BodyStore.Type = "filesystem" },
			wantErr: true,
		},
		{
			name: "filesystem body store with root is valid",
			mutate: func(c *Config) {
				c.LLMProxy.Capture.BodyStore.Type = "filesystem"
				c.LLMProxy.Capture.BodyStore.Filesystem.Root = "/var/lib/gateway/bodies"
			},
			wantErr: false,
		},
		{
			name:    "s3 body store without bucket",
			mutate:  func(c *Config) { c.LLMProxy.Capture.BodyStore.Type = "s3" },
			wantErr: true,
		},
		{
			name: "s3 body store with relative endpoint",
			mutate: func(c *Config) {
				c.LLMProxy.Capture.BodyStore.Type = "s3"
				c.LLMProxy.Capture.BodyStore.S3.Bucket = "b"
				c.LLMProxy.Capture.BodyStore.S3.Endpoint = "minio:9000"
			},
			wantErr: true,
		},
		{
			name:    "negative inline_max_bytes",
			mutate:  func(c *Config) { c.LLMProxy.Capture.BodyStore.InlineMaxBytes = -1 },
			wantErr: true,
		},
		{
			// A name that is neither built-in is a third-party pkg/session
			// driver; only pkg/session.Open can tell whether the binary
			// registered it, so Validate lets it through.
			name: "third-party session store passes validation",
			mutate: func(c *Config) {
				c.Sessions.Store = "etcd"
			},
			wantErr: false,
		},
		{
			name: "empty session store means memory",
			mutate: func(c *Config) {
				c.Sessions.Store = ""
			},
			wantErr: false,
		},
		{
			name: "redis session store without redis enabled",
			mutate: func(c *Config) {
				c.Sessions.Store = SessionStoreRedis
			},
			wantErr: true,
		},
		{
			name: "redis session store without an address",
			mutate: func(c *Config) {
				c.Sessions.Store = SessionStoreRedis
				c.Redis.Enabled = true
				c.Redis.Addr = " "
			},
			wantErr: true,
		},
		{
			name: "redis session store with redis enabled is valid",
			mutate: func(c *Config) {
				c.Sessions.Store = SessionStoreRedis
				c.Redis.Enabled = true
			},
			wantErr: false,
		},
		{
			name: "redis cluster mode is not supported",
			mutate: func(c *Config) {
				c.Sessions.Store = SessionStoreRedis
				c.Redis.Enabled = true
				c.Redis.Cluster = true
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(cfg)
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDatabaseDSN(t *testing.T) {
	d := Database{
		Host:     "localhost",
		Port:     5432,
		User:     "gateway",
		Password: "secret",
		Database: "gateway",
		SSLMode:  "disable",
	}
	want := "postgres://gateway:secret@localhost:5432/gateway?sslmode=disable"
	if got := d.DSN(); got != want {
		t.Errorf("DSN() = %q, want %q", got, want)
	}
}

// ReAWSRegion is exported so internal/llmplane can apply the SAME rule to the
// region a request carries (bedrockHost); a region either package accepts must
// be one the other accepts, or a value that fails startup would still be
// interpolated into an upstream host at request time.
func TestReAWSRegion(t *testing.T) {
	for _, ok := range []string{"us-east-1", "eu-central-2", "ap-southeast-4", "us-gov-west-1", "ap-northeast-3", "il-central-1"} {
		if !ReAWSRegion.MatchString(ok) {
			t.Errorf("ReAWSRegion rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "x@evil.com/#", "us-east-1.evil.com/", "US-EAST-1", "us-east-1/", "us_east_1", "a@b"} {
		if ReAWSRegion.MatchString(bad) {
			t.Errorf("ReAWSRegion accepted %q", bad)
		}
	}
}

// allowed_role_accounts entries: a bare account (partition "aws"), a
// partition-qualified account, or "*". Anything else fails startup.
func TestValidate_BedrockAllowedRoleAccounts(t *testing.T) {
	for entry, wantOK := range map[string]bool{
		"":                                 true, // role mode off
		"*":                                true,
		"123456789012":                     true,
		" 123456789012 , 999999999999 ":    true,
		"aws-us-gov:123456789012":          true,
		"aws-cn:123456789012":              true,
		"aws:123456789012":                 true,
		"12345678901":                      false, // 11 digits
		"aws-us-gov:12345678901":           false,
		"AWS-US-GOV:123456789012":          false,
		"arn:aws:iam::123456789012:role/x": false,
		"123456789012,not-an-account":      false,
	} {
		cfg := Default()
		cfg.LLMProxy.Bedrock.Enabled = true
		cfg.LLMProxy.Bedrock.AllowedRoleAccounts = entry
		if err := cfg.Validate(); (err == nil) != wantOK {
			t.Errorf("allowed_role_accounts=%q: err = %v, want ok=%v", entry, err, wantOK)
		}
	}
}

// llm_proxy.models is validated at startup exactly like an API write: name
// shape, at least one target, vendor rules, no credential on a platform
// row, non-negative price, no duplicate names.
func TestValidate_LLMProxyModels(t *testing.T) {
	netguardtest.AllowLoopback() // Load in other tests resets the policy
	good := ModelSeed{
		Name: "sonnet", Description: "default",
		Targets: []ModelSeedTarget{
			{Vendor: "anthropic", Model: "claude-sonnet-4-5"},
			{Vendor: "bedrock", Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0", Region: "us-east-1"},
			{Vendor: "openai_compat", Model: "gpt-4o", BaseURL: "https://llm.internal/v1"},
			{Vendor: "openai_compat", Model: "local", BaseURL: "http://127.0.0.1:11434"},
		},
		Price: &ModelSeedPrice{Input: 3, Output: 15},
	}
	cases := map[string]struct {
		mutate func(m *ModelSeed) []ModelSeed
		wantOK bool
	}{
		"valid":              {func(m *ModelSeed) []ModelSeed { return []ModelSeed{*m} }, true},
		"uppercase name":     {func(m *ModelSeed) []ModelSeed { m.Name = "Sonnet"; return []ModelSeed{*m} }, false},
		"no targets":         {func(m *ModelSeed) []ModelSeed { m.Targets = nil; return []ModelSeed{*m} }, false},
		"unknown vendor":     {func(m *ModelSeed) []ModelSeed { m.Targets[0].Vendor = "cohere"; return []ModelSeed{*m} }, false},
		"bedrock w/o region": {func(m *ModelSeed) []ModelSeed { m.Targets[1].Region = ""; return []ModelSeed{*m} }, false},
		"bedrock base_url":   {func(m *ModelSeed) []ModelSeed { m.Targets[1].BaseURL = "https://x"; return []ModelSeed{*m} }, false},
		"compat w/o base":    {func(m *ModelSeed) []ModelSeed { m.Targets[2].BaseURL = ""; return []ModelSeed{*m} }, false},
		"http non-loopback":  {func(m *ModelSeed) []ModelSeed { m.Targets[2].BaseURL = "http://llm.internal"; return []ModelSeed{*m} }, false},
		"credential on seed": {func(m *ModelSeed) []ModelSeed { m.Targets[0].Credential = "x"; return []ModelSeed{*m} }, false},
		"negative price":     {func(m *ModelSeed) []ModelSeed { m.Price = &ModelSeedPrice{Input: -1}; return []ModelSeed{*m} }, false},
		"duplicate name":     {func(m *ModelSeed) []ModelSeed { return []ModelSeed{*m, *m} }, false},
		"label":              {func(m *ModelSeed) []ModelSeed { m.Targets[2].Label = "groq"; return []ModelSeed{*m} }, true},
		"label openai":       {func(m *ModelSeed) []ModelSeed { m.Targets[2].Label = "openai"; return []ModelSeed{*m} }, true},
		"label reserved":     {func(m *ModelSeed) []ModelSeed { m.Targets[2].Label = "anthropic"; return []ModelSeed{*m} }, false},
		"label reserved 2":   {func(m *ModelSeed) []ModelSeed { m.Targets[2].Label = "bedrock"; return []ModelSeed{*m} }, false},
		"label uppercase":    {func(m *ModelSeed) []ModelSeed { m.Targets[2].Label = "Groq"; return []ModelSeed{*m} }, false},
		"label too long":     {func(m *ModelSeed) []ModelSeed { m.Targets[2].Label = strings.Repeat("a", 33); return []ModelSeed{*m} }, false},
		"label on anthropic": {func(m *ModelSeed) []ModelSeed { m.Targets[0].Label = "groq"; return []ModelSeed{*m} }, false},
		"relative base_url":  {func(m *ModelSeed) []ModelSeed { m.Targets[2].BaseURL = "/v1"; return []ModelSeed{*m} }, false},
		"http ipv6 loopback": {func(m *ModelSeed) []ModelSeed { m.Targets[2].BaseURL = "http://[::1]/v1"; return []ModelSeed{*m} }, true},
	}
	for name, c := range cases {
		cfg := Default()
		seed := good
		seed.Targets = append([]ModelSeedTarget(nil), good.Targets...)
		cfg.LLMProxy.Models = c.mutate(&seed)
		if err := cfg.Validate(); (err == nil) != c.wantOK {
			t.Errorf("%s: err = %v, want ok=%v", name, err, c.wantOK)
		}
	}
}

func TestLoad_YAMLOverlay_LLMProxyModels(t *testing.T) {
	// One target's base_url is a loopback hostname ("localhost"), which
	// CheckHostResolve now judges by its resolved address just like an
	// IP literal. Load installs the egress policy from
	// GATEWAY_EGRESS_ALLOWED_CIDRS/HOSTS (overriding anything set before
	// the call -- see Load's "Install the egress policy before
	// Validate" comment), so allowlisting loopback through that env var
	// is what reaches Validate, not a netguardtest.AllowLoopback() call
	// made beforehand. This test is about the YAML shape, not egress
	// policy, hence the allowlist.
	t.Setenv("GATEWAY_EGRESS_ALLOWED_CIDRS", "127.0.0.0/8,::1/128")
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(`
llm_proxy:
  models:
    - name: sonnet
      description: Default
      targets:
        - {vendor: anthropic, model: claude-sonnet-4-5, base_url: "https://anthropic-proxy.internal", allow_caller_key: true}
        - {vendor: bedrock, model: us.anthropic.claude-sonnet-4-5-20250929-v1:0, region: us-east-1}
        - {vendor: openai_compat, model: qwen3, base_url: "http://localhost:11434/v1", label: ollama, allow_caller_key: true}
      price: {input: 3, output: 15, cache_read: 0.3, cache_write: 3.75}
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.LLMProxy.Models) != 1 || cfg.LLMProxy.Models[0].Name != "sonnet" || len(cfg.LLMProxy.Models[0].Targets) != 3 ||
		cfg.LLMProxy.Models[0].Targets[1].Region != "us-east-1" || cfg.LLMProxy.Models[0].Price == nil || cfg.LLMProxy.Models[0].Price.CacheWrite != 3.75 {
		t.Errorf("models = %+v", cfg.LLMProxy.Models)
	}
	if t0 := cfg.LLMProxy.Models[0].Targets; !t0[0].AllowCallerKey || t0[1].AllowCallerKey {
		t.Errorf("allow_caller_key = %v/%v, want true/false", t0[0].AllowCallerKey, t0[1].AllowCallerKey)
	}
	if t2 := cfg.LLMProxy.Models[0].Targets[2]; t2.Label != "ollama" || t2.BaseURL != "http://localhost:11434/v1" || !t2.AllowCallerKey {
		t.Errorf("targets[2] = %+v, want label ollama", t2)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidateModelTarget_HTTPHosts(t *testing.T) {
	netguardtest.AllowLoopback() // Load in other tests resets the policy
	ok := []string{
		"http://localhost:11434/v1",
		"http://127.0.0.1:11434/v1",
		"http://[::1]:11434/v1",
		"http://host.docker.internal:11434/v1",
		"https://api.groq.com/openai/v1",
	}
	for _, u := range ok {
		if err := ValidateModelTarget(context.Background(), "openai_compat", "m", u, ""); err != nil {
			t.Errorf("%s: unexpected error: %v", u, err)
		}
	}
	bad := []string{
		"http://llm.internal/v1",
		"http://host.docker.internal.evil.com/v1",
		"http://evil.host.docker.internal/v1",
		"http://localhost.evil.com/v1",
		"http://127.0.0.1.nip.io/v1",
	}
	for _, u := range bad {
		if err := ValidateModelTarget(context.Background(), "openai_compat", "m", u, ""); err == nil {
			t.Errorf("%s: accepted, want https-required error", u)
		}
	}
}

func TestValidateCatalogBaseURL(t *testing.T) {
	cases := []struct {
		raw         string
		wantTrimmed string
		wantWarning bool
		wantErr     bool
	}{
		{"https://api.openai.com/v1", "https://api.openai.com/v1", false, false},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1", false, false}, // trailing slash trimmed
		{"https://generativelanguage.googleapis.com/v1beta/openai", "https://generativelanguage.googleapis.com/v1beta/openai", false, false},
		{"https://api.together.ai/v2beta", "https://api.together.ai/v2beta", false, false},
		{"https://api.example.com", "https://api.example.com", true, false},    // no version segment -> warning, not an error
		{"https://api.example.com///", "https://api.example.com", true, false}, // multiple trailing slashes trimmed
		{"http://api.example.com/v1", "", false, true},                         // http on a non-loopback host
		{"http://localhost:1234/v1", "http://localhost:1234/v1", false, false}, // loopback http is fine; /v1 present, no warning
		{"https://api.example.com/v1?x=1", "", false, true},                    // query
		{"not a url", "", false, true},
		{"", "", false, true},
	}
	for _, c := range cases {
		trimmed, warning, err := ValidateCatalogBaseURL(context.Background(), c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: error = nil, want an error", c.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error: %v", c.raw, err)
			continue
		}
		if trimmed != c.wantTrimmed {
			t.Errorf("%q: trimmed = %q, want %q", c.raw, trimmed, c.wantTrimmed)
		}
		if (warning != "") != c.wantWarning {
			t.Errorf("%q: warning = %q, want non-empty=%v", c.raw, warning, c.wantWarning)
		}
	}
}

func TestValidateCatalogSuggestedName(t *testing.T) {
	ok := []string{"glm-5.3-nebius", "kimi-k3-nebius", "gpt-6.1-sol-openai", "a", "abc123"}
	for _, n := range ok {
		if err := ValidateCatalogSuggestedName(n); err != nil {
			t.Errorf("%q: unexpected error: %v", n, err)
		}
	}
	bad := []string{"", "-leading-dash", ".leading-dot", "_leading-underscore", "Has-Upper", "has space", strings.Repeat("a", 65)}
	for _, n := range bad {
		if err := ValidateCatalogSuggestedName(n); err == nil {
			t.Errorf("%q: accepted, want an error", n)
		}
	}
}

func TestValidateCatalogProviderSlug(t *testing.T) {
	ok := []string{"nebius", "together", "openai", "a1-b2"}
	for _, s := range ok {
		if err := ValidateCatalogProviderSlug(s); err != nil {
			t.Errorf("%q: unexpected error: %v", s, err)
		}
	}
	bad := []string{"", "-leading-dash", "Has-Upper", "has_underscore", strings.Repeat("a", 33)}
	for _, s := range bad {
		if err := ValidateCatalogProviderSlug(s); err == nil {
			t.Errorf("%q: accepted, want an error", s)
		}
	}
}

// connectors.default_timeout_ms must be one the API accepts back on PUT
// (100-600000), or every connector created with it fails its next edit.
func TestValidate_ConnectorDefaultTimeoutRange(t *testing.T) {
	for v, ok := range map[int]bool{99: false, 100: true, 600000: true, 600001: false} {
		cfg := Default()
		cfg.Connectors.DefaultTimeoutMS = v
		if err := cfg.Validate(); (err == nil) != ok {
			t.Errorf("default_timeout_ms %d: err = %v, want ok=%v", v, err, ok)
		}
	}
}
