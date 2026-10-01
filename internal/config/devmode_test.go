package config

import (
	"strings"
	"testing"
)

func TestValidate_DevModeRefusesNonLoopback(t *testing.T) {
	cases := []struct {
		name    string
		mcp     string
		api     string
		allow   bool
		wantErr bool
	}{
		{"default 0.0.0.0 refused", "0.0.0.0:8081", "0.0.0.0:8080", false, true},
		{"empty host refused", ":8081", "127.0.0.1:8080", false, true},
		{"one remote plane refused", "127.0.0.1:8081", "0.0.0.0:8080", false, true},
		{"ipv6 any refused", "[::]:8081", "127.0.0.1:8080", false, true},
		{"loopback ok", "127.0.0.1:8081", "127.0.0.1:8080", false, false},
		{"localhost ok", "localhost:8081", "[::1]:8080", false, false},
		{"allow_remote lifts it", "0.0.0.0:8081", "0.0.0.0:8080", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Auth.DevMode.Enabled = true
			cfg.Auth.DevMode.AllowRemote = tc.allow
			cfg.MCP.Address = tc.mcp
			cfg.API.Address = tc.api
			cfg.LLMProxy.Enabled = false
			err := cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "allow_remote") {
				t.Errorf("error should name auth.dev_mode.allow_remote: %v", err)
			}
		})
	}

	// Dev mode off: any address is fine.
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}
}
