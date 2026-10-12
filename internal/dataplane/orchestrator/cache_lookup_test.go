package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/cache"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/router"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/memory"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// cacheLookups records what Deps.OnToolCacheLookup is told.
type cacheLookups struct {
	mu  sync.Mutex
	got []string
}

func (c *cacheLookups) observe(result string) {
	c.mu.Lock()
	c.got = append(c.got, result)
	c.mu.Unlock()
}

func (c *cacheLookups) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.got
	c.got = nil
	return out
}

// listWithCache runs one tools/list through a real orchestrator over a
// real L2 cache (on the in-memory dptest store) and returns what the
// OnToolCacheLookup hook saw.
func listWithCache(t *testing.T, serveStale bool, seed func(st *dptest.Store, now time.Time), hook bool) []string {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	st := dptest.New()
	if err := st.Tenants().Create(ctx, &store.Tenant{ID: testTenant, Slug: "a", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if seed != nil {
		seed(st, now)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cl := client.New(client.Options{Logger: logger})
	t.Cleanup(func() { _ = cl.Close() })

	seen := &cacheLookups{}
	deps := Deps{
		Sessions:   session.NewManager(memory.New(), session.Options{Logger: logger}),
		Profiles:   profile.New(st.AgentProfiles(), st.Connectors(), st.Skills(), profile.Options{}),
		Router:     router.New(st.Connectors(), router.Options{}),
		Client:     cl,
		Cache:      cache.NewL2(st.ToolCache(), cache.Options{ServeStale: serveStale, Now: func() time.Time { return now }}),
		Connectors: st.Connectors(),
		Logger:     logger,
	}
	if hook {
		deps.OnToolCacheLookup = seen.observe
	}
	orch, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	p := &pkgauth.Principal{Subject: "key-1", TenantID: testTenant, Roles: []string{"agent"}, AuthMethod: "apikey"}
	req := &mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsList}
	res := orch.Handle(pkgauth.WithPrincipal(ctx, p), Request{JSONRPC: req, Principal: p})
	if res.Response == nil || res.Response.Error != nil {
		t.Fatalf("tools/list failed: %+v", res.Response)
	}
	return seen.take()
}

func cachedTool(expires time.Time, stale bool) func(*dptest.Store, time.Time) {
	return func(st *dptest.Store, now time.Time) {
		err := st.ToolCache().Upsert(context.Background(), &store.CachedTool{
			TenantID: testTenant, ConnectorID: "c1", ToolNamespace: "alpha", ToolName: "echo",
			InputSchema: map[string]any{"type": "object"}, CachedAt: now, ExpiresAt: expires, Stale: stale,
		})
		if err != nil {
			panic(err)
		}
	}
}

func TestToolCacheLookupHook(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		serveStale bool
		seed       func(*dptest.Store, time.Time)
		want       string
	}{
		{"empty cache is a miss", true, nil, cache.LookupMiss},
		{"fresh rows are a hit", true, cachedTool(now.Add(time.Hour), false), cache.LookupHit},
		{"expired rows served stale", true, cachedTool(now.Add(-time.Minute), false), cache.LookupStale},
		{"rows marked stale are served stale", true, cachedTool(now.Add(time.Hour), true), cache.LookupStale},
		{"expired rows with serve_stale off are a miss", false, cachedTool(now.Add(-time.Minute), false), cache.LookupMiss},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := listWithCache(t, tc.serveStale, tc.seed, true)
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("hook saw %v, want [%s]", got, tc.want)
			}
		})
	}
}

// A nil hook (every deployment without metrics) must not panic.
func TestToolCacheLookupHookNilIsSafe(t *testing.T) {
	if got := listWithCache(t, true, nil, false); len(got) != 0 {
		t.Errorf("no hook installed, yet saw %v", got)
	}
}
