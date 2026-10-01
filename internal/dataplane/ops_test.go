package dataplane_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	// Registers the "memory" session driver dataplane.New opens by default.
	_ "github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/memory"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const opsTenantID = "tenant-ops"

// nopAuth is an Authenticator that is never exercised by these tests
// (they call Plane.ConnectorOps/CacheOps directly, not the HTTP handler)
// but is required by dataplane.New.
type nopAuth struct{}

func (nopAuth) Name() string { return "nop" }
func (nopAuth) Authenticate(context.Context, *http.Request) (*pkgauth.Principal, error) {
	return nil, pkgauth.ErrNoCredential
}

// newOpsFixture builds a real MCP plane (dataplane.New) over an in-memory
// dptest.Store and a real HTTP backend (dptest.Backend), the same
// collaborators internal/dataplane/transport's own tests use. It proves
// Plane.ConnectorOps/CacheOps -- the pkg/ops adapter -- work against the
// same wiring a production plane gets, not a second, hand-rolled fake.
func newOpsFixture(t *testing.T) (plane *dataplane.Plane, st *dptest.Store, backend *dptest.Backend, conn *store.Connector) {
	t.Helper()
	ctx := context.Background()

	backend = dptest.NewBackend(dptest.BackendOptions{
		Tools: []mcp.Tool{
			{Name: "echo", Description: "Echo a message", InputSchema: dptest.ObjectSchema([]string{"message"}, "message")},
			{Name: "ping", Description: "Ping the backend", InputSchema: dptest.ObjectSchema(nil)},
		},
		RequireSession: true,
	})
	t.Cleanup(backend.Close)

	st = dptest.New()
	tenant := &store.Tenant{ID: opsTenantID, Slug: "ops", Name: "Ops"}
	if err := st.Tenants().Create(ctx, tenant); err != nil {
		t.Fatal(err)
	}

	conn = &store.Connector{
		TenantID: opsTenantID, Name: "alpha", Slug: "alpha",
		Endpoint: backend.URL, TimeoutMS: 3000, Status: "unknown", Metadata: map[string]any{},
	}
	if err := st.Connectors().Create(ctx, conn); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Service.Version = "test"

	p, err := dataplane.New(dataplane.Deps{
		Config:        cfg,
		Store:         st,
		Headers:       headers.NewRegistry(),
		Authenticator: nopAuth{},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })

	return p, st, backend, conn
}

func TestOpsAdapter_Probe_FlipsStatusHealthy(t *testing.T) {
	plane, st, _, conn := newOpsFixture(t)
	ctx := context.Background()

	result, err := plane.ConnectorOps.Probe(ctx, opsTenantID, conn.ID)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Status != "healthy" {
		t.Fatalf("Status = %q, want healthy (error=%q)", result.Status, result.Error)
	}
	if result.Capabilities["tools"] != true {
		t.Errorf("Capabilities = %+v, want tools=true", result.Capabilities)
	}
	if result.Capabilities["protocol_version"] == "" || result.Capabilities["protocol_version"] == nil {
		t.Errorf("Capabilities[protocol_version] missing: %+v", result.Capabilities)
	}
	si, ok := result.Capabilities["server_info"].(map[string]any)
	if !ok || si["name"] == "" {
		t.Errorf("Capabilities[server_info] missing/empty: %+v", result.Capabilities)
	}
	if result.CheckedAt.IsZero() {
		t.Error("CheckedAt is zero")
	}

	updated, err := st.Connectors().Get(ctx, opsTenantID, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "healthy" {
		t.Errorf("persisted status = %q, want healthy", updated.Status)
	}
	// The probe's capabilities (including protocol_version/server_info)
	// are persisted onto the connector row, not just returned in the
	// ProbeResult -- GET /connectors surfaces them from here.
	upCaps, _ := updated.Capabilities["server_info"].(map[string]any)
	if updated.Capabilities["tools"] != true || upCaps["name"] == "" {
		t.Errorf("persisted Capabilities = %+v, want tools=true and a non-empty server_info.name", updated.Capabilities)
	}
}

func TestOpsAdapter_Probe_UnreachableConnectorMarksUnhealthy(t *testing.T) {
	plane, st, _, conn := newOpsFixture(t)
	ctx := context.Background()

	// Point the connector at a port nobody is listening on, with a short
	// timeout, so the handshake fails fast rather than hanging.
	c, err := st.Connectors().Get(ctx, opsTenantID, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.Endpoint = "http://127.0.0.1:1"
	c.TimeoutMS = 500
	if err := st.Connectors().Update(ctx, c); err != nil {
		t.Fatal(err)
	}

	result, err := plane.ConnectorOps.Probe(ctx, opsTenantID, conn.ID)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Status != "unhealthy" {
		t.Fatalf("Status = %q, want unhealthy", result.Status)
	}
	if result.Error == "" {
		t.Error("Error is empty, want a failure reason")
	}

	updated, err := st.Connectors().Get(ctx, opsTenantID, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "unhealthy" {
		t.Errorf("persisted status = %q, want unhealthy", updated.Status)
	}
}

func TestOpsAdapter_Discover_PopulatesCache(t *testing.T) {
	plane, st, _, conn := newOpsFixture(t)
	ctx := context.Background()

	tools, err := plane.ConnectorOps.Discover(ctx, opsTenantID, conn.ID)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2: %+v", len(tools), tools)
	}

	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.ToolName] = true
		if tl.ConnectorID != conn.ID {
			t.Errorf("tool %q ConnectorID = %q, want %q", tl.ToolName, tl.ConnectorID, conn.ID)
		}
	}
	if !names["echo"] || !names["ping"] {
		t.Errorf("tool names = %+v, want echo and ping", names)
	}

	// The cache row is independently readable through the store, exactly
	// as GET /connectors/{id}/tools reads it in production.
	cached, err := st.ToolCache().ListByConnector(ctx, opsTenantID, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) != 2 {
		t.Fatalf("cached rows = %d, want 2", len(cached))
	}
}

