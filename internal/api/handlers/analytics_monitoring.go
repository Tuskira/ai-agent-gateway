package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Caller roles on the Token Monitoring page. A caller is an API key; its
// role comes from the key row, except that ingested traffic is always
// "interceptor" (it was reported, not proxied). Keys the tenant no longer
// has, calls with no key, and viewer keys are "other".
const (
	roleAgent       = "agent"
	roleAdmin       = "admin"
	roleInterceptor = "interceptor"
	roleOther       = "other"
)

var callerRoles = []string{roleAgent, roleAdmin, roleInterceptor, roleOther}

const maxMonitoringModelName = 512

// tokenTotalsView adds the headline total: tokens plus cache reads and
// writes, with its change against the same measure before.
type tokenTotalsView struct {
	analytics.TokenUsage
	DeltaPct                *float64 `json:"delta_pct"`
	TokensWithCache         uint64   `json:"tokens_with_cache"`
	TokensWithCacheDeltaPct *float64 `json:"tokens_with_cache_delta_pct"`
}

type tokenModelView struct {
	analytics.ModelTokenUsage
	DeltaPct *float64 `json:"delta_pct"`
}

type tokenKeyView struct {
	analytics.KeyTokenUsage
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	DeltaPct *float64 `json:"delta_pct"`
}

type tokenSessionView struct {
	analytics.SessionTokenUsage
	KeyName string `json:"key_name"`
	Role    string `json:"role"`
}

type roleUsage struct {
	Role   string `json:"role"`
	Tokens uint64 `json:"tokens"`
	Calls  uint64 `json:"calls"`
}

type keyRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// tokenMonitoringView is the body of the page and of both drill-downs:
// Model or Key names the drill-down's subject, ByRole is the page's role
// split, Sessions are the drill-downs'.
type tokenMonitoringView struct {
	Range         analytics.Range         `json:"range"`
	Start         time.Time               `json:"start"`
	End           time.Time               `json:"end"`
	Granularity   analytics.Granularity   `json:"granularity"`
	Model         string                  `json:"model,omitempty"`
	Key           *keyRef                 `json:"key,omitempty"`
	Totals        tokenTotalsView         `json:"totals"`
	ByModel       []tokenModelView        `json:"by_model"`
	ByKey         []tokenKeyView          `json:"by_key"`
	ByRole        []roleUsage             `json:"by_role,omitempty"`
	Burn          []analytics.TokenBucket `json:"burn"`
	Sessions      []tokenSessionView      `json:"sessions,omitempty"`
	SessionsTotal int                     `json:"sessions_total,omitempty"`
}

// TokenMonitoring handles GET /api/v1/analytics/token-monitoring?range=24h|7d|30d
// or ?from=YYYY-MM-DD&to=YYYY-MM-DD.
func (h Analytics) TokenMonitoring(w http.ResponseWriter, r *http.Request) {
	h.serveMonitoring(w, r, analytics.TokenMonitoringQuery{}, func(view *tokenMonitoringView, _ map[string]*store.APIKey) {
		view.ByRole = make([]roleUsage, len(callerRoles))
		idx := map[string]int{}
		for i, role := range callerRoles {
			view.ByRole[i].Role, idx[role] = role, i
		}
		for _, k := range view.ByKey {
			view.ByRole[idx[k.Role]].Tokens += k.Tokens
			view.ByRole[idx[k.Role]].Calls += k.Calls
		}
	})
}

// TokenMonitoringModel handles GET
// /api/v1/analytics/token-monitoring/model?model=...&range=...&limit=&offset=.
// The model is a query parameter because names carry "/" and ":".
func (h Analytics) TokenMonitoringModel(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" || len(model) > maxMonitoringModelName {
		httpx.ValidationError(w, "model is required (at most 512 characters)")
		return
	}
	h.serveMonitoring(w, r, analytics.TokenMonitoringQuery{Model: model}, func(view *tokenMonitoringView, _ map[string]*store.APIKey) {
		view.Model = model
	})
}

// TokenMonitoringKey handles GET
// /api/v1/analytics/token-monitoring/keys/{id}?range=...&limit=&offset=.
func (h Analytics) TokenMonitoringKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	h.serveMonitoring(w, r, analytics.TokenMonitoringQuery{KeyID: id}, func(view *tokenMonitoringView, keys map[string]*store.APIKey) {
		view.Key = &keyRef{ID: id, Name: nameOf(keys[id]), Role: callerRole("", keys[id])}
	})
}

