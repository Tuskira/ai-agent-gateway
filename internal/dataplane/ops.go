package dataplane

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/cache"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/router"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/transport"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/ops"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// opsAdapter implements pkg/ops.ConnectorOps and pkg/ops.CacheOps over the
// same collaborators the MCP plane's orchestrator uses: the connector
// store, the router (health state), the backend client, and the tool
// cache.
//
// It builds its own cache.ToolCache rather than reusing the orchestrator's
// (which is nil when cfg.ToolCache.Enabled is false): these control-plane
// routes manage the underlying store.ToolCacheStore table directly --
// GET .../tools already reads it unconditionally (see
// internal/api/handlers.Connectors.Tools) -- independent of whether the
// live tools/list path is currently serving from it.
//
// Every call here is a one-shot control-plane action: no MCP session is
// created or reused, and there is no caller Principal to propagate beyond
// a synthetic tenant-scoped one for header resolution (mirrors
// orchestrator.RefreshTenant, which does the same for the background
// refresh routine).
type opsAdapter struct {
	connectors     store.ConnectorStore
	profiles       store.AgentProfileStore
	toolCacheStore store.ToolCacheStore
	router         *router.Router
	client         *client.Client
	cache          cache.ToolCache
	enforcer       *profile.Enforcer
	// hub pushes a live notifications/tools/list_changed and
	// notifications/prompts/list_changed to any GET /mcp/stream open on
	// this replica for the invalidated profile -- see InvalidateProfile
	// and transport.Hub.NotifyProfileListChanged. Never nil in
	// production (plane.go always builds one); tests that construct an
	// opsAdapter without one get InvalidateProfile's cache invalidation
	// with no live push, which is a documented no-op, not a panic.
	hub *transport.Hub
}

var (
	_ ops.ConnectorOps = (*opsAdapter)(nil)
	_ ops.CacheOps     = (*opsAdapter)(nil)
	_ ops.ProfileOps   = (*opsAdapter)(nil)
)

// newOpsAdapter builds the ops adapter. toolRouter, backend and enforcer
// are the same instances New wires into the orchestrator, so a
// probe/discover/invalidate run through the API and one triggered by a
// live MCP request agree about a connector's health state, cool-down
// window and cached profile resolutions.
func newOpsAdapter(deps Deps, toolRouter *router.Router, backend *client.Client, enforcer *profile.Enforcer, hub *transport.Hub) *opsAdapter {
	ttl := deps.Config.ToolCache.L2TTL
	return &opsAdapter{
		connectors:     deps.Store.Connectors(),
		profiles:       deps.Store.AgentProfiles(),
		toolCacheStore: deps.Store.ToolCache(),
		router:         toolRouter,
		client:         backend,
		cache:          cache.NewL2(deps.Store.ToolCache(), cache.Options{TTL: ttl, ServeStale: true}),
		enforcer:       enforcer,
		hub:            hub,
	}
}

// syntheticPrincipal stands in for a caller identity on these
// admin-triggered backend calls, so static/secret_store connector headers
// and X-Tenant-Id still resolve. A connector header that forwards
// something from an inbound caller (token_field, incoming_field) cannot
// be satisfied here and is skipped by the client's per-header
// degradation, exactly as it is for the background cache refresher.
func syntheticPrincipal(tenantID string) *pkgauth.Principal {
	return &pkgauth.Principal{
		Subject:    "system:api-ops",
		TenantID:   tenantID,
		Roles:      []string{"agent"},
		AuthMethod: "system",
	}
}

// getConnector looks up a tenant-scoped connector so a wrong-tenant or
// unknown id surfaces as store.ErrNotFound -- which the API handlers map
// onto 404 -- rather than a generic failure.
func (a *opsAdapter) getConnector(ctx context.Context, tenantID, connectorID string) (*store.Connector, error) {
	return a.connectors.Get(ctx, tenantID, connectorID)
}

