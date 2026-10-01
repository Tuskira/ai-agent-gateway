package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/ops"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func TestCache_Stats_Happy(t *testing.T) {
	deps := newTestDeps()
	cachedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := cachedAt.Add(30 * time.Minute)
	deps.CacheOps = &fakeCacheOps{
		store: deps.Store.Connectors(),
		stats: ops.CacheStats{
			Connectors: []ops.CacheConnectorStats{
				{ConnectorID: "connector-1", Name: "okta", Tools: 5, Stale: 1, CachedAt: cachedAt, ExpiresAt: expiresAt},
			},
			TotalTools: 5,
			StaleTools: 1,
		},
	}
	h := Cache{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/cache/stats", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/cache/stats", h.Stats, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var got cacheStatsView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TotalTools != 5 || got.StaleTools != 1 || len(got.Connectors) != 1 {
		t.Fatalf("body = %+v", got)
	}
	c := got.Connectors[0]
	if c.ConnectorID != "connector-1" || c.Name != "okta" || c.Tools != 5 || c.Stale != 1 {
		t.Errorf("connector stats = %+v", c)
	}
	if c.CachedAt != cachedAt.Format(time.RFC3339) || c.ExpiresAt != expiresAt.Format(time.RFC3339) {
		t.Errorf("timestamps = %+v", c)
	}
}

func TestCache_Stats_OpsUnavailable503(t *testing.T) {
	h := Cache{Deps: newTestDeps()} // Deps.CacheOps left nil

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/cache/stats", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/cache/stats", h.Stats, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "unavailable")
}

