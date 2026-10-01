//go:build e2e

package e2e

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/supervisor"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/stdout"
)

// TestAPIOpsRoutesWithoutMCPPlane drives the control-plane API's connector
// health and discover routes on an instance wired the way a per-plane
// Kubernetes deployment wires gateway-api: api.enabled true, mcp.enabled
// false (deploy/k8s/base/deployment-api.yaml sets GATEWAY_MCP_ENABLED=false).
// Those two routes -- and the console's Health/Discover buttons -- answered
// 503 there for as long as cmd/gateway built the data plane, the thing
// ConnectorOps/CacheOps come from, only under mcp.enabled.
//
// The wiring below mirrors cmd/gateway/main.go's
// `if cfg.MCP.Enabled || cfg.API.Enabled` block (main.go ~lines 217-256)
// rather than calling it: run() is package main's, not importable from
// here. Keep the two in step.
func TestAPIOpsRoutesWithoutMCPPlane(t *testing.T) {
	h, secretSvc := newFixture(t)
	ctx := context.Background()

	cfg := config.Default()
	cfg.Service.Version = "e2e"
	cfg.MCP.Enabled = false
	cfg.API.Enabled = true

	logSink := stdout.New(io.Discard)
	t.Cleanup(func() { _ = logSink.Close() })

	// Built although mcp.enabled is false, because api.enabled is true and
	// the control plane's ops routes have nowhere else to come from.
	plane, err := dataplane.New(dataplane.Deps{
		Config:        cfg,
		Store:         h.store,
		Headers:       newHeaderRegistry(t, secretSvc),
		Authenticator: apikey.New(h.store.APIKeys(), apikey.Options{}),
		Authorizer:    pkgauth.NewRoleAuthorizer(),
		Sink:          logSink,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("build mcp plane: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	// main.go's inner `if cfg.MCP.Enabled` block, mirrored: on this config
	// it contributes nothing, so the :8080 listener is never registered and
	// none of the plane's background loops (session cleanup, tool-cache
	// refresh and cleanup) run here -- the mcp replicas own those. Nothing
	// else in dataplane.New starts a goroutine or a ticker, so an unstarted
	// Plane is inert.
	var mcpRoutines []supervisor.Routine
	if cfg.MCP.Enabled {
		mcpRoutines = append(mcpRoutines, plane.Routines...)
	}
	if len(mcpRoutines) != 0 {
		t.Fatalf("api-only instance would start %d MCP routines, want 0", len(mcpRoutines))
	}
	if len(plane.Routines) == 0 {
		t.Fatal("plane.Routines is empty, so the assertion above would hold for the wrong reason")
	}

	// Only the control plane is served: plane.Handler is never mounted, and
	// h.baseURL stays empty because there is no MCP plane to talk to.
	h.apiBaseURL = "http://" + startAPIPlane(t, h.store, plane)

	t.Run("health probes the connector and persists status", func(t *testing.T) {
		var got struct {
			Status    string `json:"status"`
			LatencyMS int64  `json:"latency_ms"`
			CheckedAt string `json:"checked_at"`
		}
		resp := h.apiRequest(t, http.MethodGet, "/api/v1/connectors/"+h.connector.ID+"/health", &got)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (503 is the bug this test pins)", resp.StatusCode)
		}
		if got.Status != "healthy" {
			t.Fatalf("health status = %q, want healthy", got.Status)
		}
		if got.CheckedAt == "" {
			t.Error("checked_at is empty")
		}

		conn, err := h.store.Connectors().Get(ctx, h.tenant.ID, h.connector.ID)
		if err != nil {
			t.Fatal(err)
		}
		if conn.Status != "healthy" {
			t.Errorf("persisted connector status = %q, want healthy", conn.Status)
		}
	})

	t.Run("discover reaches the backend and writes the cache", func(t *testing.T) {
		var got struct {
			Items []struct {
				ToolName string `json:"tool_name"`
			} `json:"items"`
			Total int `json:"total"`
		}
		resp := h.apiRequest(t, http.MethodPost, "/api/v1/connectors/"+h.connector.ID+"/discover", &got)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (503 is the bug this test pins)", resp.StatusCode)
		}
		if got.Total < 1 {
			t.Fatalf("total = %d, want at least one discovered tool", got.Total)
		}
	})

	t.Run("the MCP plane is not served on this instance", func(t *testing.T) {
		// The corollary of the routine count above, from the outside: the
		// data plane exists in-process for ops only, so nothing answers on
		// the MCP transport's paths.
		resp := h.apiRequest(t, http.MethodPost, "/mcp", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST /mcp on the api plane = %d, want 404", resp.StatusCode)
		}
	})
}
