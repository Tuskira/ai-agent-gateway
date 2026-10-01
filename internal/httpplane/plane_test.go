package httpplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthHandler(t *testing.T) {
	h := HealthHandler("mcp", "1.0.0")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" || body["plane"] != "mcp" || body["version"] != "1.0.0" {
		t.Errorf("body = %+v", body)
	}
}

func TestGatedHandler(t *testing.T) {
	// A no-op middleware that never blocks, to isolate GatedHandler's own
	// routing (health open, everything else -> mw -> 404) from any real
	// auth logic.
	passthrough := func(next http.Handler) http.Handler { return next }
	h := GatedHandler("mcp", "1.0.0", passthrough)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/health status = %d, want 200", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/mcp status = %d, want 404 (passthrough mw, no real handler yet)", rec.Code)
	}
}

func TestGatedHandler_MiddlewareCanBlock(t *testing.T) {
	blocking := func(_ http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
	}
	h := GatedHandler("llm", "1.0.0", blocking)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestPlane_RunServesAndStopShutsDown(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Port 0 binds an ephemeral loopback port so concurrent test runs never collide.
	p := New("test", "127.0.0.1:0", HealthHandler("test", "dev"), time.Second, time.Second, time.Second, logger)

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run(context.Background()) }()

	// Give Run a moment to bind the listener before stopping.
	time.Sleep(50 * time.Millisecond)

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after Stop()")
	}
}
