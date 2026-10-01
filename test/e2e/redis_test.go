//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/stdout"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// startRedisPlane builds an MCP plane with sessions.store: redis the way
// cmd/gateway does -- dataplane.New from config, the plane's Routines
// run alongside its listener -- and serves it on a free port. Two calls
// against one store and one Redis are two replicas behind one Service.
func startRedisPlane(t *testing.T, st store.Store, secretSvc *secrets.Service, redisAddr string) (string, *dataplane.Plane) {
	t.Helper()

	cfg := config.Default()
	cfg.Service.Version = "e2e"
	cfg.ToolCache.Enabled = false
	cfg.Sessions.Store = config.SessionStoreRedis
	cfg.Redis.Enabled = true
	cfg.Redis.Addr = redisAddr
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	logSink := stdout.New(io.Discard)
	t.Cleanup(func() { _ = logSink.Close() })

	plane, err := dataplane.New(dataplane.Deps{
		Config:        cfg,
		Store:         st,
		Headers:       newHeaderRegistry(t, secretSvc),
		Authenticator: apikey.New(st.APIKeys(), apikey.Options{}),
		Sink:          logSink,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("build mcp plane: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	// The routines, as supervisor.Run would drive them: every Init
	// first, then Run, then Stop.
	ctx, cancel := context.WithCancel(context.Background())
	for _, r := range plane.Routines {
		if err := r.Init(ctx); err != nil {
			cancel()
			t.Fatalf("init %s: %v", r.Name(), err)
		}
	}
	for _, r := range plane.Routines {
		go func() { _ = r.Run(ctx) }()
	}
	t.Cleanup(func() {
		for _, r := range plane.Routines {
			_ = r.Stop(context.Background())
		}
		cancel()
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: plane.Handler, ReadTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})

	return ln.Addr().String(), plane
}

// stream opens GET /mcp/stream on h and returns a channel that yields
// every `data:` frame the server sends (keep-alive comments are not
// data frames and are dropped), closing when the stream ends.
func (h *harness) stream(t *testing.T, ctx context.Context) <-chan string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+"/mcp/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /mcp/stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET /mcp/stream = %d", resp.StatusCode)
	}
	frames := make(chan string, 16)
	go func() {
		defer close(frames)
		defer resp.Body.Close()
		lines := bufio.NewScanner(resp.Body)
		for lines.Scan() {
			if line := lines.Text(); strings.HasPrefix(line, "data:") {
				frames <- line
			}
		}
	}()
	return frames
}

// settleStream proves the stream's Hub subscription is live: the handler
// subscribes just after sending the headers, and a notification is an
// edge, not a queue, so a signal raised before that is lost. A different
// tenant would not do here (the Hub is per tenant), so the probe is a
// real tools/list_changed for this tenant; the caller's assertions start
// after the probe frame has been consumed.
func settleStream(t *testing.T, notify func(), frames <-chan string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		notify()
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("stream ended before it was settled")
			}
			if !strings.Contains(frame, mcp.NotificationToolsListChanged) {
				t.Fatalf("stream frame = %q, want a tools/list_changed notification", frame)
			}
			// Drain anything the probing loop produced beyond the
			// first frame (a signal raised while the previous one was
			// unread coalesces, so at most one more can be pending).
			select {
			case <-frames:
			case <-time.After(300 * time.Millisecond):
			}
			return
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("stream never carried a probe notification")
		}
	}
}

