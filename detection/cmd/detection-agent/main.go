// Command detection-agent is the sidecar the gateway's detection tee posts each
// completed call to: it redacts on the customer's host and sends the turn to
// the detection engine to be judged and recorded.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/internal/agent"
	"github.com/Tuskira/tusk-ai-secured-gateway/detection/turn"
)

// shutdownTimeout bounds the drain on SIGTERM: open turns first, then the
// queued and running background judgments (each bounded at 30 s).
const shutdownTimeout = 30 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("detection-agent exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, listen, level, err := config()
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	turn.EnableScanCache() // the agent resends history every call; the engine never caches
	turn.SetScanCacheBytes(int64(cfg.ScanCacheBytes))
	a := agent.New(cfg)
	srv := &http.Server{
		Addr:              listen,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("detection-agent listening", "addr", listen, "engine", cfg.EngineURL)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("detection-agent shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Warn("detection-agent: queued turns abandoned at shutdown", "queued", a.Queued())
		return err
	}
	done := make(chan struct{})
	go func() { a.Wait(); close(done) }()
	select {
	case <-done:
	case <-sctx.Done():
		slog.Warn("detection-agent: background judgments still running at shutdown", "queued", a.Queued())
	}
	return nil
}

func config() (cfg agent.Config, listen string, level slog.Level, err error) {
	cfg = agent.Config{EngineURL: os.Getenv("DETECTION_ENGINE_URL"), Token: os.Getenv("DETECTION_AGENT_TOKEN")}
	if cfg.EngineURL == "" || cfg.Token == "" {
		return cfg, "", level, errors.New("DETECTION_ENGINE_URL and DETECTION_AGENT_TOKEN are required")
	}
	// AGENT_LOG_LEVEL: debug, info (the default), warn or error. debug adds
	// one line per judged turn (ids, stages, counts, bytes, prepare time;
	// never its text).
	if v := os.Getenv("AGENT_LOG_LEVEL"); v != "" {
		if err := level.UnmarshalText([]byte(v)); err != nil {
			return cfg, "", level, fmt.Errorf("AGENT_LOG_LEVEL: want debug, info, warn or error, got %q", v)
		}
	}
	for name, n := range map[string]*int{
		"AGENT_MAX_IN_FLIGHT":    &cfg.MaxInFlight,
		"AGENT_QUEUE_SIZE":       &cfg.QueueSize,
		"AGENT_QUEUE_BYTES":      &cfg.QueueBytes,
		"AGENT_SCAN_CACHE_BYTES": &cfg.ScanCacheBytes,
	} {
		if v := os.Getenv(name); v != "" {
			if *n, err = strconv.Atoi(v); err != nil || *n <= 0 {
				return cfg, "", level, fmt.Errorf("%s: want a positive integer, got %q", name, v)
			}
		}
	}
	for name, d := range map[string]*time.Duration{
		"AGENT_ENGINE_TIMEOUT": &cfg.EngineTimeout,
		"AGENT_POLICY_TTL":     &cfg.PolicyTTL,
	} {
		if v := os.Getenv(name); v != "" {
			if *d, err = time.ParseDuration(v); err != nil || *d <= 0 {
				return cfg, "", level, fmt.Errorf("%s: want a positive duration, got %q", name, v)
			}
		}
	}
	listen = os.Getenv("AGENT_LISTEN")
	if listen == "" {
		listen = "127.0.0.1:8090"
	}
	return cfg, listen, level, nil
}
