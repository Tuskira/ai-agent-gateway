// Package httpplane wraps an http.Server as a supervisor.Routine, shared by
// the MCP, API, and LLM proxy planes.
package httpplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/supervisor"
)

type plane struct {
	name   string
	addr   string
	srv    *http.Server
	logger *slog.Logger
}

// New returns a supervisor.Routine named name that serves h on addr. Run
// listens and serves; Stop gracefully shuts the server down. Pass write=0
// for planes that stream long-lived responses (e.g. the LLM proxy).
func New(name, addr string, h http.Handler, readTimeout, writeTimeout, idleTimeout time.Duration, logger *slog.Logger) supervisor.Routine {
	return &plane{
		name:   name,
		addr:   addr,
		logger: logger,
		srv: &http.Server{
			Addr:         addr,
			Handler:      h,
			ReadTimeout:  readTimeout,
			WriteTimeout: writeTimeout,
			IdleTimeout:  idleTimeout,
		},
	}
}

func (p *plane) Name() string { return p.name }

func (p *plane) Init(_ context.Context) error { return nil }

// Run listens on addr and serves until Stop is called (or the listener
// fails). It does not return on context cancellation alone: callers must
// call Stop to trigger a graceful shutdown.
func (p *plane) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", p.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", p.addr, err)
	}

	p.logger.Info("plane listening", "plane", p.name, "addr", p.addr)
	if err := p.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serve %s: %w", p.name, err)
	}
	return nil
}

func (p *plane) Stop(ctx context.Context) error {
	if err := p.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown %s: %w", p.name, err)
	}
	return nil
}

// HealthHandler serves a liveness/version probe at /health and basic
// service info at /.
func HealthHandler(name, version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{
			"status":  "ok",
			"plane":   name,
			"version": version,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{
			"service": "tusk-ai-secured-gateway",
			"plane":   name,
			"version": version,
		})
	})
	return mux
}

// GatedHandler serves the same open liveness probe as HealthHandler at
// /health, but requires mw (typically internal/auth.Middleware) for every
// other path, falling through to a 404 once authenticated. It's meant for
// planes that don't have a real handler yet (MCP, LLM proxy today): even
// before those planes grow real routes, an unauthenticated request to any
// non-health path is rejected with 401 rather than reaching an
// unauthenticated 404.
func GatedHandler(name, version string, mw func(http.Handler) http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{
			"status":  "ok",
			"plane":   name,
			"version": version,
		})
	})
	mux.Handle("/", mw(http.NotFoundHandler()))
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
