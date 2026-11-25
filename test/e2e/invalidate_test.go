//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/invalidate"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// replica is one more gateway process over the same database: its own MCP
// plane, with its own profile and secret caches.
type replica struct {
	*harness
}

// newReplica starts a second MCP plane (own enforcer, own secret store
// cache) on h's database. With listen, it also runs the cache invalidation
// listener, wired as pkg/gateway wires it.
func newReplica(t *testing.T, h *harness, listen bool) replica {
	t.Helper()
	ring, err := secrets.LoadKeyRing(config.SecretStore{MasterKeyEnv: "GATEWAY_MASTER_KEY", ActiveKeyID: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	svc := secrets.NewService(h.store.Credentials(), ring)
	addr, plane := startPlane(t, h.store, svc)

	rh := *h
	rh.baseURL = "http://" + addr
	if listen {
		n, ok := h.store.(store.InvalidationNotifier)
		if !ok {
			t.Fatal("store has no invalidation notifier")
		}
		ready := make(chan struct{}, 8)
		caches := &invalidate.Caches{Profiles: plane, Secrets: svc}
		target := invalidate.Funcs{
			ApplyFunc: caches.Apply,
			FlushFunc: func() { caches.Flush(); ready <- struct{}{} },
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			invalidate.Listen(ctx, n, target, slog.New(slog.NewTextHandler(io.Discard, nil)))
		}()
		t.Cleanup(func() { cancel(); <-done })
		select {
		case <-ready:
		case <-time.After(10 * time.Second):
			t.Fatal("invalidation listener never became ready")
		}
	}
	return replica{&rh}
}

func (r replica) listTools(t *testing.T, profile string) map[string]bool {
	t.Helper()
	_, decoded, raw := r.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsList}, rpcOptions{ProfileName: profile})
	if decoded.Error != nil {
		t.Fatalf("tools/list: %s", raw)
	}
	var res mcp.ToolsListResult
	if err := json.Unmarshal(decoded.Result, &res); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, tool := range res.Tools {
		out[tool.Name] = true
	}
	return out
}

func (r replica) echo(t *testing.T, profile string) {
	t.Helper()
	_, decoded, raw := r.rpc(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsCall,
		Params: json.RawMessage(`{"name":"alpha__echo","arguments":{"message":"hi"}}`),
	}, rpcOptions{ProfileName: profile})
	if decoded.Error != nil {
		t.Fatalf("tools/call: %s", raw)
	}
}

// A profile grant or credential rotation made through one replica reaches
// the caches of another at once when its invalidation listener runs, and
// not (until the TTL) when it does not.
func TestInvalidationReachesOtherReplicasEndToEnd(t *testing.T) {
	h, secretSvc := newFixture(t)
	addr, planeA := startPlane(t, h.store, secretSvc)
	h.baseURL = "http://" + addr
	h.apiBaseURL = "http://" + startSourcesAPIPlane(t, h.store, planeA, secretSvc, newHeaderRegistry(t, secretSvc))

	b := newReplica(t, h, true)  // listens
	c := newReplica(t, h, false) // control: no listener

	// Warm both replicas' caches: profile Reader grants only echo, and the
	// backend credential resolves to the original key.
	for name, r := range map[string]replica{"listening": b, "control": c} {
		got := r.listTools(t, "Reader")
		if !got["alpha__echo"] || got["alpha__danger"] {
			t.Fatalf("%s replica: tools/list before = %v", name, got)
		}
		r.echo(t, "Reader")
		if k := h.backend.LastHeaders().Get("X-Backend-Key"); k != "s3cret-backend-key" {
			t.Fatalf("%s replica: X-Backend-Key = %q", name, k)
		}
	}

	// Through replica A: grant danger too, and rotate the credential.
	var profiles struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if code := h.apiJSON(t, http.MethodGet, "/api/v1/profiles", nil, &profiles); code != http.StatusOK || len(profiles.Items) != 1 {
		t.Fatalf("list profiles: %d %+v", code, profiles)
	}
	if code := h.apiJSON(t, http.MethodPut, "/api/v1/profiles/"+profiles.Items[0].ID+"/tools", map[string]any{
		"tools": []map[string]any{
			{"connector_id": h.connector.ID, "tool_name": "echo"},
			{"connector_id": h.connector.ID, "tool_name": "danger"},
		},
	}, nil); code != http.StatusOK {
		t.Fatalf("set profile tools: %d", code)
	}
	if code := h.apiJSON(t, http.MethodPut, "/api/v1/credentials/backend-auth", map[string]any{
		"payload": map[string]string{"api_key": "rotated-key"},
	}, nil); code != http.StatusOK {
		t.Fatalf("rotate credential: %d", code)
	}

	// The listening replica converges at once.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b.listTools(t, "Reader")["alpha__danger"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listening replica never saw the new grant")
		}
		time.Sleep(50 * time.Millisecond)
	}
	for {
		b.echo(t, "Reader")
		if h.backend.LastHeaders().Get("X-Backend-Key") == "rotated-key" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("listening replica still sends X-Backend-Key = %q", h.backend.LastHeaders().Get("X-Backend-Key"))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The control replica, which only has TTLs, is still stale: this is what
	// the listener fixes.
	if got := c.listTools(t, "Reader"); got["alpha__danger"] {
		t.Errorf("control replica already sees the new grant (%v): the test does not discriminate", got)
	}
	c.echo(t, "Reader")
	if k := h.backend.LastHeaders().Get("X-Backend-Key"); k != "s3cret-backend-key" {
		t.Errorf("control replica sends X-Backend-Key = %q, want the stale original", k)
	}
}
