package handlers

import (
	"context"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/ops"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// fakeConnectorOps is a scripted ops.ConnectorOps for handler tests. It
// enforces the same tenant-scoping contract the real
// internal/dataplane adapter does -- a connector lookup through the
// tenant-scoped store, so a wrong-tenant or unknown id surfaces as
// store.ErrNotFound -- while letting a test script the probe/discover
// outcome for a connector that does resolve.
type fakeConnectorOps struct {
	store store.ConnectorStore

	probeResult ops.ProbeResult
	probeErr    error // an infra failure once the connector resolves

	discoverTools []store.CachedTool
	discoverErr   error
}

func (f *fakeConnectorOps) Probe(ctx context.Context, tenantID, connectorID string) (ops.ProbeResult, error) {
	if _, err := f.store.Get(ctx, tenantID, connectorID); err != nil {
		return ops.ProbeResult{}, fmt.Errorf("fake connector ops: probe: %w", err)
	}
	if f.probeErr != nil {
		return ops.ProbeResult{}, f.probeErr
	}
	return f.probeResult, nil
}

func (f *fakeConnectorOps) Discover(ctx context.Context, tenantID, connectorID string) ([]store.CachedTool, error) {
	if _, err := f.store.Get(ctx, tenantID, connectorID); err != nil {
		return nil, fmt.Errorf("fake connector ops: discover: %w", err)
	}
	if f.discoverErr != nil {
		return nil, f.discoverErr
	}
	return f.discoverTools, nil
}

var _ ops.ConnectorOps = (*fakeConnectorOps)(nil)

// fakeCacheOps is a scripted ops.CacheOps for handler tests. Refresh and
// Invalidate look the connector up through the tenant-scoped store when
// given a non-empty connectorID, mirroring the real adapter's 404
// contract for the single-connector routes.
type fakeCacheOps struct {
	store store.ConnectorStore

	stats    ops.CacheStats
	statsErr error

	searchTools []store.CachedTool
	searchErr   error
	// lastSearchQuery/lastSearchLimit/lastSearchIncludeStale record the
	// most recent Search call's arguments, so a test can assert the
	// handler parsed ?q=/?limit=/?include_stale= correctly.
	lastSearchQuery        string
	lastSearchLimit        int
	lastSearchIncludeStale bool

	refreshed  int
	refreshErr error
	// lastRefreshConnectorID records the most recent Refresh call's
	// connectorID ("" means "every connector").
	lastRefreshConnectorID string

	invalidateErr error
	// lastInvalidateConnectorID records the most recent Invalidate call's
	// connectorID ("" means "every connector").
	lastInvalidateConnectorID string
}

func (f *fakeCacheOps) Stats(ctx context.Context, tenantID string) (ops.CacheStats, error) {
	return f.stats, f.statsErr
}

func (f *fakeCacheOps) Search(ctx context.Context, tenantID, q string, limit int, includeStale bool) ([]store.CachedTool, error) {
	f.lastSearchQuery, f.lastSearchLimit, f.lastSearchIncludeStale = q, limit, includeStale
	return f.searchTools, f.searchErr
}

func (f *fakeCacheOps) Refresh(ctx context.Context, tenantID, connectorID string) (int, error) {
	f.lastRefreshConnectorID = connectorID
	if connectorID != "" {
		if _, err := f.store.Get(ctx, tenantID, connectorID); err != nil {
			return 0, fmt.Errorf("fake cache ops: refresh: %w", err)
		}
	}
	return f.refreshed, f.refreshErr
}

func (f *fakeCacheOps) Invalidate(ctx context.Context, tenantID, connectorID string) error {
	f.lastInvalidateConnectorID = connectorID
	if connectorID != "" {
		if _, err := f.store.Get(ctx, tenantID, connectorID); err != nil {
			return fmt.Errorf("fake cache ops: invalidate: %w", err)
		}
	}
	return f.invalidateErr
}

var _ ops.CacheOps = (*fakeCacheOps)(nil)

// fakeProfileOps is a scripted ops.ProfileOps for handler tests.
// InvalidateProfile looks the profile up through the tenant-scoped store,
// mirroring the real internal/dataplane adapter's 404 contract.
type fakeProfileOps struct {
	store store.AgentProfileStore

	invalidateErr error
	// lastInvalidateProfileID records the most recent InvalidateProfile
	// call's profileID.
	lastInvalidateProfileID string
	invalidateCalls         int

	invalidateTenantErr   error
	invalidateTenantCalls int
	// lastInvalidateTenantID records the most recent
	// InvalidateTenantProfiles call's tenantID.
	lastInvalidateTenantID string
}

func (f *fakeProfileOps) InvalidateProfile(ctx context.Context, tenantID, profileID string) error {
	f.invalidateCalls++
	f.lastInvalidateProfileID = profileID
	if _, err := f.store.Get(ctx, tenantID, profileID); err != nil {
		return fmt.Errorf("fake profile ops: invalidate: %w", err)
	}
	return f.invalidateErr
}

func (f *fakeProfileOps) InvalidateTenantProfiles(_ context.Context, tenantID string) error {
	f.invalidateTenantCalls++
	f.lastInvalidateTenantID = tenantID
	return f.invalidateTenantErr
}

var _ ops.ProfileOps = (*fakeProfileOps)(nil)