// unqualifyToolName strips the "<connector>__" prefix client.ListTools
// stamps onto every tool name, mirroring orchestrator's private
// unqualify: cache.Entry.ToolName is documented as the backend's own,
// unprefixed name.
func unqualifyToolName(connectorName, qualified string) string {
	prefix := connectorName + "__"
	if len(qualified) > len(prefix) && strings.HasPrefix(qualified, prefix) {
		return qualified[len(prefix):]
	}
	return qualified
}

// discoverEntries handshakes with conn and lists its tools, translating
// them into cache entries. It does not touch health state or the cache --
// callers decide what a failure or success means for those.
func (a *opsAdapter) discoverEntries(ctx context.Context, conn *store.Connector) ([]cache.Entry, error) {
	_, initResult, err := a.client.Initialize(ctx, client.Call{Connector: conn})
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}

	// Not fatal: some servers answer the notification with an error yet
	// serve tools/list perfectly well afterwards (mirrors
	// orchestrator.handshake).
	_ = a.client.SendInitialized(ctx, client.Call{
		Connector:       conn,
		SessionID:       initResult.SessionID,
		ProtocolVersion: initResult.ProtocolVersion,
	})

	tools, _, err := a.client.ListTools(ctx, client.Call{
		Connector:       conn,
		SessionID:       initResult.SessionID,
		ProtocolVersion: initResult.ProtocolVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}

	entries := make([]cache.Entry, 0, len(tools))
	for _, tool := range tools {
		entries = append(entries, cache.Entry{
			ConnectorID:   conn.ID,
			ConnectorName: client.Qualifier(conn),
			ToolName:      unqualifyToolName(client.Qualifier(conn), tool.Name),
			Tool:          tool,
		})
	}
	return entries, nil
}

// writeConnectorCache replaces conn's cached tools. cache.ToolCache.Put
// only clears the connectors represented in entries, so an empty result
// (a connector that now advertises zero tools) has to be cleared
// explicitly rather than through Put.
func (a *opsAdapter) writeConnectorCache(ctx context.Context, tenantID string, conn *store.Connector, entries []cache.Entry) error {
	if len(entries) == 0 {
		return a.toolCacheStore.DeleteByConnector(ctx, conn.ID)
	}
	return a.cache.Put(ctx, tenantID, entries)
}

// Probe implements ops.ConnectorOps.
func (a *opsAdapter) Probe(ctx context.Context, tenantID, connectorID string) (ops.ProbeResult, error) {
	conn, err := a.getConnector(ctx, tenantID, connectorID)
	if err != nil {
		return ops.ProbeResult{}, fmt.Errorf("ops: probe: %w", err)
	}
	ctx = pkgauth.WithPrincipal(ctx, syntheticPrincipal(tenantID))

	start := time.Now()
	res, _, callErr := a.client.Initialize(ctx, client.Call{Connector: conn})
	latency := time.Since(start)
	checkedAt := time.Now().UTC()

	if callErr != nil {
		if err := a.router.MarkUnhealthy(ctx, conn); err != nil {
			return ops.ProbeResult{}, fmt.Errorf("ops: probe: persist unhealthy status: %w", err)
		}
		return ops.ProbeResult{
			Status:    router.StatusUnhealthy,
			LatencyMS: latency.Milliseconds(),
			CheckedAt: checkedAt,
			Error:     callErr.Error(),
		}, nil
	}

	caps := map[string]any{
		"tools":            res.Capabilities.Tools != nil,
		"resources":        res.Capabilities.Resources != nil,
		"prompts":          res.Capabilities.Prompts != nil,
		"protocol_version": res.ProtocolVersion,
		"server_info": map[string]any{
			"name":    res.ServerInfo.Name,
			"version": res.ServerInfo.Version,
		},
	}
	conn.Capabilities = caps
	conn.Status = router.StatusHealthy
	if err := a.connectors.Update(ctx, conn); err != nil {
		return ops.ProbeResult{}, fmt.Errorf("ops: probe: persist healthy status: %w", err)
	}
	// MarkHealthy is a status no-op here (Update above already persisted
	// it), but it also clears the router's probe cool-down window, which
	// a direct Update call does not.
	if err := a.router.MarkHealthy(ctx, conn); err != nil {
		return ops.ProbeResult{}, fmt.Errorf("ops: probe: clear cool-down: %w", err)
	}

	return ops.ProbeResult{
		Status:       router.StatusHealthy,
		LatencyMS:    latency.Milliseconds(),
		Capabilities: caps,
		CheckedAt:    checkedAt,
	}, nil
}

