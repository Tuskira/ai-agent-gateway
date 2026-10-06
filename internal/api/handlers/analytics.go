package handlers

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// analyticsUnavailableMessage is the body of every /analytics/* route's
// 404 when no analytics.Reader is configured (no ClickHouse sink). The
// web UI (web/src/lib/overview.ts) treats any failure reaching this
// route -- this 404 included -- as "no data yet", not an error.
const analyticsUnavailableMessage = "analytics requires the ClickHouse sink"

// Analytics implements the /api/v1/analytics routes. h.Analytics is nil
// on any deployment without a ClickHouse sink configured; every method
// below answers 404 in that case rather than calling through a nil --
// except the LLM-logs list and detail, which fall back to h.LLMCalls
// (see llmCalls).
type Analytics struct{ Deps }

// Overview handles GET /api/v1/analytics/overview?range=24h|7d|30d, or
// ?from=YYYY-MM-DD&to=YYYY-MM-DD for custom dates.
func (h Analytics) Overview(w http.ResponseWriter, r *http.Request) {
	if h.Analytics == nil {
		httpx.NotFound(w, analyticsUnavailableMessage)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	rng, p, ok := parsePeriod(w, r, analytics.Range24h)
	if !ok {
		return
	}

	ov, err := h.Analytics.Overview(r.Context(), tid, p)
	if err != nil {
		writeAnalyticsErr(w, r, "compute analytics overview", err)
		return
	}
	ov.Range = rng
	h.resolveConnectorNames(r, tid, ov.TopConnectors)

	// Every cost figure in this response comes from pkg/pricing's rate
	// card, not a provider-reported invoice, so it's always an estimate.
	// PricingSource reflects which card: the embedded default, or an
	// operator override merged in via llm_proxy.pricing_file (see
	// h.AnalyticsMeta's doc comment). Empty AnalyticsMeta (e.g. a
	// deployment that never set it) means the embedded card.
	ov.CostEstimated = true
	ov.PricingSource = h.AnalyticsMeta.PricingSource
	if ov.PricingSource == "" {
		ov.PricingSource = analytics.PricingSourceEmbedded
	}

	httpx.WriteJSON(w, http.StatusOK, ov)
}

// resolveConnectorNames swaps connector ids for connector names in a
// ranking. The log store only knows ids; names live in Postgres. Rows
// whose connector no longer exists keep the id. Best effort: a store
// error leaves the ranking untouched rather than failing the page.
func (h Analytics) resolveConnectorNames(r *http.Request, tenantID string, rows []analytics.NamedCount) {
	if h.Store == nil || len(rows) == 0 {
		return
	}
	conns, err := h.Store.Connectors().List(r.Context(), tenantID)
	if err != nil {
		return
	}
	names := make(map[string]string, len(conns))
	for _, c := range conns {
		names[c.ID] = c.Name
	}
	for i := range rows {
		if n, ok := names[rows[i].Name]; ok {
			rows[i].Name = n
		}
	}
}

// Models handles GET /api/v1/analytics/models?range=24h|7d|30d: a
// per-model usage summary (calls, tokens, cost, distinct callers) computed
// from llm_calls, feeding the console's Models page. Unlike Overview,
// this defaults to 7d, not 24h -- the page's own framing ("Observed from
// gateway traffic over the last 7 days").
func (h Analytics) Models(w http.ResponseWriter, r *http.Request) {
	if h.Analytics == nil {
		httpx.NotFound(w, analyticsUnavailableMessage)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	raw := r.URL.Query().Get("range")
	if raw == "" {
		raw = "7d"
	}
	rng, ok := analytics.ParseRange(raw)
	if !ok {
		httpx.ValidationError(w, `range must be one of "24h", "7d", "30d"`)
		return
	}

	summary, err := h.Analytics.ModelsSummary(r.Context(), tid, rng)
	if err != nil {
		writeAnalyticsErr(w, r, "compute models summary", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, summary)
}

// Skills handles GET /api/v1/analytics/skills?range=24h|7d|30d: a per
// skill/command usage summary (calls, distinct callers, last seen)
// computed from mcp_access_logs, feeding the console's Skills page.
// Defaults to 7d, same framing as Models. Kind ("skill"/"command") is
// joined in from the skill/command registry (internal/skills; see
// resolveSkillKinds) by name -- the ClickHouse reader knows nothing about
// the registry, only observed traffic.
func (h Analytics) Skills(w http.ResponseWriter, r *http.Request) {
	if h.Analytics == nil {
		httpx.NotFound(w, analyticsUnavailableMessage)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	rng, p, ok := parsePeriod(w, r, analytics.Range7d)
	if !ok {
		return
	}

	summary, err := h.Analytics.SkillsSummary(r.Context(), tid, p)
	if err != nil {
		writeAnalyticsErr(w, r, "compute skills summary", err)
		return
	}
	summary.Range = rng
	h.resolveSkillKinds(r, tid, summary)
	httpx.WriteJSON(w, http.StatusOK, summary)
}

// resolveSkillKinds fills in each row's Kind ("skill" or "command") from
// the skill/command registry (tenant rows override platform rows by
// name, same as the registry's own List) by name. Best effort, mirroring
// resolveConnectorNames: a store error leaves every Kind "" rather than
// failing the page -- traffic is real even when the registry lookup
// isn't available. A name that no longer resolves to any registry row
// (deleted since) is left with Kind "" too, not omitted.
func (h Analytics) resolveSkillKinds(r *http.Request, tenantID string, summary *analytics.SkillsSummary) {
	if h.Store == nil || len(summary.Skills) == 0 {
		return
	}
	list, _, err := h.Store.Skills().List(r.Context(), tenantID, store.SkillListOptions{})
	if err != nil {
		return
	}
	kinds := make(map[string]string, len(list))
	for _, sk := range list {
		kinds[sk.Name] = sk.Kind
	}
	for i := range summary.Skills {
		summary.Skills[i].Kind = kinds[summary.Skills[i].Name]
	}
	if summary.MostUsed != nil {
		summary.MostUsed.Kind = kinds[summary.MostUsed.Name]
	}
}

// SkillsUsage handles GET /api/v1/analytics/skills/usage?range=24h|7d|30d:
// the skills the model was observed USING in LLM traffic (llm_calls, from
// the response's Skill tool calls), not the gateway's own skill loads (that
// is /analytics/skills). Each row says whether the name is registered (a
// tenant or platform skill/command exists) and its kind, so a console can list
// the unregistered ones as "discovered". Defaults to 7d.
func (h Analytics) SkillsUsage(w http.ResponseWriter, r *http.Request) {
	if h.Analytics == nil {
		httpx.NotFound(w, analyticsUnavailableMessage)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	rng, p, ok := parsePeriod(w, r, analytics.Range7d)
	if !ok {
		return
	}
	usage, err := h.Analytics.SkillUsage(r.Context(), tid, p)
	if err != nil {
		writeAnalyticsErr(w, r, "compute skill usage", err)
		return
	}
	// Best effort, like resolveSkillKinds: without the registry every row
	// reads unregistered rather than failing traffic that really happened.
	kinds := map[string]string{}
	if h.Store != nil && len(usage) > 0 {
		if list, _, err := h.Store.Skills().List(r.Context(), tid, store.SkillListOptions{}); err == nil {
			for _, sk := range list {
				kinds[strings.ToLower(sk.Name)] = sk.Kind
			}
		}
	}
	out := analytics.DiscoveredSkills{Range: rng, Skills: make([]analytics.DiscoveredSkill, 0, len(usage))}
	for _, u := range usage {
		kind, reg := kinds[strings.ToLower(u.Name)]
		out.Skills = append(out.Skills, analytics.DiscoveredSkill{
			Name: u.Name, Calls: u.Calls, UsedBy: u.UsedBy, LastSeen: u.LastSeen, Registered: reg, Kind: kind,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// MCPsUsage handles GET /api/v1/analytics/mcps/usage?range=24h|7d|30d: the
// MCP servers (and their tools) the model was observed using in LLM traffic,
// one row per server, sorted by calls descending. A server whose name matches
// a tenant connector's slug or name (case-insensitive) carries that
// connector's slug as registered_connector_slug. A tool named
// "<registered-connector-slug>__<tool>" was routed through this gateway's own
// MCP plane (whose tools are connector__tool): it is attributed to that
// connector with via_gateway=true, whatever alias the client gave the
// gateway ("gw" in mcp__gw__langfuse__get_trace). Defaults to 7d.
func (h Analytics) MCPsUsage(w http.ResponseWriter, r *http.Request) {
	if h.Analytics == nil {
		httpx.NotFound(w, analyticsUnavailableMessage)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	rng, p, ok := parsePeriod(w, r, analytics.Range7d)
	if !ok {
		return
	}
	usage, err := h.Analytics.MCPToolUsage(r.Context(), tid, p)
	if err != nil {
		writeAnalyticsErr(w, r, "compute mcp usage", err)
		return
	}
	slugs := map[string]string{} // lowercased slug or name -> canonical slug
	if h.Store != nil && len(usage) > 0 {
		if conns, err := h.Store.Connectors().List(r.Context(), tid); err == nil {
			for _, c := range conns {
				slugs[strings.ToLower(c.Name)] = c.Slug
				slugs[strings.ToLower(c.Slug)] = c.Slug
			}
		}
	}
	var serverCalls []analytics.MCPServerCalls
	if len(usage) > 0 {
		serverCalls, err = h.Analytics.MCPServerCalls(r.Context(), tid, p, slugs)
		if err != nil {
			writeAnalyticsErr(w, r, "compute mcp server calls", err)
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, analytics.DiscoveredMCPServers{Range: rng, Servers: foldMCPUsage(usage, serverCalls, slugs)})
}

// foldMCPUsage folds per-(server, tool) usage into per-server rows (see
// MCPsUsage). slugs maps a lowercased connector slug or name to its slug.
// A row's Calls is the distinct-LLM-call count from serverCalls (a call using
// two tools of one server counts once); when the reader has no entry for a
// row it falls back to its busiest tool's count, a lower bound that never
// double counts.
func foldMCPUsage(usage []analytics.MCPToolUsage, serverCalls []analytics.MCPServerCalls, slugs map[string]string) []analytics.DiscoveredMCPServer {
	type key struct {
		server string
		via    bool
	}
	distinct := make(map[key]int64, len(serverCalls))
	for _, c := range serverCalls {
		distinct[key{server: c.Server, via: c.Via}] = c.Calls
	}
	type agg struct {
		row   analytics.DiscoveredMCPServer
		tools map[string]struct{}
		keys  map[string]struct{}
	}
	rows := map[key]*agg{}
	for _, u := range usage {
		k := key{server: u.Server}
		tool, registered := u.Tool, slugs[strings.ToLower(u.Server)]
		// "<slug>__<tool>": routed through the gateway's MCP plane.
		if head, rest, ok := strings.Cut(u.Tool, "__"); ok && rest != "" {
			if slug, isConn := slugs[strings.ToLower(head)]; isConn {
				k = key{server: slug, via: true}
				tool, registered = rest, slug
			}
		}
		a := rows[k]
		if a == nil {
			a = &agg{tools: map[string]struct{}{}, keys: map[string]struct{}{}}
			a.row = analytics.DiscoveredMCPServer{Server: k.server, RegisteredConnectorSlug: registered, ViaGateway: k.via}
			rows[k] = a
		}
		a.tools[tool] = struct{}{}
		for _, id := range u.Keys {
			a.keys[id] = struct{}{}
		}
		if u.Calls > a.row.Calls {
			a.row.Calls = u.Calls
		}
		if u.LastSeen.After(a.row.LastSeen) {
			a.row.LastSeen = u.LastSeen
		}
	}
	out := make([]analytics.DiscoveredMCPServer, 0, len(rows))
	for k, a := range rows {
		if n, ok := distinct[k]; ok {
			a.row.Calls = n
		}
		a.row.Tools = make([]string, 0, len(a.tools))
		for t := range a.tools {
			a.row.Tools = append(a.row.Tools, t)
		}
		sort.Strings(a.row.Tools)
		a.row.UsedBy = int64(len(a.keys))
		out = append(out, a.row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return !out[i].ViaGateway && out[j].ViaGateway
	})
	return out
}

// parsePeriod reads a dashboard window: ?from=&to= (YYYY-MM-DD, whole UTC
// days, labeled RangeCustom) when either is set, else ?range= (default
// def). It writes a 400 on a bad value.
func parsePeriod(w http.ResponseWriter, r *http.Request, def analytics.Range) (analytics.Range, analytics.Period, bool) {
	q := r.URL.Query()
	if q.Has("from") || q.Has("to") {
		p, err := analytics.ParseDateRange(q.Get("from"), q.Get("to"), time.Now())
		if err != nil {
			httpx.ValidationError(w, err.Error())
			return "", analytics.Period{}, false
		}
		return analytics.RangeCustom, p, true
	}
	rng := def
	if raw := q.Get("range"); raw != "" {
		var ok bool
		if rng, ok = analytics.ParseRange(raw); !ok {
			httpx.ValidationError(w, `range must be one of "24h", "7d", "30d"`)
			return "", analytics.Period{}, false
		}
	}
	return rng, rng.Period(time.Now()), true
}

// ClientModelSankey handles GET
// /api/v1/analytics/client-models?range=24h|7d|30d&metric=calls|tokens|cost&limit=&client_name=:
// a three-column CLIENT -> MODEL -> PROVIDER usage graph built from
// llm_calls, in a node/link shape any Sankey chart library (nivo,
// recharts) can render directly. Defaults to 7d (like Models/Skills)
// and metric=calls.
func (h Analytics) ClientModelSankey(w http.ResponseWriter, r *http.Request) {
	if h.Analytics == nil {
		httpx.NotFound(w, analyticsUnavailableMessage)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	q, ok := parseSankeyQuery(w, r)
	if !ok {
		return
	}
	sankey, err := h.Analytics.ClientModelSankey(r.Context(), tid, q)
	if err != nil {
		writeAnalyticsErr(w, r, "compute client-model sankey", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sankey)
}

// TrafficFlow handles GET
// /api/v1/analytics/traffic-flow?range=24h|7d|30d&metric=calls|tokens|cost&limit=&client_name=:
// the CLIENT -> PATH -> MODEL | CONNECTOR graph joining both planes, in
// the same node/link shape as ClientModelSankey. Same parameters and
// defaults (7d, calls, limit 10); limit folds the tail of each terminal
// column separately.
func (h Analytics) TrafficFlow(w http.ResponseWriter, r *http.Request) {
	if h.Analytics == nil {
		httpx.NotFound(w, analyticsUnavailableMessage)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	q, ok := parseSankeyQuery(w, r)
	if !ok {
		return
	}
	flow, err := h.Analytics.TrafficFlowSankey(r.Context(), tid, q)
	if err != nil {
		writeAnalyticsErr(w, r, "compute traffic-flow sankey", err)
		return
	}
	h.resolveFlowConnectorLabels(r, tid, flow)
	httpx.WriteJSON(w, http.StatusOK, flow)
}

// resolveFlowConnectorLabels swaps each CONNECTOR node's Label from the
// connector id the ClickHouse Reader knows to the connector's display
// name, the same way resolveConnectorNames does for Overview's
// TopConnectors. Only Label changes: Key (and so ID) stays the connector
// id, which is what links reference and what the console deep-links on.
// Best effort -- a store error, or an id whose connector was deleted
// since, leaves the id as the label rather than dropping the node.
func (h Analytics) resolveFlowConnectorLabels(r *http.Request, tenantID string, flow *analytics.TrafficFlow) {
	if h.Store == nil || flow == nil || len(flow.Nodes) == 0 {
		return
	}
	conns, err := h.Store.Connectors().List(r.Context(), tenantID)
	if err != nil {
		return
	}
	names := make(map[string]string, len(conns))
	for _, c := range conns {
		names[c.ID] = c.Name
	}
	for i := range flow.Nodes {
		if flow.Nodes[i].Type != analytics.SankeyNodeConnector {
			continue
		}
		if n, ok := names[flow.Nodes[i].Key]; ok {
			flow.Nodes[i].Label = n
		}
	}
}

// parseSankeyQuery reads the range/metric/limit/client_name parameters
// shared by ClientModelSankey and TrafficFlow, writing a 400 and returning
// ok=false on the first invalid one.
func parseSankeyQuery(w http.ResponseWriter, r *http.Request) (analytics.SankeyQuery, bool) {
	q := r.URL.Query()

	rng, period, ok := parsePeriod(w, r, analytics.Range7d)
	if !ok {
		return analytics.SankeyQuery{}, false
	}

	rawMetric := q.Get("metric")
	if rawMetric == "" {
		rawMetric = string(analytics.SankeyMetricCalls)
	}
	metric, ok := analytics.ParseSankeyMetric(rawMetric)
	if !ok {
		httpx.ValidationError(w, `metric must be one of "calls", "tokens", "cost"`)
		return analytics.SankeyQuery{}, false
	}

	limit := analytics.SankeyDefaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			httpx.ValidationError(w, "limit must be a positive integer")
			return analytics.SankeyQuery{}, false
		}
		limit = n
	}

	return analytics.SankeyQuery{
		Range:      rng,
		Period:     period,
		Metric:     metric,
		Limit:      limit,
		ClientName: q.Get("client_name"),
	}, true
}

// Logs handles GET /api/v1/analytics/logs: a paginated, filtered list of
// access-log rows without bodies.
func (h Analytics) Logs(w http.ResponseWriter, r *http.Request) {
	if h.Analytics == nil {
		httpx.NotFound(w, analyticsUnavailableMessage)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	page := httpx.ParsePagination(r)
	q := r.URL.Query()
	status, ok := parseOptionalInt(w, q.Get("status"))
	if !ok {
		return
	}
	from, ok := parseOptionalTime(w, q.Get("from"))
	if !ok {
		return
	}
	to, ok := parseOptionalTime(w, q.Get("to"))
	if !ok {
		return
	}

	items, total, err := h.Analytics.ListAccessLogs(r.Context(), tid, analytics.AccessLogFilter{
		Limit: page.Limit, Offset: page.Offset,
		Method:      q.Get("method"),
		ConnectorID: q.Get("connector_id"),
		ToolName:    q.Get("tool"),
		SessionID:   q.Get("session_id"),
		Principal:   q.Get("principal"),
		Status:      status,
		Source:      q.Get("source"),
		User:        q.Get("user"),
		From:        from,
		To:          to,
	})
	if err != nil {
		writeAnalyticsErr(w, r, "list access logs", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: items, Total: total})
}

// LogsGet handles GET /api/v1/analytics/logs/{request_id}: one access-log
// row including bodies/headers. Admin-only (see Permission in
// internal/api/router.go's routeSpec).
func (h Analytics) LogsGet(w http.ResponseWriter, r *http.Request) {
	if h.Analytics == nil {
		httpx.NotFound(w, analyticsUnavailableMessage)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	rec, err := h.Analytics.GetAccessLog(r.Context(), tid, chi.URLParam(r, "request_id"))
	if err != nil {
		writeAnalyticsErr(w, r, "get access log", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rec)
}

// llmCalls is the LLM-logs source: the ClickHouse reader when configured,
// else the Postgres capture store (Deps.LLMCalls), else nil (404).
func (h Analytics) llmCalls() analytics.LLMCallReader {
	if h.Analytics != nil {
		return h.Analytics
	}
	return h.LLMCalls
}

// LLMLogs handles GET /api/v1/analytics/llm-logs: a paginated, filtered
// list of LLM-call rows without bodies.
func (h Analytics) LLMLogs(w http.ResponseWriter, r *http.Request) {
	calls := h.llmCalls()
	if calls == nil {
		httpx.NotFound(w, analyticsUnavailableMessage)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	page := httpx.ParsePagination(r)
	q := r.URL.Query()
	status, ok := parseOptionalInt(w, q.Get("status"))
	if !ok {
		return
	}
	from, ok := parseOptionalTime(w, q.Get("from"))
	if !ok {
		return
	}
	to, ok := parseOptionalTime(w, q.Get("to"))
	if !ok {
		return
	}

	items, total, err := calls.ListLLMCalls(r.Context(), tid, analytics.LLMCallFilter{
		Limit: page.Limit, Offset: page.Offset,
		Model:      q.Get("model"),
		SessionID:  q.Get("session_id"),
		Principal:  q.Get("principal"),
		ClientName: q.Get("client_name"),
		Status:     status,
		Source:     q.Get("source"),
		User:       q.Get("user"),
		From:       from,
		To:         to,
	})
	if err != nil {
		writeAnalyticsErr(w, r, "list llm calls", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: items, Total: total})
}

// LLMLogsGet handles GET /api/v1/analytics/llm-logs/{request_id}: one
// LLM-call row including bodies/messages/system/tools. Admin-only.
func (h Analytics) LLMLogsGet(w http.ResponseWriter, r *http.Request) {
	calls := h.llmCalls()
	if calls == nil {
		httpx.NotFound(w, analyticsUnavailableMessage)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	rec, err := calls.GetLLMCall(r.Context(), tid, chi.URLParam(r, "request_id"))
	if err != nil {
		writeAnalyticsErr(w, r, "get llm call", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.resolveBodies(r, rec))
}

// SessionTimeline handles GET
// /api/v1/analytics/sessions/{session_id}/timeline: the merged,
// MCP+LLM event timeline for one gateway session, ordered by (ts, id) per the order query parameter, computed
// server-side under the ownership rule (see pkg/analytics.SessionTimeline
// and docs/observability.md#session-timeline-ownership) so a caller-claimed
// session tag can never pull another key's calls into it. An unknown or
// never-used session id is not a 404 -- it is a valid, empty timeline.
func (h Analytics) SessionTimeline(w http.ResponseWriter, r *http.Request) {
	if h.Analytics == nil {
		httpx.NotFound(w, analyticsUnavailableMessage)
		return
	}
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	if sessionID == "" {
		httpx.ValidationError(w, "session_id is required")
		return
	}

	// order=asc|desc, default asc. The sort is the Reader's job, by
	// (ts, id); clients must not re-sort.
	order, ok := analytics.ParseTimelineOrder(r.URL.Query().Get("order"))
	if !ok {
		httpx.ValidationError(w, "order must be asc or desc")
		return
	}

	tl, err := h.Analytics.SessionTimeline(r.Context(), tid, sessionID, analytics.TimelineOptions{Order: order})
	if err != nil {
		writeAnalyticsErr(w, r, "compute session timeline", err)
		return
	}
	tl.Order = order
	httpx.WriteJSON(w, http.StatusOK, tl)
}

// llmCallDetail is the LLM-log detail response: the stored row plus, when
// its bodies were offloaded but could not be fetched back, why.
type llmCallDetail struct {
	*sink.LLMCall
	BodiesUnavailable string `json:"bodies_unavailable,omitempty"`
}

// resolveBodies fills an offloaded row's request_body/response_body from
// the BodyStore, so the console reads the same fields whether bodies were
// stored inline or offloaded. A store miss or failure degrades to the row
// without bodies (the metadata is still worth showing) plus a reason.
func (h Analytics) resolveBodies(r *http.Request, rec *sink.LLMCall) llmCallDetail {
	out := llmCallDetail{LLMCall: rec}
	if rec.BodyRef == "" {
		return out
	}
	if h.BodyStore == nil {
		out.BodiesUnavailable = "body store not configured on this instance"
		return out
	}
	req, resp, err := h.BodyStore.Get(r.Context(), rec.BodyRef)
	switch {
	case err == nil:
		rec.RequestBody, rec.ResponseBody = req, resp
	case errors.Is(err, sink.ErrBodyNotFound):
		out.BodiesUnavailable = "bodies not found in the body store (expired or deleted)"
	default:
		applog.From(r.Context()).Error("resolve llm call bodies failed", "body_ref", rec.BodyRef, "error", err)
		out.BodiesUnavailable = "body store read failed"
	}
	return out
}

// writeAnalyticsErr maps a pkg/analytics error onto the shared HTTP error
// envelope: analytics.ErrNotFound -> 404, anything else -> 500 (logged
// server-side, never echoed to the caller) -- the same shape
// writeStoreErr (errors.go) gives store-layer errors.
func writeAnalyticsErr(w http.ResponseWriter, r *http.Request, action string, err error) {
	if err == analytics.ErrNotFound {
		httpx.NotFound(w, "not found")
		return
	}
	applog.From(r.Context()).Error(action+" failed", "error", err)
	httpx.Internal(w, "internal error")
}

// parseOptionalInt parses raw as an int when non-empty; "" means
// unfiltered (0). Writes a 400 and returns ok=false on a malformed
// value.
func parseOptionalInt(w http.ResponseWriter, raw string) (int, bool) {
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		httpx.ValidationError(w, "status must be an integer")
		return 0, false
	}
	return n, true
}

// parseOptionalTime parses raw as RFC3339 when non-empty; "" means
// unbounded (zero time). Writes a 400 and returns ok=false on a
// malformed value.
func parseOptionalTime(w http.ResponseWriter, raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		httpx.ValidationError(w, "from/to must be RFC3339 timestamps, e.g. 2026-01-02T15:04:05Z")
		return time.Time{}, false
	}
	return t, true
}
