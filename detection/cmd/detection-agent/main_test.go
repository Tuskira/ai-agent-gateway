package main

import (
	"log/slog"
	"testing"
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
