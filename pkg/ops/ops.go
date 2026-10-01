// Package ops declares the small, plugin-seam interfaces the control-plane
// API depends on for connector health/discovery and tool-cache management.
//
// The API package (internal/api/handlers) never imports the MCP data
// plane's concrete types (orchestrator, router, client, cache) directly --
// it only knows ConnectorOps and CacheOps, satisfied today by
// internal/dataplane's ops.go adapter and, potentially, by a private
// plugin built against this same contract. That is why these live under
// pkg/ rather than internal/: pkg/ is the only part of this module a
// separate module (a private plugin) can import at all.
package ops

import (
	"context"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// ProbeResult is the outcome of one connector health probe (an MCP
// initialize handshake).
type ProbeResult struct {
	// Status is the connector's status after the probe: "healthy" or
	// "unhealthy" (see internal/dataplane/router's Status constants --
	// this package intentionally doesn't import that internal package,
	// so the values are duplicated as plain strings here).
	Status string
	// LatencyMS is how long the handshake took, in milliseconds.
	LatencyMS int64
	// Capabilities is the connector's declared MCP capabilities:
	// "tools"/"resources"/"prompts" -> bool, plus "protocol_version"
	// (string) and "server_info" (map with "name"/"version") taken from
	// the backend's initialize result. Nil when the probe failed.
	Capabilities map[string]any
	// CheckedAt is when the probe ran.
	CheckedAt time.Time
	// Error is the handshake failure reason, empty when Status is
	// "healthy".
	Error string
}

// ConnectorOps performs live MCP operations against one connector on
// behalf of the control-plane API: a health probe and an on-demand tool
// discovery. Both also update the connector's persisted health status.
type ConnectorOps interface {
	// Probe performs an MCP initialize against the connector and updates
	// its status; returns latency and capabilities. A non-nil error means
	// the operation itself could not run (e.g. the connector does not
	// exist in the caller's tenant) -- a reachable-but-unhealthy backend
	// is reported as ProbeResult{Status: "unhealthy", ...}, nil, not as
	// an error.
	Probe(ctx context.Context, tenantID, connectorID string) (ProbeResult, error)
	// Discover performs tools/list against the connector, writes the
	// result to the tool cache, and returns the cached rows.
	Discover(ctx context.Context, tenantID, connectorID string) ([]store.CachedTool, error)
}

// CacheConnectorStats summarizes one connector's cached tools.
type CacheConnectorStats struct {
	ConnectorID string
	Name        string
	Tools       int
	Stale       int
	// CachedAt is the oldest CachedAt among the connector's cached tools.
	CachedAt time.Time
	// ExpiresAt is the newest ExpiresAt among the connector's cached
	// tools.
	ExpiresAt time.Time
}

// CacheStats summarizes a tenant's whole tool cache.
type CacheStats struct {
	Connectors []CacheConnectorStats
	TotalTools int
	StaleTools int
}

// CacheOps manages a tenant's tool cache on behalf of the control-plane
// API.
type CacheOps interface {
	// Stats returns per-connector counts, stale counts, and the
	// oldest/newest cached_at for each connector.
	Stats(ctx context.Context, tenantID string) (CacheStats, error)
	// Search full-text searches the tenant's cached tools. includeStale
	// false (the handler's default) excludes stale rows; true includes
	// them.
	Search(ctx context.Context, tenantID, q string, limit int, includeStale bool) ([]store.CachedTool, error)
	// Refresh re-discovers tools from live connectors and rewrites the
	// cache; connectorID "" means every connector in the tenant. It
	// returns the number of tools written.
	Refresh(ctx context.Context, tenantID string, connectorID string) (int, error)
	// Invalidate drops cached tools; connectorID "" means every
	// connector in the tenant.
	Invalidate(ctx context.Context, tenantID string, connectorID string) error
}

// ProfileOps invalidates the MCP plane's agent-profile enforcer cache
// (internal/dataplane/profile.Enforcer) on behalf of the control-plane
// API, mirroring CacheOps for the tool cache. A profile/skill write that
// doesn't call this still takes effect within the enforcer's own 30s TTL
// -- these calls only make it immediate on the replica that made the
// write; other replicas (a split api/mcp deployment) still wait out the
// TTL, since the enforcer's cache is per-process.
type ProfileOps interface {
	// InvalidateProfile drops one profile's cached resolution (its
	// tools, instructions, and attached skills/commands), looked up by
	// id within tenantID so a wrong-tenant id is a no-op rather than
	// cross-tenant interference. A non-nil error means the profile could
	// not be resolved (e.g. it doesn't exist in this tenant) -- callers
	// treat that as best-effort and log it, the same as CacheOps.Invalidate
	// before a connector delete. Named "...Profile" (not "Invalidate")
	// because the same adapter also implements CacheOps.Invalidate, a
	// different operation the two must not be confused with.
	InvalidateProfile(ctx context.Context, tenantID, profileID string) error
	// InvalidateTenantProfiles drops every cached profile resolution for
	// a tenant. Used after a skill/command registry write (a new
	// version, enable/disable, delete): resolving exactly which profiles
	// reference that skill isn't worth a query, so every profile in the
	// tenant is simply dropped from the cache.
	InvalidateTenantProfiles(ctx context.Context, tenantID string) error
}
