package main

import (
	"log/slog"
	"testing"
	"time"
)

// AGENT_LOG_LEVEL sets the log level; unset is info, a bad value fails
// startup.
func TestLogLevel(t *testing.T) {
	t.Setenv("DETECTION_ENGINE_URL", "https://engine.example.com")
	t.Setenv("DETECTION_AGENT_TOKEN", "x")
	for v, want := range map[string]slog.Level{"": slog.LevelInfo, "debug": slog.LevelDebug, "INFO": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		t.Setenv("AGENT_LOG_LEVEL", v)
		if _, _, got, err := config(); err != nil || got != want {
			t.Errorf("%q: level %v, %v; want %v", v, got, err, want)
		}
	}
	t.Setenv("AGENT_LOG_LEVEL", "verbose")
	if _, _, _, err := config(); err == nil {
		t.Error("verbose: no error")
	}
}

// AGENT_SCAN_CACHE_BYTES sets the scan cache's budget; unset is the
// default, a value that is not a positive integer fails startup.
func TestScanCacheBytes(t *testing.T) {
	t.Setenv("DETECTION_ENGINE_URL", "https://engine.example.com")
	t.Setenv("DETECTION_AGENT_TOKEN", "x")
	for v, want := range map[string]int{"": 0, "1048576": 1 << 20} {
		t.Setenv("AGENT_SCAN_CACHE_BYTES", v)
		if cfg, _, _, err := config(); err != nil || cfg.ScanCacheBytes != want {
			t.Errorf("%q: %d, %v; want %d", v, cfg.ScanCacheBytes, err, want)
		}
	}
	for _, v := range []string{"0", "-1", "64MiB"} {
		t.Setenv("AGENT_SCAN_CACHE_BYTES", v)
		if _, _, _, err := config(); err == nil {
			t.Errorf("%q: no error", v)
		}
	}
}

// DETECTION_INBOUND_TOKEN is optional: unset, the agent takes requests
// without a token; set, it is the token they must carry.
func TestInboundToken(t *testing.T) {
	t.Setenv("DETECTION_ENGINE_URL", "https://engine.example.com")
	t.Setenv("DETECTION_AGENT_TOKEN", "x")
	t.Setenv("DETECTION_INBOUND_TOKEN", "")
	if cfg, _, _, err := config(); err != nil || cfg.InboundToken != "" {
		t.Errorf("unset: %q, %v; want empty", cfg.InboundToken, err)
	}
	t.Setenv("DETECTION_INBOUND_TOKEN", "gw-secret")
	if cfg, _, _, err := config(); err != nil || cfg.InboundToken != "gw-secret" || cfg.Token != "x" {
		t.Errorf("set: inbound %q engine %q, %v", cfg.InboundToken, cfg.Token, err)
	}
}

// DETECTION_FORWARD_CLIENT_IP defaults to true (forward); false sets
// OmitClientIP; a value that is not a boolean fails startup.
func TestForwardClientIP(t *testing.T) {
	t.Setenv("DETECTION_ENGINE_URL", "https://engine.example.com")
	t.Setenv("DETECTION_AGENT_TOKEN", "x")
	for v, omit := range map[string]bool{"": false, "true": false, "false": true, "0": true} {
		t.Setenv("DETECTION_FORWARD_CLIENT_IP", v)
		if cfg, _, _, err := config(); err != nil || cfg.OmitClientIP != omit {
			t.Errorf("%q: omit %v, %v; want %v", v, cfg.OmitClientIP, err, omit)
		}
	}
	t.Setenv("DETECTION_FORWARD_CLIENT_IP", "maybe")
	if _, _, _, err := config(); err == nil {
		t.Error("maybe: no error")
	}
}

// DETECTION_TOOL_CHECK_TIMEOUT sets the tool-call check's engine budget;
// unset leaves the default (agent.New's 1500ms); a bad value fails startup.
func TestToolCheckTimeout(t *testing.T) {
	t.Setenv("DETECTION_ENGINE_URL", "https://engine.example.com")
	t.Setenv("DETECTION_AGENT_TOKEN", "x")
	t.Setenv("DETECTION_TOOL_CHECK_TIMEOUT", "")
	if cfg, _, _, err := config(); err != nil || cfg.ToolCheckTimeout != 0 {
		t.Errorf("unset: %s, %v", cfg.ToolCheckTimeout, err)
	}
	t.Setenv("DETECTION_TOOL_CHECK_TIMEOUT", "900ms")
	if cfg, _, _, err := config(); err != nil || cfg.ToolCheckTimeout != 900*time.Millisecond {
		t.Errorf("900ms: %s, %v", cfg.ToolCheckTimeout, err)
	}
	for _, v := range []string{"0s", "soon"} {
		t.Setenv("DETECTION_TOOL_CHECK_TIMEOUT", v)
		if _, _, _, err := config(); err == nil {
			t.Errorf("%q: no error", v)
		}
	}
}