// expectExactlyOne asserts that frames yields one tools/list_changed
// notification and then nothing more within quiet.
func expectExactlyOne(t *testing.T, name string, frames <-chan string, quiet time.Duration) {
	t.Helper()
	select {
	case frame, ok := <-frames:
		if !ok {
			t.Fatalf("%s: stream ended without a notification", name)
		}
		if !strings.Contains(frame, mcp.NotificationToolsListChanged) {
			t.Fatalf("%s: frame = %q, want a tools/list_changed notification", name, frame)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no notification arrived", name)
	}
	select {
	case frame := <-frames:
		t.Fatalf("%s: a second frame arrived (%q); the notification fired twice", name, frame)
	case <-time.After(quiet):
	}
}

// TestMCPRedisSessionsAcrossReplicas is the reason sessions.store: redis
// exists: a session minted on one replica is served by another, backend
// handle included, and a tools/list_changed raised on one reaches an SSE
// stream held open on the other -- exactly once on each replica.
func TestMCPRedisSessionsAcrossReplicas(t *testing.T) {
	redisAddr := os.Getenv("GATEWAY_TEST_REDIS_ADDR")
	if redisAddr == "" {
		t.Skip("GATEWAY_TEST_REDIS_ADDR not set; skipping the Redis session end-to-end test")
	}

	h, secretSvc := newFixture(t)
	addrA, planeA := startRedisPlane(t, h.store, secretSvc, redisAddr)
	addrB, _ := startRedisPlane(t, h.store, secretSvc, redisAddr)
	replicaA := *h
	replicaA.baseURL = "http://" + addrA
	replicaB := *h
	replicaB.baseURL = "http://" + addrB

	var sessionID string

	t.Run("initialize on replica A issues a session", func(t *testing.T) {
		resp, decoded, raw := replicaA.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
			Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}`),
		}, rpcOptions{})
		t.Logf("A initialize -> %d %s", resp.StatusCode, raw)
		if decoded.Error != nil {
			t.Fatalf("initialize failed: %+v", decoded.Error)
		}
		sessionID = resp.Header.Get(mcp.HeaderSessionID)
		if sessionID == "" {
			t.Fatal("no Mcp-Session-Id header on the initialize response")
		}
		if n := h.backend.Calls(mcp.MethodInitialize); n != 1 {
			t.Fatalf("backend saw %d initialize calls, want 1", n)
		}
	})
	if sessionID == "" {
		t.FailNow()
	}

	t.Run("tools/list on replica B serves A's session", func(t *testing.T) {
		_, decoded, raw := replicaB.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsList},
			rpcOptions{SessionID: sessionID, ProfileName: "Reader"})
		t.Logf("B tools/list -> %s", raw)
		if decoded.Error != nil {
			t.Fatalf("tools/list on replica B failed: %+v", decoded.Error)
		}
		var result mcp.ToolsListResult
		if err := json.Unmarshal(decoded.Result, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Tools) != 1 || result.Tools[0].Name != "alpha__echo" {
			t.Fatalf("tools = %+v, want exactly alpha__echo", result.Tools)
		}
	})

	t.Run("tools/call on replica B reuses A's backend session", func(t *testing.T) {
		_, decoded, raw := replicaB.rpc(t, mcp.Request{
			JSONRPC: mcp.Version, ID: 3, Method: mcp.MethodToolsCall,
			Params: json.RawMessage(`{"name":"alpha__echo","arguments":{"message":"from B"}}`),
		}, rpcOptions{SessionID: sessionID, ProfileName: "Reader"})
		t.Logf("B tools/call -> %s", raw)
		if decoded.Error != nil {
			t.Fatalf("tools/call on replica B failed: %+v", decoded.Error)
		}
		var result mcp.ToolsCallResult
		if err := json.Unmarshal(decoded.Result, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Content) != 1 || result.Content[0].Text != "echo: from B" {
			t.Fatalf("content = %+v", result.Content)
		}
		// The backend requires its session; B presented the one A
		// negotiated, read back out of Redis, without a handshake of
		// its own.
		if got := h.backend.LastHeaders().Get(mcp.HeaderSessionID); got != h.backend.SessionID() {
			t.Errorf("backend saw mcp-session-id = %q, want %q", got, h.backend.SessionID())
		}
		if n := h.backend.Calls(mcp.MethodInitialize); n != 1 {
			t.Errorf("backend saw %d initialize calls, want 1 (replica B re-handshook)", n)
		}
	})

	t.Run("tools/list_changed raised on A reaches streams on A and B exactly once", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		onB := replicaB.stream(t, ctx)
		onA := replicaA.stream(t, ctx)
		notify := func() { planeA.NotifyToolsListChanged(h.tenant.ID) }
		// One probe loop settles both: each raise reaches A's Hub
		// directly and B's over Redis, so once B has a frame, A has one
		// too (drained below).
		settleStream(t, notify, onB)
		select {
		case <-onA:
		case <-time.After(5 * time.Second):
			t.Fatal("A's own stream carried no probe frame")
		}
		for drained := false; !drained; {
			select {
			case <-onA:
			case <-time.After(300 * time.Millisecond):
				drained = true
			}
		}

		notify()
		expectExactlyOne(t, "replica B (over Redis)", onB, 700*time.Millisecond)
		expectExactlyOne(t, "replica A (local hub, own echo skipped)", onA, 700*time.Millisecond)
	})

	t.Run("DELETE on replica B ends the session for replica A", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodDelete, replicaB.baseURL+"/mcp", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+h.adminKey)
		req.Header.Set(mcp.HeaderSessionID, sessionID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		_, decoded, raw := replicaA.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 4, Method: mcp.MethodToolsList},
			rpcOptions{SessionID: sessionID})
		t.Logf("A tools/list after delete -> %s", raw)
		if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeSessionNotFound {
			t.Fatalf("error = %+v, want session not found", decoded.Error)
		}
	})
}
