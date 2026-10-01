package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/cache"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

const tenant = "tenant-a"

func entries(names ...string) []cache.Entry {
	out := make([]cache.Entry, 0, len(names))
	for _, name := range names {
		out = append(out, cache.Entry{
			ConnectorID:   "conn-1",
			ConnectorName: "alpha",
			ToolName:      name,
			Tool: mcp.Tool{
				Name:        "alpha__" + name,
				Description: name + " does something",
				InputSchema: mcp.InputSchema{
					Type:       "object",
					Properties: map[string]any{"q": map[string]any{"type": "string"}},
					Required:   []string{"q"},
					Extra:      map[string]any{"additionalProperties": false},
				},
			},
		})
	}
	return out
}

func TestMissOnAnEmptyCache(t *testing.T) {
	c := cache.NewL2(dptest.New().ToolCache(), cache.Options{TTL: time.Hour, ServeStale: true})

	snap, err := c.Get(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Hit {
		t.Fatalf("an empty cache reported a hit: %+v", snap)
	}
}

func TestPutThenGetRoundTripsTheAdvertisedTool(t *testing.T) {
	c := cache.NewL2(dptest.New().ToolCache(), cache.Options{TTL: time.Hour, ServeStale: true})
	ctx := context.Background()

	if err := c.Put(ctx, tenant, entries("echo")); err != nil {
		t.Fatal(err)
	}

	snap, err := c.Get(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Hit || snap.Stale || len(snap.Entries) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}

	got := snap.Entries[0]
	if got.Tool.Name != "alpha__echo" || got.ToolName != "echo" || got.ConnectorID != "conn-1" {
		t.Fatalf("entry = %+v", got)
	}
	if got.Tool.Description != "echo does something" {
		t.Errorf("description = %q", got.Tool.Description)
	}
	// A keyword the struct does not model must survive the trip through
	// the store, or the model never sees it.
	if got.Tool.InputSchema.Extra["additionalProperties"] != false {
		t.Errorf("schema lost additionalProperties: %+v", got.Tool.InputSchema)
	}
	if len(got.Tool.InputSchema.Required) != 1 || got.Tool.InputSchema.Required[0] != "q" {
		t.Errorf("required = %v", got.Tool.InputSchema.Required)
	}
}

func TestExpiredEntriesAreServedStaleWhenConfigured(t *testing.T) {
	// A tool list a few minutes old beats blocking tools/list on a slow
	// backend; the background refresher bounds how old it can get.
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	c := cache.NewL2(dptest.New().ToolCache(), cache.Options{TTL: 30 * time.Minute, ServeStale: true, Now: clock})
	ctx := context.Background()

	if err := c.Put(ctx, tenant, entries("echo")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Minute)

	snap, err := c.Get(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Hit || !snap.Stale || len(snap.Entries) != 1 {
		t.Fatalf("snapshot = %+v, want a stale hit", snap)
	}
}

func TestExpiredEntriesAreAMissWhenStaleServingIsOff(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	st := dptest.New()
	c := cache.NewL2(st.ToolCache(), cache.Options{TTL: 30 * time.Minute, ServeStale: false, Now: clock})
	ctx := context.Background()

	if err := c.Put(ctx, tenant, entries("echo")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Minute)

	snap, err := c.Get(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Hit {
		t.Fatalf("snapshot = %+v, want a miss", snap)
	}
	// The rows stay: deleting them here would turn a slow backend into
	// an empty tool list for everyone. Cleanup owns deletion.
	rows, err := st.ToolCache().ListByTenant(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("a miss deleted the rows: %d left", len(rows))
	}
}

func TestPutRemovesToolsTheBackendNoLongerAdvertises(t *testing.T) {
	// An upsert alone would leave a removed tool in the cache until it
	// expired, so the gateway would keep advertising a tool that no
	// longer exists.
	c := cache.NewL2(dptest.New().ToolCache(), cache.Options{TTL: time.Hour, ServeStale: true})
	ctx := context.Background()

	if err := c.Put(ctx, tenant, entries("echo", "retired")); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, tenant, entries("echo")); err != nil {
		t.Fatal(err)
	}

	snap, err := c.Get(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 1 || snap.Entries[0].ToolName != "echo" {
		t.Fatalf("entries = %+v, want only echo", snap.Entries)
	}
}

func TestInvalidateConnectorMarksItsToolsStale(t *testing.T) {
	c := cache.NewL2(dptest.New().ToolCache(), cache.Options{TTL: time.Hour, ServeStale: true})
	ctx := context.Background()

	if err := c.Put(ctx, tenant, entries("echo")); err != nil {
		t.Fatal(err)
	}
	if err := c.InvalidateConnector(ctx, "conn-1"); err != nil {
		t.Fatal(err)
	}

	snap, err := c.Get(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Stale {
		t.Fatal("an invalidated connector's tools were not reported stale")
	}
}

func TestCacheIsTenantScoped(t *testing.T) {
	c := cache.NewL2(dptest.New().ToolCache(), cache.Options{TTL: time.Hour, ServeStale: true})
	ctx := context.Background()

	if err := c.Put(ctx, tenant, entries("echo")); err != nil {
		t.Fatal(err)
	}

	snap, err := c.Get(ctx, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Hit {
		t.Fatalf("tenant-b read tenant-a's cached tools: %+v", snap)
	}
}