// Discover implements ops.ConnectorOps.
func (a *opsAdapter) Discover(ctx context.Context, tenantID, connectorID string) ([]store.CachedTool, error) {
	conn, err := a.getConnector(ctx, tenantID, connectorID)
	if err != nil {
		return nil, fmt.Errorf("ops: discover: %w", err)
	}
	ctx = pkgauth.WithPrincipal(ctx, syntheticPrincipal(tenantID))

	entries, err := a.discoverEntries(ctx, conn)
	if err != nil {
		_ = a.router.MarkUnhealthy(ctx, conn)
		return nil, fmt.Errorf("ops: discover connector %q: %w", conn.Name, err)
	}
	if err := a.router.MarkHealthy(ctx, conn); err != nil {
		return nil, fmt.Errorf("ops: discover: persist healthy status: %w", err)
	}

	if err := a.writeConnectorCache(ctx, tenantID, conn, entries); err != nil {
		return nil, fmt.Errorf("ops: discover: write cache: %w", err)
	}

	rows, err := a.toolCacheStore.ListByConnector(ctx, tenantID, conn.ID)
	if err != nil {
		return nil, fmt.Errorf("ops: discover: read back cache: %w", err)
	}
	out := make([]store.CachedTool, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row)
	}
	return out, nil
}

// Stats implements ops.CacheOps.
func (a *opsAdapter) Stats(ctx context.Context, tenantID string) (ops.CacheStats, error) {
	rows, err := a.toolCacheStore.ListByTenant(ctx, tenantID)
	if err != nil {
		return ops.CacheStats{}, fmt.Errorf("ops: cache stats: %w", err)
	}

	type acc struct {
		name      string
		tools     int
		stale     int
		cachedAt  time.Time
		expiresAt time.Time
	}
	byConnector := make(map[string]*acc)
	var order []string
	var totalTools, staleTools int

	for _, row := range rows {
		a2, ok := byConnector[row.ConnectorID]
		if !ok {
			a2 = &acc{name: row.ToolNamespace, cachedAt: row.CachedAt, expiresAt: row.ExpiresAt}
			byConnector[row.ConnectorID] = a2
			order = append(order, row.ConnectorID)
		}
		a2.tools++
		if row.Stale {
			a2.stale++
			staleTools++
		}
		if row.CachedAt.Before(a2.cachedAt) {
			a2.cachedAt = row.CachedAt
		}
		if row.ExpiresAt.After(a2.expiresAt) {
			a2.expiresAt = row.ExpiresAt
		}
		totalTools++
	}

	sort.Strings(order)
	out := make([]ops.CacheConnectorStats, 0, len(order))
	for _, id := range order {
		a2 := byConnector[id]
		out = append(out, ops.CacheConnectorStats{
			ConnectorID: id,
			Name:        a2.name,
			Tools:       a2.tools,
			Stale:       a2.stale,
			CachedAt:    a2.cachedAt,
			ExpiresAt:   a2.expiresAt,
		})
	}

	return ops.CacheStats{Connectors: out, TotalTools: totalTools, StaleTools: staleTools}, nil
}

// Search implements ops.CacheOps.
func (a *opsAdapter) Search(ctx context.Context, tenantID, q string, limit int, includeStale bool) ([]store.CachedTool, error) {
	rows, err := a.toolCacheStore.Search(ctx, tenantID, q, limit, includeStale)
	if err != nil {
		return nil, fmt.Errorf("ops: cache search: %w", err)
	}
	out := make([]store.CachedTool, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row)
	}
	return out, nil
}

