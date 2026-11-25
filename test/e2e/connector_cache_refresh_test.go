//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// With the tool cache on, creating and editing a connector through the API
// shows up in tools/list at once (no waiting for a refresh interval),
// pushes notifications/tools/list_changed to an open stream, and a disabled
// connector's tools stop being listed.
func TestConnectorChangeRefreshesToolCacheEndToEnd(t *testing.T) {
	h, secretSvc := newFixture(t)
	addr, plane := startPlaneConfigured(t, h.store, newHeaderRegistry(t, secretSvc), nil, func(c *config.Config) {
		c.ToolCache.Enabled = true
	})
	h.baseURL = "http://" + addr
	h.apiBaseURL = "http://" + startAPIPlane(t, h.store, plane)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, decoded, raw := h.rpc(t, mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}`),
	}, rpcOptions{})
	if decoded.Error != nil {
		t.Fatalf("initialize: %s", raw)
	}
	sessionID := resp.Header.Get(mcp.HeaderSessionID)
	frames := h.sessionStream(t, ctx, sessionID)

	listed := func() map[string]bool {
		t.Helper()
		_, decoded, raw := h.rpc(t, mcp.Request{JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodToolsList}, rpcOptions{SessionID: sessionID})
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
	waitListed := func(what string, ok func(map[string]bool) bool) map[string]bool {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			got := listed()
			if ok(got) {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: tools/list = %v", what, got)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitNotification := func(what string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case frame := <-frames:
				if strings.Contains(frame, mcp.NotificationToolsListChanged) {
					return
				}
			case <-deadline:
				t.Fatalf("%s: no %s on the stream", what, mcp.NotificationToolsListChanged)
			}
		}
	}

	// Warm the cache: it now holds alpha's tools and nothing else.
	if got := listed(); !got["alpha__echo"] {
		t.Fatalf("warm-up tools/list = %v", got)
	}

	second := dptest.NewBackend(dptest.BackendOptions{
		Tools:          []mcp.Tool{{Name: "lookup", Description: "Look something up", InputSchema: dptest.ObjectSchema(nil)}},
		RequireSession: true,
	})
	t.Cleanup(second.Close)
	third := dptest.NewBackend(dptest.BackendOptions{
		Tools:          []mcp.Tool{{Name: "fetch", Description: "Fetch something", InputSchema: dptest.ObjectSchema(nil)}},
		RequireSession: true,
	})
	t.Cleanup(third.Close)

	// Create: the new connector's tools appear, and the stream is told.
	var created struct {
		ID string `json:"id"`
	}
	if code := h.apiJSON(t, http.MethodPost, "/api/v1/connectors", map[string]any{
		"name": "beta", "endpoint": second.URL,
	}, &created); code != http.StatusCreated {
		t.Fatalf("create connector: %d", code)
	}
	waitNotification("after create")
	waitListed("after create", func(m map[string]bool) bool { return m["beta__lookup"] && m["alpha__echo"] })

	// Edit the endpoint: the old tools go, the new ones come.
	if code := h.apiJSON(t, http.MethodPut, "/api/v1/connectors/"+created.ID, map[string]any{
		"name": "beta", "endpoint": third.URL,
	}, nil); code != http.StatusOK {
		t.Fatalf("update endpoint: %d", code)
	}
	waitNotification("after endpoint edit")
	waitListed("after endpoint edit", func(m map[string]bool) bool { return m["beta__fetch"] && !m["beta__lookup"] })

	// Disable: its tools stop being listed, and the stream is told.
	if code := h.apiJSON(t, http.MethodPut, "/api/v1/connectors/"+created.ID, map[string]any{
		"name": "beta", "endpoint": third.URL, "metadata": map[string]any{"enabled": false},
	}, nil); code != http.StatusOK {
		t.Fatalf("disable: %d", code)
	}
	waitNotification("after disable")
	got := waitListed("after disable", func(m map[string]bool) bool { return !m["beta__fetch"] })
	if !got["alpha__echo"] {
		t.Errorf("disabling beta dropped alpha's tools: %v", got)
	}

	// Enable again: back.
	if code := h.apiJSON(t, http.MethodPut, "/api/v1/connectors/"+created.ID, map[string]any{
		"name": "beta", "endpoint": third.URL, "metadata": map[string]any{},
	}, nil); code != http.StatusOK {
		t.Fatalf("enable: %d", code)
	}
	waitNotification("after enable")
	waitListed("after enable", func(m map[string]bool) bool { return m["beta__fetch"] })
}
