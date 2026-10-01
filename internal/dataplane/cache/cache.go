// Package cache holds the gateway's tool-list cache: the layer that
// makes tools/list a single Postgres read instead of a fan-out to every
// backend on every call.
//
// The cache is defined as an interface (ToolCache) over one shipped
// implementation (L2, backed by pkg/store's ToolCacheStore). A Redis L1
// in front of it is the obvious next layer and needs no change here --
// it wraps a ToolCache and is one.
//
// The serving policy is stale-while-revalidate: an expired entry is
// still served, and the refresh happens in the background. A tool list
// that is a few minutes old is a far better answer than a tools/list
// that blocks on a slow backend, and the background refresher bounds how
// old it can get.
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Entry is one cached tool: the connector that serves it, and the tool
// as the gateway advertises it (name already qualified, gateway-stamped
// arguments already scrubbed out of the schema).
type Entry struct {
	ConnectorID   string
	ConnectorName string
	// ToolName is the backend's own, unprefixed name.
	ToolName string
	Tool     mcp.Tool
}

// Snapshot is the result of a cache read.
type Snapshot struct {
	Entries []Entry
	// Hit reports whether anything was cached at all.
	Hit bool
	// Stale reports that the entries are past their TTL (or were
	// explicitly marked stale) and are being served anyway.
	Stale bool
}

// ToolCache stores a tenant's advertised tools.
//
// Implementations must be safe for concurrent use and must never return
// another tenant's rows: every method is tenant-scoped, and the store
// beneath enforces that with a tenant_id predicate.
type ToolCache interface {
	// Get returns the tenant's cached tools. A miss is (Snapshot{}, nil)
	// -- not an error, since the caller's recovery is a live fan-out
	// either way.
	Get(ctx context.Context, tenantID string) (Snapshot, error)
	// Put replaces the cached tools of every connector represented in
	// entries. Connectors absent from entries are left alone, so a
	// single-connector refresh does not wipe the rest.
	Put(ctx context.Context, tenantID string, entries []Entry) error
	// InvalidateConnector drops one connector's cached tools.
	InvalidateConnector(ctx context.Context, connectorID string) error
}

// L2 is the Postgres-backed ToolCache.
type L2 struct {
	store      store.ToolCacheStore
	ttl        time.Duration
	serveStale bool
	now        func() time.Time
}

// Options configures an L2.
type Options struct {
	// TTL is how long a written entry stays fresh. Zero uses 30m.
	TTL time.Duration
	// ServeStale returns expired entries rather than reporting a miss.
	ServeStale bool
	// Now returns the current time; tests override it.
	Now func() time.Time
}

// NewL2 returns a ToolCache over a store.ToolCacheStore.
func NewL2(s store.ToolCacheStore, opts Options) *L2 {
	if opts.TTL <= 0 {
		opts.TTL = 30 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &L2{store: s, ttl: opts.TTL, serveStale: opts.ServeStale, now: opts.Now}
}

var _ ToolCache = (*L2)(nil)

// Get implements ToolCache.
func (l *L2) Get(ctx context.Context, tenantID string) (Snapshot, error) {
	rows, err := l.store.ListByTenant(ctx, tenantID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("cache: read tool cache: %w", err)
	}
	if len(rows) == 0 {
		return Snapshot{}, nil
	}

	now := l.now()
	var stale bool
	entries := make([]Entry, 0, len(rows))
	for _, row := range rows {
		if row.Stale || !now.Before(row.ExpiresAt) {
			stale = true
		}
		entry, err := toEntry(row)
		if err != nil {
			return Snapshot{}, err
		}
		entries = append(entries, entry)
	}

	if stale && !l.serveStale {
		// Report a miss so the caller fans out live, but leave the
		// rows in place: the background cleanup owns deletion, and
		// dropping them here would turn a slow backend into an empty
		// tool list for everyone.
		return Snapshot{Stale: true}, nil
	}

	return Snapshot{Entries: entries, Hit: true, Stale: stale}, nil
}

// Put implements ToolCache.
//
// Writing is delete-then-insert per connector rather than a bare upsert:
// a tool a backend has REMOVED has to disappear from the cache, and an
// upsert alone would leave it there until it expired.
func (l *L2) Put(ctx context.Context, tenantID string, entries []Entry) error {
	now := l.now()
	expires := now.Add(l.ttl)

	connectors := make(map[string]struct{})
	for _, e := range entries {
		connectors[e.ConnectorID] = struct{}{}
	}
	for id := range connectors {
		if err := l.store.DeleteByConnector(ctx, id); err != nil {
			return fmt.Errorf("cache: clear connector %s: %w", id, err)
		}
	}

	if len(entries) == 0 {
		return nil
	}

	rows := make([]*store.CachedTool, 0, len(entries))
	for _, e := range entries {
		schema, err := schemaToMap(e.Tool.InputSchema)
		if err != nil {
			return err
		}
		rows = append(rows, &store.CachedTool{
			TenantID:      tenantID,
			ConnectorID:   e.ConnectorID,
			ToolNamespace: e.ConnectorName,
			ToolName:      e.ToolName,
			Description:   e.Tool.Description,
			InputSchema:   schema,
			CachedAt:      now,
			ExpiresAt:     expires,
			Stale:         false,
		})
	}

	if err := l.store.Upsert(ctx, rows...); err != nil {
		return fmt.Errorf("cache: write tool cache: %w", err)
	}
	return nil
}

// InvalidateConnector implements ToolCache.
func (l *L2) InvalidateConnector(ctx context.Context, connectorID string) error {
	if err := l.store.MarkStale(ctx, connectorID); err != nil {
		return fmt.Errorf("cache: invalidate connector %s: %w", connectorID, err)
	}
	return nil
}

// toEntry rebuilds the advertised tool from a cached row. The qualified
// name is reconstructed rather than stored, so a connector rename takes
// effect on the next refresh without a schema migration.
func toEntry(row *store.CachedTool) (Entry, error) {
	schema, err := mapToSchema(row.InputSchema)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		ConnectorID:   row.ConnectorID,
		ConnectorName: row.ToolNamespace,
		ToolName:      row.ToolName,
		Tool: mcp.Tool{
			Name:        row.ToolNamespace + "__" + row.ToolName,
			Description: row.Description,
			InputSchema: schema,
		},
	}, nil
}

// schemaToMap and mapToSchema round-trip an InputSchema through the
// document-shaped map the store persists. They go via JSON so the
// schema's custom marshaling -- which preserves the keywords the struct
// does not model -- is the single definition of the on-disk shape.
func schemaToMap(s mcp.InputSchema) (map[string]any, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("cache: encode tool schema: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("cache: encode tool schema: %w", err)
	}
	return out, nil
}

func mapToSchema(m map[string]any) (mcp.InputSchema, error) {
	if len(m) == 0 {
		return mcp.InputSchema{Type: "object"}, nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return mcp.InputSchema{}, fmt.Errorf("cache: decode tool schema: %w", err)
	}
	var out mcp.InputSchema
	if err := json.Unmarshal(raw, &out); err != nil {
		return mcp.InputSchema{}, fmt.Errorf("cache: decode tool schema: %w", err)
	}
	return out, nil
}