// Refresh implements ops.CacheOps.
func (a *opsAdapter) Refresh(ctx context.Context, tenantID, connectorID string) (int, error) {
	ctx = pkgauth.WithPrincipal(ctx, syntheticPrincipal(tenantID))

	if connectorID != "" {
		conn, err := a.getConnector(ctx, tenantID, connectorID)
		if err != nil {
			return 0, fmt.Errorf("ops: refresh cache: %w", err)
		}
		entries, err := a.discoverEntries(ctx, conn)
		if err != nil {
			_ = a.router.MarkUnhealthy(ctx, conn)
			return 0, fmt.Errorf("ops: refresh cache: connector %q: %w", conn.Name, err)
		}
		if err := a.writeConnectorCache(ctx, tenantID, conn, entries); err != nil {
			return 0, fmt.Errorf("ops: refresh cache: write connector %q: %w", conn.Name, err)
		}
		_ = a.router.MarkHealthy(ctx, conn)
		return len(entries), nil
	}

	all, err := a.connectors.List(ctx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("ops: refresh cache: list connectors: %w", err)
	}

	// A connector that fails to refresh is logged (by MarkUnhealthy's
	// caller-side persistence, since this adapter has no logger of its
	// own -- callers may wrap this) and skipped rather than failing the
	// whole batch: one broken backend must not block refreshing the rest.
	var total int
	for _, conn := range all {
		entries, err := a.discoverEntries(ctx, conn)
		if err != nil {
			_ = a.router.MarkUnhealthy(ctx, conn)
			continue
		}
		if err := a.writeConnectorCache(ctx, tenantID, conn, entries); err != nil {
			return total, fmt.Errorf("ops: refresh cache: write connector %q: %w", conn.Name, err)
		}
		_ = a.router.MarkHealthy(ctx, conn)
		total += len(entries)
	}
	return total, nil
}

// Invalidate implements ops.CacheOps.
func (a *opsAdapter) Invalidate(ctx context.Context, tenantID, connectorID string) error {
	if connectorID != "" {
		if _, err := a.getConnector(ctx, tenantID, connectorID); err != nil {
			return fmt.Errorf("ops: invalidate cache: %w", err)
		}
		if err := a.toolCacheStore.DeleteByConnector(ctx, connectorID); err != nil {
			return fmt.Errorf("ops: invalidate cache: %w", err)
		}
		return nil
	}

	all, err := a.connectors.List(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("ops: invalidate cache: list connectors: %w", err)
	}
	for _, conn := range all {
		if err := a.toolCacheStore.DeleteByConnector(ctx, conn.ID); err != nil {
			return fmt.Errorf("ops: invalidate cache: connector %q: %w", conn.Name, err)
		}
	}
	return nil
}

// InvalidateProfile implements ops.ProfileOps: it drops profileID's
// cached resolution, using the profile row's own Slug (not whatever name
// a caller's X-Agent-Profile-Name header might use) so this is correct
// even after the profile's display Name has changed. It also pushes a
// live notifications/tools/list_changed and
// notifications/prompts/list_changed to any GET /mcp/stream open on this
// replica for this exact profile (see transport.Hub.
// NotifyProfileListChanged for the reach and its limit) -- a no-op when
// a is built without a hub, or when no stream is currently subscribed.
func (a *opsAdapter) InvalidateProfile(ctx context.Context, tenantID, profileID string) error {
	prof, err := a.profiles.Get(ctx, tenantID, profileID)
	if err != nil {
		return fmt.Errorf("ops: invalidate profile: %w", err)
	}
	a.enforcer.InvalidateSlug(tenantID, prof.Slug)
	if a.hub != nil {
		a.hub.NotifyProfileListChanged(tenantID, prof.Slug)
	}
	return nil
}

// InvalidateTenantProfiles implements ops.ProfileOps.
func (a *opsAdapter) InvalidateTenantProfiles(_ context.Context, tenantID string) error {
	a.enforcer.InvalidateTenant(tenantID)
	return nil
}