func TestCache_Search_Happy(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeCacheOps{
		store: deps.Store.Connectors(),
		searchTools: []store.CachedTool{
			{ConnectorID: "connector-1", ToolNamespace: "okta", ToolName: "list_users", Description: "List users",
				InputSchema: map[string]any{"type": "object"}},
		},
	}
	deps.CacheOps = fake
	h := Cache{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/cache/search?q=list&limit=5", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/cache/search", h.Search, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fake.lastSearchQuery != "list" || fake.lastSearchLimit != 5 {
		t.Errorf("Search called with q=%q limit=%d, want q=list limit=5", fake.lastSearchQuery, fake.lastSearchLimit)
	}

	var page struct {
		Items []cacheSearchResultView `json:"items"`
		Total int                     `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 1 || page.Items[0].ToolName != "list_users" || page.Items[0].ConnectorName != "okta" {
		t.Fatalf("page = %+v", page)
	}
}

func TestCache_Search_IncludeStaleDefaultsFalse(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeCacheOps{store: deps.Store.Connectors()}
	deps.CacheOps = fake
	h := Cache{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/cache/search?q=list", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/cache/search", h.Search, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fake.lastSearchIncludeStale {
		t.Error("include_stale defaulted to true, want false")
	}
}

func TestCache_Search_IncludeStaleTrue(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeCacheOps{store: deps.Store.Connectors()}
	deps.CacheOps = fake
	h := Cache{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/cache/search?q=list&include_stale=true", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/cache/search", h.Search, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !fake.lastSearchIncludeStale {
		t.Error("include_stale=true was not parsed through to CacheOps.Search")
	}
}

func TestCache_Search_IncludeStaleFalseExplicit(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeCacheOps{store: deps.Store.Connectors()}
	deps.CacheOps = fake
	h := Cache{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/cache/search?q=list&include_stale=false", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/cache/search", h.Search, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fake.lastSearchIncludeStale {
		t.Error("include_stale=false was parsed as true")
	}
}

func TestCache_Search_DefaultLimit(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeCacheOps{store: deps.Store.Connectors()}
	deps.CacheOps = fake
	h := Cache{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/cache/search", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/cache/search", h.Search, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fake.lastSearchLimit != defaultCacheSearchLimit {
		t.Errorf("limit = %d, want default %d", fake.lastSearchLimit, defaultCacheSearchLimit)
	}
}

func TestCache_Search_OpsUnavailable503(t *testing.T) {
	h := Cache{Deps: newTestDeps()} // Deps.CacheOps left nil

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/cache/search", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/cache/search", h.Search, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "unavailable")
}

func TestCache_RefreshAll_Happy(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeCacheOps{store: deps.Store.Connectors(), refreshed: 14}
	deps.CacheOps = fake
	h := Cache{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/cache/refresh", nil), "tenant-a", "admin")
	w := serve(http.MethodPost, "/cache/refresh", h.RefreshAll, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fake.lastRefreshConnectorID != "" {
		t.Errorf("RefreshAll called Refresh with connectorID = %q, want empty (every connector)", fake.lastRefreshConnectorID)
	}

	var got refreshView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Refreshed != 14 {
		t.Errorf("Refreshed = %d, want 14", got.Refreshed)
	}
}

func TestCache_RefreshConnector_Happy(t *testing.T) {
	deps := newTestDeps()
	id := createTestConnector(t, deps, "tenant-a")
	fake := &fakeCacheOps{store: deps.Store.Connectors(), refreshed: 3}
	deps.CacheOps = fake
	h := Cache{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/cache/refresh/connectors/"+id, nil), "tenant-a", "admin")
	w := serve(http.MethodPost, "/cache/refresh/connectors/{id}", h.RefreshConnector, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fake.lastRefreshConnectorID != id {
		t.Errorf("Refresh called with connectorID = %q, want %q", fake.lastRefreshConnectorID, id)
	}
}

func TestCache_RefreshConnector_WrongTenant404(t *testing.T) {
	deps := newTestDeps()
	id := createTestConnector(t, deps, "tenant-a")
	deps.CacheOps = &fakeCacheOps{store: deps.Store.Connectors()}
	h := Cache{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/cache/refresh/connectors/"+id, nil), "tenant-b", "admin")
	w := serve(http.MethodPost, "/cache/refresh/connectors/{id}", h.RefreshConnector, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "not_found")
}

func TestCache_Refresh_OpsUnavailable503(t *testing.T) {
	h := Cache{Deps: newTestDeps()} // Deps.CacheOps left nil

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/cache/refresh", nil), "tenant-a", "admin")
	w := serve(http.MethodPost, "/cache/refresh", h.RefreshAll, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "unavailable")
}

func TestCache_InvalidateAll_Happy(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeCacheOps{store: deps.Store.Connectors()}
	deps.CacheOps = fake
	h := Cache{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodDelete, "/cache", nil), "tenant-a", "admin")
	w := serve(http.MethodDelete, "/cache", h.InvalidateAll, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", w.Code, w.Body.String())
	}
	if fake.lastInvalidateConnectorID != "" {
		t.Errorf("InvalidateAll called Invalidate with connectorID = %q, want empty", fake.lastInvalidateConnectorID)
	}
}

func TestCache_InvalidateConnector_Happy(t *testing.T) {
	deps := newTestDeps()
	id := createTestConnector(t, deps, "tenant-a")
	fake := &fakeCacheOps{store: deps.Store.Connectors()}
	deps.CacheOps = fake
	h := Cache{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodDelete, "/cache/connectors/"+id, nil), "tenant-a", "admin")
	w := serve(http.MethodDelete, "/cache/connectors/{id}", h.InvalidateConnector, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", w.Code, w.Body.String())
	}
	if fake.lastInvalidateConnectorID != id {
		t.Errorf("Invalidate called with connectorID = %q, want %q", fake.lastInvalidateConnectorID, id)
	}
}

func TestCache_InvalidateConnector_WrongTenant404(t *testing.T) {
	deps := newTestDeps()
	id := createTestConnector(t, deps, "tenant-a")
	deps.CacheOps = &fakeCacheOps{store: deps.Store.Connectors()}
	h := Cache{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodDelete, "/cache/connectors/"+id, nil), "tenant-b", "admin")
	w := serve(http.MethodDelete, "/cache/connectors/{id}", h.InvalidateConnector, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "not_found")
}

func TestCache_Invalidate_OpsUnavailable503(t *testing.T) {
	h := Cache{Deps: newTestDeps()} // Deps.CacheOps left nil

	req := withPrincipal(httptest.NewRequest(http.MethodDelete, "/cache", nil), "tenant-a", "admin")
	w := serve(http.MethodDelete, "/cache", h.InvalidateAll, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "unavailable")
}
