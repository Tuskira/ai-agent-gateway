package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoad_TrustedProxiesEnvListAndValidation(t *testing.T) {
	t.Setenv("GATEWAY_API_TRUSTED_PROXIES", "10.0.0.0/8, 192.168.1.1 ,fd00::/8")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"10.0.0.0/8", "192.168.1.1", "fd00::/8"}
	if len(cfg.API.TrustedProxies) != len(want) {
		t.Fatalf("TrustedProxies = %v, want %v", cfg.API.TrustedProxies, want)
	}
	for i := range want {
		if cfg.API.TrustedProxies[i] != want[i] {
			t.Errorf("TrustedProxies[%d] = %q, want %q", i, cfg.API.TrustedProxies[i], want[i])
		}
	}

	t.Setenv("GATEWAY_API_TRUSTED_PROXIES", "10.0.0.0/8,not-a-cidr")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "trusted_proxies") {
		t.Errorf("malformed trusted proxy: err = %v, want a trusted_proxies error", err)
	}
}

func TestDefault_AuthLimitsAndCacheTTL(t *testing.T) {
	cfg := Default()
	if cfg.Auth.APIKeys.CacheTTL != 30*time.Second {
		t.Errorf("CacheTTL = %v, want 30s", cfg.Auth.APIKeys.CacheTTL)
	}
	if cfg.Auth.RateLimit.CredentialMaxFailures != 20 || cfg.Auth.RateLimit.CredentialWindow != 5*time.Minute {
		t.Errorf("credential limit = %d/%v, want 20/5m", cfg.Auth.RateLimit.CredentialMaxFailures, cfg.Auth.RateLimit.CredentialWindow)
	}
}