func TestOpsAdapter_CacheOps_RefreshStatsSearchInvalidate(t *testing.T) {
	plane, st, _, conn := newOpsFixture(t)
	ctx := context.Background()

	n, err := plane.CacheOps.Refresh(ctx, opsTenantID, "")
	if err != nil {
		t.Fatalf("Refresh(all): %v", err)
	}
	if n != 2 {
		t.Fatalf("Refresh(all) = %d, want 2", n)
	}

	stats, err := plane.CacheOps.Stats(ctx, opsTenantID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalTools != 2 || len(stats.Connectors) != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Connectors[0].ConnectorID != conn.ID || stats.Connectors[0].Tools != 2 {
		t.Errorf("connector stats = %+v", stats.Connectors[0])
	}

	results, err := plane.CacheOps.Search(ctx, opsTenantID, "", 10, false)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("search results = %d, want 2", len(results))
	}

	if err := plane.CacheOps.Invalidate(ctx, opsTenantID, ""); err != nil {
		t.Fatalf("Invalidate(all): %v", err)
	}
	rows, err := st.ToolCache().ListByTenant(ctx, opsTenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows after invalidate = %d, want 0", len(rows))
	}

	// A connector-scoped refresh still works after a full invalidate.
	n2, err := plane.CacheOps.Refresh(ctx, opsTenantID, conn.ID)
	if err != nil {
		t.Fatalf("Refresh(connector): %v", err)
	}
	if n2 != 2 {
		t.Fatalf("Refresh(connector) = %d, want 2", n2)
	}

	if err := plane.CacheOps.Invalidate(ctx, opsTenantID, conn.ID); err != nil {
		t.Fatalf("Invalidate(connector): %v", err)
	}
	rows, err = st.ToolCache().ListByTenant(ctx, opsTenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows after connector invalidate = %d, want 0", len(rows))
	}
}

func TestOpsAdapter_UnknownConnectorIsNotFound(t *testing.T) {
	plane, _, _, _ := newOpsFixture(t)
	ctx := context.Background()

	if _, err := plane.ConnectorOps.Probe(ctx, opsTenantID, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Probe error = %v, want wrapping store.ErrNotFound", err)
	}
	if _, err := plane.ConnectorOps.Discover(ctx, opsTenantID, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Discover error = %v, want wrapping store.ErrNotFound", err)
	}
	if _, err := plane.CacheOps.Refresh(ctx, opsTenantID, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Refresh error = %v, want wrapping store.ErrNotFound", err)
	}
	if err := plane.CacheOps.Invalidate(ctx, opsTenantID, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Invalidate error = %v, want wrapping store.ErrNotFound", err)
	}
}
