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