// serveMonitoring answers the page (q names no subject) or a drill-down (q
// names a model or a key); finish adds what is specific to each.
func (h Analytics) serveMonitoring(w http.ResponseWriter, r *http.Request, q analytics.TokenMonitoringQuery, finish func(*tokenMonitoringView, map[string]*store.APIKey)) {
	mon, tid, ok := h.monitoringRequest(w, r)
	if !ok {
		return
	}
	if q.Range, q.Period, ok = parsePeriod(w, r, analytics.Range24h); !ok {
		return
	}
	page := httpx.ParsePagination(r)
	q.Limit, q.Offset = page.Limit, page.Offset
	g, err := mon.TokenMonitoring(r.Context(), tid, q)
	if err != nil {
		writeAnalyticsErr(w, r, "compute token monitoring", err)
		return
	}
	keys := h.tenantKeys(r, tid)
	sessions := make([]tokenSessionView, 0, len(g.Sessions))
	for _, s := range g.Sessions {
		sessions = append(sessions, tokenSessionView{SessionTokenUsage: s, KeyName: nameOf(keys[s.KeyID]), Role: callerRole("", keys[s.KeyID])})
	}
	view := tokenMonitoringView{
		Range: g.Range, Start: g.Start, End: g.End, Granularity: g.Granularity,
		Totals:        totalsView(g.Totals),
		ByModel:       modelViews(g.ByModel),
		ByKey:         keyViews(g.ByKey, keys),
		Burn:          g.Burn,
		Sessions:      sessions,
		SessionsTotal: g.SessionsTotal,
	}
	finish(&view, keys)
	httpx.WriteJSON(w, http.StatusOK, view)
}

// monitoringRequest answers 404 when no reader can serve token monitoring
// (no ClickHouse sink).
func (h Analytics) monitoringRequest(w http.ResponseWriter, r *http.Request) (analytics.TokenMonitoringReader, string, bool) {
	mon, ok := h.Analytics.(analytics.TokenMonitoringReader)
	if h.Analytics == nil || !ok {
		httpx.NotFound(w, analyticsUnavailableMessage)
		return nil, "", false
	}
	tid, ok := requireTenant(w, r)
	return mon, tid, ok
}

// tenantKeys indexes the tenant's API keys by id for names and roles. A
// failed read degrades to unnamed callers rather than failing the page.
func (h Analytics) tenantKeys(r *http.Request, tid string) map[string]*store.APIKey {
	out := map[string]*store.APIKey{}
	keys, err := h.Store.APIKeys().List(r.Context(), tid)
	if err != nil {
		applog.From(r.Context()).Warn("token monitoring: list api keys failed; callers shown unnamed", "error", err)
		return out
	}
	for _, k := range keys {
		out[k.ID] = k
	}
	return out
}

func callerRole(source string, k *store.APIKey) string {
	if source == "interceptor" {
		return roleInterceptor
	}
	if k == nil {
		return roleOther
	}
	switch k.Role {
	case "agent":
		return roleAgent
	case "admin", pkgauth.RolePlatformAdmin:
		return roleAdmin
	case "interceptor":
		return roleInterceptor
	}
	return roleOther
}

func nameOf(k *store.APIKey) string {
	if k == nil {
		return ""
	}
	return k.Name
}

func totalsView(u analytics.TokenUsage) tokenTotalsView {
	withCache := u.Tokens + u.CacheReadTokens + u.CacheWriteTokens
	return tokenTotalsView{
		TokenUsage:              u,
		DeltaPct:                analytics.DeltaPct(u.Tokens, u.PrevTokens),
		TokensWithCache:         withCache,
		TokensWithCacheDeltaPct: analytics.DeltaPct(withCache, u.PrevTokensWithCache),
	}
}

func modelViews(in []analytics.ModelTokenUsage) []tokenModelView {
	out := make([]tokenModelView, 0, len(in))
	for _, m := range in {
		out = append(out, tokenModelView{ModelTokenUsage: m, DeltaPct: analytics.DeltaPct(m.Tokens, m.PrevTokens)})
	}
	return out
}

func keyViews(in []analytics.KeyTokenUsage, keys map[string]*store.APIKey) []tokenKeyView {
	out := make([]tokenKeyView, 0, len(in))
	for _, k := range in {
		out = append(out, tokenKeyView{KeyTokenUsage: k, Name: nameOf(keys[k.KeyID]), Role: callerRole(k.Source, keys[k.KeyID]),
			DeltaPct: analytics.DeltaPct(k.Tokens, k.PrevTokens)})
	}
	return out
}
