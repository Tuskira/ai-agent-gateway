package config

import (
	"context"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard/netguardtest"
)

func TestDatabaseDefaultPasswordWarning(t *testing.T) {
	cases := []struct {
		name string
		db   Database
		warn bool
	}{
		{"compose service name, no tls", Database{Host: "postgres", Password: "gateway", SSLMode: "disable"}, false},
		{"loopback, no tls", Database{Host: "127.0.0.1", Password: "gateway", SSLMode: "disable"}, false},
		{"localhost, no tls", Database{Host: "localhost", Password: "gateway", SSLMode: "disable"}, false},
		{"ipv6 loopback", Database{Host: "::1", Password: "gateway", SSLMode: "disable"}, false},
		{"remote fqdn", Database{Host: "db.prod.example.com", Password: "gateway", SSLMode: "disable"}, true},
		{"remote ip", Database{Host: "10.0.3.4", Password: "gateway", SSLMode: "disable"}, true},
		{"service name but tls required", Database{Host: "postgres", Password: "gateway", SSLMode: "require"}, true},
		{"loopback but tls", Database{Host: "localhost", Password: "gateway", SSLMode: "verify-full"}, true},
		{"strong password anywhere", Database{Host: "db.prod.example.com", Password: "s3cret", SSLMode: "require"}, false},
	}
	for _, c := range cases {
		if got := c.db.DefaultPasswordWarning() != ""; got != c.warn {
			t.Errorf("%s: warn = %v, want %v", c.name, got, c.warn)
		}
	}
}

func TestEgressEnvOverlayAndValidation(t *testing.T) {
	t.Cleanup(netguardtest.AllowLoopback) // Load installs the policy process-wide

	t.Setenv("GATEWAY_EGRESS_ALLOWED_CIDRS", "127.0.0.0/8, ::1/128")
	t.Setenv("GATEWAY_EGRESS_ALLOWED_HOSTS", "host.docker.internal")
	t.Setenv("GATEWAY_CONNECTORS_ALLOW_BEARER_TOKEN_FORWARDING", "true")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Egress.AllowedCIDRs) != 2 || cfg.Egress.AllowedHosts[0] != "host.docker.internal" || !cfg.Connectors.AllowBearerTokenForwarding {
		t.Errorf("egress/connectors = %+v / %+v", cfg.Egress, cfg.Connectors)
	}
	if err := ValidateMCPCatalogURL(context.Background(), "url", "http://127.0.0.1:9000/mcp"); err != nil {
		t.Errorf("allowlisted loopback catalog URL: %v", err)
	}
	if err := ValidateMCPCatalogURL(context.Background(), "url", "http://localhost:9000/mcp"); err != nil {
		t.Errorf("allowlisted loopback catalog URL by hostname: %v", err)
	}

	t.Setenv("GATEWAY_EGRESS_ALLOWED_CIDRS", "not-a-cidr")
	if _, err := Load(""); err == nil {
		t.Error("bad egress.allowed_cidrs accepted")
	}

	t.Setenv("GATEWAY_EGRESS_ALLOWED_CIDRS", "")
	t.Setenv("GATEWAY_EGRESS_ALLOWED_HOSTS", "")
	t.Setenv("GATEWAY_CONNECTORS_ALLOW_BEARER_TOKEN_FORWARDING", "false")
	if _, err := Load(""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMCPCatalogURL(context.Background(), "url", "http://127.0.0.1:9000/mcp"); err == nil || !strings.Contains(err.Error(), "egress_blocked") {
		t.Errorf("default policy: catalog loopback err = %v, want egress_blocked", err)
	}
	// The hostname form must be rejected too, not just the IP literal --
	// this is the gap CheckHostResolve closes (CheckHost alone only
	// catches an IP literal; "localhost" is not one).
	if err := ValidateMCPCatalogURL(context.Background(), "url", "http://localhost:9000/mcp"); err == nil || !strings.Contains(err.Error(), "egress_blocked") {
		t.Errorf("default policy: catalog localhost err = %v, want egress_blocked", err)
	}
	if err := ValidateMCPCatalogURL(context.Background(), "url", "https://169.254.169.254/x"); err == nil {
		t.Error("metadata IP accepted in catalog URL")
	}
}
