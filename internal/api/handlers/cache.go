package handlers

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
)

// defaultCacheSearchLimit bounds GET /cache/search when ?limit= is
// omitted or invalid.
const defaultCacheSearchLimit = 20

// Cache implements the /api/v1/cache routes: read and manage the tool
// cache the MCP data plane populates (internal/dataplane/ops.go).
type Cache struct{ Deps }

type cacheConnectorStatsView struct {
	ConnectorID string `json:"connector_id"`
	Name        string `json:"name"`
	Tools       int    `json:"tools"`
	Stale       int    `json:"stale"`
	CachedAt    string `json:"cached_at"`
	ExpiresAt   string `json:"expires_at"`
}

type cacheStatsView struct {
	Connectors []cacheConnectorStatsView `json:"connectors"`
	TotalTools int                       `json:"total_tools"`
	StaleTools int                       `json:"stale_tools"`
}

// Stats handles GET /api/v1/cache/stats.
func (h Cache) Stats(w http.ResponseWriter, r *http.Request) {
	if h.CacheOps == nil {
		writeOpsUnavailable(w)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	stats, err := h.CacheOps.Stats(r.Context(), tid)
	if err != nil {
		writeStoreErr(w, r, "cache stats", err)
		return
	}

	view := cacheStatsView{
		Connectors: make([]cacheConnectorStatsView, 0, len(stats.Connectors)),
		TotalTools: stats.TotalTools,
		StaleTools: stats.StaleTools,
	}
	for _, c := range stats.Connectors {
		view.Connectors = append(view.Connectors, cacheConnectorStatsView{
			ConnectorID: c.ConnectorID,
			Name:        c.Name,
			Tools:       c.Tools,
			Stale:       c.Stale,
			CachedAt:    formatTime(c.CachedAt),
			ExpiresAt:   formatTime(c.ExpiresAt),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// cacheSearchResultView is one hit in GET /cache/search's items. It is a
// distinct shape from cachedToolView (connectors.go): search results are
// tenant-wide, so the connector is identified by name for readability
// rather than by the "tool_namespace" field name cachedToolView uses to
// stay close to the store.CachedTool column it mirrors.
type cacheSearchResultView struct {
	ConnectorID   string         `json:"connector_id"`
	ConnectorName string         `json:"connector_name"`
	ToolName      string         `json:"tool_name"`
	Description   string         `json:"description,omitempty"`
	InputSchema   map[string]any `json:"input_schema,omitempty"`
}

// Search handles GET /api/v1/cache/search?q=&limit=&include_stale=. Stale
// rows (is_stale) are excluded unless include_stale=true is passed.
func (h Cache) Search(w http.ResponseWriter, r *http.Request) {
	if h.CacheOps == nil {
		writeOpsUnavailable(w)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	q := r.URL.Query().Get("q")
	limit := defaultCacheSearchLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > httpx.MaxLimit {
		limit = httpx.MaxLimit
	}
	includeStale := false
	if raw := r.URL.Query().Get("include_stale"); raw != "" {
		if b, err := strconv.ParseBool(raw); err == nil {
			includeStale = b
		}
	}

	tools, err := h.CacheOps.Search(r.Context(), tid, q, limit, includeStale)
	if err != nil {
		writeStoreErr(w, r, "cache search", err)
		return
	}

	views := make([]cacheSearchResultView, 0, len(tools))
	for i := range tools {
		t := &tools[i]
		views = append(views, cacheSearchResultView{
			ConnectorID:   t.ConnectorID,
			ConnectorName: t.ToolNamespace,
			ToolName:      t.ToolName,
			Description:   t.Description,
			InputSchema:   t.InputSchema,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: views, Total: len(views)})
}

type refreshView struct {
	Refreshed int `json:"refreshed"`
}

// RefreshAll handles POST /api/v1/cache/refresh: refresh every connector
// in the caller's tenant.
func (h Cache) RefreshAll(w http.ResponseWriter, r *http.Request) {
	h.refresh(w, r, "")
}

// RefreshConnector handles POST /api/v1/cache/refresh/connectors/{id}.
func (h Cache) RefreshConnector(w http.ResponseWriter, r *http.Request) {
	h.refresh(w, r, chi.URLParam(r, "id"))
}

func (h Cache) refresh(w http.ResponseWriter, r *http.Request, connectorID string) {
	if h.CacheOps == nil {
		writeOpsUnavailable(w)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	n, err := h.CacheOps.Refresh(r.Context(), tid, connectorID)
	if err != nil {
		writeStoreErr(w, r, "refresh cache", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, refreshView{Refreshed: n})
}

// InvalidateAll handles DELETE /api/v1/cache: drop the cache for every
// connector in the caller's tenant.
func (h Cache) InvalidateAll(w http.ResponseWriter, r *http.Request) {
	h.invalidate(w, r, "")
}

// InvalidateConnector handles DELETE /api/v1/cache/connectors/{id}.
func (h Cache) InvalidateConnector(w http.ResponseWriter, r *http.Request) {
	h.invalidate(w, r, chi.URLParam(r, "id"))
}

func (h Cache) invalidate(w http.ResponseWriter, r *http.Request, connectorID string) {
	if h.CacheOps == nil {
		writeOpsUnavailable(w)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	if err := h.CacheOps.Invalidate(r.Context(), tid, connectorID); err != nil {
		writeStoreErr(w, r, "invalidate cache", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
