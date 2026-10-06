// Package analytics declares the read-side seam for the gateway's
// analytics API (internal/api/handlers/analytics.go): the query shapes it
// accepts and the Reader interface it depends on.
//
// The control-plane API never imports pkg/sink/clickhouse directly -- it
// only knows Reader, satisfied today by that package's Sink and,
// potentially, by any other store that wants to back GET
// /api/v1/analytics/*. That is why this lives under pkg/ rather than
// internal/: pkg/ is the only part of this module a separate module (a
// private plugin, or a future store) can import at all. It mirrors
// pkg/ops's ConnectorOps/CacheOps seam.
//
// Every Reader method is tenant-scoped: callers pass tenantID explicitly
// and implementations must filter every query by it, the same invariant
// every store.Store method upholds.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// ErrNotFound is returned by Get* methods when no row matches the given
// tenant and request id.
var ErrNotFound = errors.New("analytics: not found")

// PricingSource values for Overview.PricingSource: which rate card the
// running gateway instance costs LLM calls from.
const (
	PricingSourceEmbedded = "embedded" // the rate card built into the binary (pkg/pricing/prices.json)
	PricingSourceFile     = "file"     // llm_proxy.pricing_file was loaded and merged over the embedded card
)

// Meta carries request-independent facts about the running gateway
// instance that the Overview handler stamps onto every response, alongside
// (not through) the Reader seam: which pricing source is configured. It is
// NOT part of analytics.Reader — Reader implementations (pkg/sink/clickhouse)
// know nothing about the LLM plane's config; internal/api/handlers.Analytics
// sets these fields on the Overview struct itself after calling Reader.Overview.
type Meta struct {
	// PricingSource is one of the PricingSource* constants above.
	PricingSource string
}

// Range is one of the Overview page's supported time windows.
type Range string

const (
	Range24h Range = "24h"
	Range7d  Range = "7d"
	Range30d Range = "30d"
)

// ParseRange validates a "range" query parameter. ok is false for anything
// other than the three supported values.
func ParseRange(s string) (r Range, ok bool) {
	switch Range(s) {
	case Range24h, Range7d, Range30d:
		return Range(s), true
	default:
		return "", false
	}
}

// Window returns r's duration.
func (r Range) Window() time.Duration {
	switch r {
	case Range7d:
		return 7 * 24 * time.Hour
	case Range30d:
		return 30 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// RangeCustom labels a window given as explicit dates (from/to) instead of
// a preset.
const RangeCustom Range = "custom"

// Granularity is the bucket size of a usage-over-time series.
type Granularity string

const (
	GranularityHour Granularity = "hour"
	GranularityDay  Granularity = "day"
)

// Step is g's duration.
func (g Granularity) Step() time.Duration {
	if g == GranularityHour {
		return time.Hour
	}
	return 24 * time.Hour
}

// Period is a resolved time window: usage is counted in [Start, End) and
// compared with [PrevStart, PrevEnd), the window of the same length just
// before it. Series are hourly for windows up to 48h, daily beyond. UTC.
type Period struct {
	Start, End, PrevStart, PrevEnd time.Time
	Granularity                    Granularity
}

// NewPeriod is the Period [start, end).
func NewPeriod(start, end time.Time) Period {
	start, end = start.UTC(), end.UTC()
	span := end.Sub(start)
	g := GranularityDay
	if span <= 48*time.Hour {
		g = GranularityHour
	}
	return Period{Start: start, End: end, PrevStart: start.Add(-span), PrevEnd: start, Granularity: g}
}

// Period resolves r at now: the window of r's length ending now.
func (r Range) Period(now time.Time) Period { return NewPeriod(now.Add(-r.Window()), now) }

// MaxCustomRange bounds a from/to window, keeping its queries cheap.
const MaxCustomRange = 366 * 24 * time.Hour

// ParseDateRange resolves a custom window from two YYYY-MM-DD dates: whole
// UTC days, both inclusive, ending no later than now.
func ParseDateRange(from, to string, now time.Time) (Period, error) {
	start, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return Period{}, fmt.Errorf("from must be a date (YYYY-MM-DD)")
	}
	last, err := time.Parse(time.DateOnly, to)
	if err != nil {
		return Period{}, fmt.Errorf("to must be a date (YYYY-MM-DD)")
	}
	end := last.Add(24 * time.Hour)
	switch {
	case last.Before(start):
		return Period{}, fmt.Errorf("to must not be before from")
	case !start.Before(now):
		return Period{}, fmt.Errorf("from must not be in the future")
	case end.Sub(start) > MaxCustomRange:
		return Period{}, fmt.Errorf("a custom range spans at most 366 days")
	}
	if end.After(now) {
		end = now
	}
	return NewPeriod(start, end), nil
}

// AccessLogFilter narrows GET /analytics/logs. Zero values mean
// "unfiltered" for every field; Limit <= 0 means the caller's default.
type AccessLogFilter struct {
	Limit, Offset                                       int
	Method, ConnectorID, ToolName, SessionID, Principal string
	// Status filters on an exact HTTP status code; 0 means unfiltered.
	Status int
	// Source filters on sink.AccessLog.Source ("gateway" | "interceptor").
	// "" means unfiltered (every source).
	Source string
	// User filters on sink.AccessLog.User, the ingested row's self-reported
	// caller identity. "" means unfiltered.
	User     string
	From, To time.Time // zero = unbounded on that side
}

// LLMCallFilter narrows GET /analytics/llm-logs. Same zero-value
// conventions as AccessLogFilter.
type LLMCallFilter struct {
	Limit, Offset               int
	Model, SessionID, Principal string
	// ClientName filters on sink.LLMCall.ClientName (sink.ClientFamily's
	// output): "claude-code", "cursor", "vscode", "codex", "claude-desktop",
	// or "other". "" means unfiltered.
	ClientName string
	Status     int
	// Source filters on sink.LLMCall.Source ("gateway" | "interceptor").
	// "" means unfiltered (every source).
	Source string
	// User filters on sink.LLMCall.User, the ingested row's self-reported
	// caller identity. "" means unfiltered.
	User     string
	From, To time.Time
}

// LLMCallReader is the LLM-logs subset of Reader. The Postgres capture
// store (pkg/sink/postgres) implements it, so LLM Logs work without
// ClickHouse; the full Reader still needs the ClickHouse sink.
type LLMCallReader interface {
	ListLLMCalls(ctx context.Context, tenantID string, f LLMCallFilter) ([]sink.LLMCall, int, error)
	GetLLMCall(ctx context.Context, tenantID, requestID string) (*sink.LLMCall, error)
}

// Reader is the read side of the analytics API: everything
// internal/api/handlers.Analytics needs, satisfied by
// pkg/sink/clickhouse.Sink. A Reader that is not configured (no
// ClickHouse sink) is represented by a nil Reader; handlers answer 404
// rather than call through a nil.
type Reader interface {
	// Overview computes the Overview page's metrics for tenantID over p,
	// including deltas against p's previous period (p.PrevStart..PrevEnd).
	// The caller sets the result's Range.
	Overview(ctx context.Context, tenantID string, p Period) (*Overview, error)

	// ListAccessLogs returns a page of access-log rows (bodies/headers
	// never populated -- this is the "logs" list, not the "get one"
	// route) matching f, and the total number of matching rows
	// (ignoring Limit/Offset).
	ListAccessLogs(ctx context.Context, tenantID string, f AccessLogFilter) ([]sink.AccessLog, int, error)
	// GetAccessLog returns one access-log row, including bodies/headers,
	// by request id. ErrNotFound if no row matches.
	GetAccessLog(ctx context.Context, tenantID, requestID string) (*sink.AccessLog, error)

	// ListLLMCalls mirrors ListAccessLogs for the LLM plane.
	ListLLMCalls(ctx context.Context, tenantID string, f LLMCallFilter) ([]sink.LLMCall, int, error)
	// GetLLMCall mirrors GetAccessLog for the LLM plane.
	GetLLMCall(ctx context.Context, tenantID, requestID string) (*sink.LLMCall, error)

	// ModelsSummary computes GET /api/v1/analytics/models's per-model
	// usage summary for tenantID over r (default 7d -- see the handler).
	ModelsSummary(ctx context.Context, tenantID string, r Range) (*ModelsSummary, error)

	// SkillsSummary computes GET /api/v1/analytics/skills's per
	// skill/command usage summary for tenantID over p, from mcp_access_logs
	// rows carrying a non-empty skill_name. Kind is left "" on every row --
	// the handler joins it in from the skill/command registry by name, and
	// sets Range.
	SkillsSummary(ctx context.Context, tenantID string, p Period) (*SkillsSummary, error)

	// SessionTimeline computes GET
	// /api/v1/analytics/sessions/{session_id}/timeline's merged,
	// time-ordered MCP+LLM event timeline for one gateway session, under
	// the ownership rule documented at
	// docs/observability.md#session-timeline-ownership: an LLM-plane event
	// or an MCP-plane event recorded under any key other than the
	// session's owning key is excluded, never trusted from a
	// caller-supplied tag alone. An unknown or never-used sessionID is not
	// an error -- it returns a zero-Events timeline, not ErrNotFound.
	//
	// opts.Order picks the direction events are returned in (the zero
	// value means ascending); stats and ownership never depend on it.
	SessionTimeline(ctx context.Context, tenantID, sessionID string, opts TimelineOptions) (*SessionTimeline, error)

	// ClientModelSankey computes GET /api/v1/analytics/client-models's
	// three-column CLIENT -> MODEL -> PROVIDER usage graph for tenantID,
	// aggregating llm_calls by client_name, requested_model and vendor
	// over q.Range. See the Sankey doc comment for the response shape.
	ClientModelSankey(ctx context.Context, tenantID string, q SankeyQuery) (*Sankey, error)

	// TrafficFlowSankey computes GET /api/v1/analytics/traffic-flow's
	// CLIENT -> PATH -> MODEL | CONNECTOR graph for tenantID: llm_calls
	// (by client_name and requested model) on the LLM branch and
	// tools/call mcp_access_logs rows (by User-Agent family and
	// connector_id) on the MCP branch, over q.Range. q.Limit folds the
	// tail of each terminal column. See TrafficFlow for the metric rules.
	TrafficFlowSankey(ctx context.Context, tenantID string, q SankeyQuery) (*TrafficFlow, error)

	// SkillUsage returns, per skill name, how many LLM calls' responses
	// asked to use it over p (llm_calls.skills_used; see internal/discovery).
	// Names only: registry state (registered, kind) is joined in by the
	// handler. Ordered by Calls descending, name.
	SkillUsage(ctx context.Context, tenantID string, p Period) ([]SkillUsage, error)

	// MCPToolUsage returns, per (server, tool) the model's responses asked
	// to call over p (llm_calls.mcp_tools_used). The handler folds these
	// into per-server rows and joins connector registration.
	MCPToolUsage(ctx context.Context, tenantID string, p Period) ([]MCPToolUsage, error)

	// MCPServerCalls returns, per server row, the number of DISTINCT LLM calls
	// (request_id) that used at least one of its tools over p, so a call using
	// two tools of one server counts once. aliases maps a lowercased connector
	// slug or name to its canonical slug; a tool named "<alias>__<tool>" is
	// attributed to that connector with Via=true (the gateway's MCP plane),
	// whatever server alias the client gave the gateway. Entries are keyed the
	// same way the handler folds MCPToolUsage rows: (Server, Via).
	MCPServerCalls(ctx context.Context, tenantID string, p Period, aliases map[string]string) ([]MCPServerCalls, error)
}

// MCPServerCalls is the Reader's per-server-row distinct-LLM-call count. For a
// Via row Server is the canonical connector slug.
type MCPServerCalls struct {
	Server string
	Via    bool
	Calls  int64
}

/* ---------------------------------------------------------------------- */
/* Discovered skills and MCP servers                                      */
/* ---------------------------------------------------------------------- */

// SkillUsage is the Reader's raw per-skill usage (no registry join).
type SkillUsage struct {
	Name     string
	Calls    int64
	UsedBy   int64 // distinct key_id
	LastSeen time.Time
}

// MCPToolUsage is the Reader's raw per-(server, tool) usage. Server is the
// lowercased name from the client's mcp__<server>__<tool> tool name; Tool is
// everything after the first "__" (so a tool reached through the gateway's own
// MCP plane reads "<connector-slug>__<tool>").
type MCPToolUsage struct {
	Server   string
	Tool     string
	Calls    int64
	Keys     []string // distinct key_id
	LastSeen time.Time
}

// DiscoveredSkill is one row of GET /api/v1/analytics/skills/usage: a skill
// the model was observed using in LLM traffic, registered or not.
type DiscoveredSkill struct {
	Name     string    `json:"name"`
	Calls    int64     `json:"calls"`
	UsedBy   int64     `json:"used_by"`
	LastSeen time.Time `json:"last_seen"`
	// Registered is true when a tenant or platform skill/command of this
	// name exists in the registry (compared case-insensitively).
	Registered bool `json:"registered"`
	// Kind is "skill" or "command" from the registry, "" when unregistered.
	Kind string `json:"kind"`
}

// DiscoveredSkills is GET /api/v1/analytics/skills/usage's response body.
type DiscoveredSkills struct {
	Range  Range             `json:"range"`
	Skills []DiscoveredSkill `json:"skills"`
}

// DiscoveredMCPServer is one row of GET /api/v1/analytics/mcps/usage.
type DiscoveredMCPServer struct {
	// Server is the MCP server name the client used. When ViaGateway, it is
	// the registered connector's slug the call was attributed to.
	Server string   `json:"server"`
	Tools  []string `json:"tools"`
	// Calls sums the per-tool call counts (an LLM call that used two tools
	// of one server counts twice).
	Calls    int64     `json:"calls"`
	UsedBy   int64     `json:"used_by"`
	LastSeen time.Time `json:"last_seen"`
	// RegisteredConnectorSlug is the tenant connector whose slug or name
	// matches Server (case-insensitive); "" when none does.
	RegisteredConnectorSlug string `json:"registered_connector_slug"`
	// ViaGateway is true when the tool part looked like
	// "<registered-connector-slug>__<tool>": the call went through this
	// gateway's MCP plane, whose tools are named connector__tool.
	ViaGateway bool `json:"via_gateway"`
}

// DiscoveredMCPServers is GET /api/v1/analytics/mcps/usage's response body.
type DiscoveredMCPServers struct {
	Range   Range                 `json:"range"`
	Servers []DiscoveredMCPServer `json:"servers"`
}

/* ---------------------------------------------------------------------- */
/* Overview wire shape -- mirrors web/src/lib/overview.ts's OverviewMetrics */
/* exactly (field names included): the handler marshals this struct       */
/* straight to JSON, so its json tags ARE the contract, not internal      */
/* naming convention.                                                     */
/* ---------------------------------------------------------------------- */

// KpiDelta is a signed percent change versus the previous window.
type KpiDelta struct {
	Pct       float64 `json:"pct"`
	Direction string  `json:"direction"` // "up" | "down"
}

// CountKpi is one KPI tile: a value plus its delta against the previous
// window. Value is a float64 (not int64) because it also carries the
// sub-dollar totalCost tile; count tiles hold whole numbers and marshal
// as integers. Mirrors web/src/lib/overview.ts CountKpi (value: number).
type CountKpi struct {
	Value float64  `json:"value"`
	Delta KpiDelta `json:"delta"`
}

// SuccessRateKpi is the "successRate" KPI tile: a percentage plus a
// human-readable "N / M responses" sub-label.
type SuccessRateKpi struct {
	Value float64 `json:"value"`
	Sub   string  `json:"sub"`
}

// OverviewKpis is the Overview page's top KPI row.
type OverviewKpis struct {
	LlmAgentCalls CountKpi       `json:"llmAgentCalls"`
	McpToolCalls  CountKpi       `json:"mcpToolCalls"`
	TotalTokens   CountKpi       `json:"totalTokens"`
	TotalCost     CountKpi       `json:"totalCost"`
	SuccessRate   SuccessRateKpi `json:"successRate"`
}

// LlmUsageRow is one model's row in the LLM usage table. Cost is the sum
// of the frozen-at-write cost_usd (pkg/pricing) for that model in the
// window; unpriced/unknown models contribute 0.
type LlmUsageRow struct {
	Model  string  `json:"model"`
	Calls  int64   `json:"calls"`
	Tokens int64   `json:"tokens"`
	Cost   float64 `json:"cost"`
}

// NamedCount is a generic (name, count) ranking row: top profiles
// ("topAgents"), mcp tools, top connectors, requests by client.
type NamedCount struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// StatusCodeSlice is one wedge of the status-code breakdown, bucketed by
// outcome (see the Outcome* constants) rather than by raw HTTP status:
// every MCP call answers HTTP 200 whether or not the JSON-RPC call
// itself failed, so a raw "2xx"/"4xx"/"5xx" split would show 100% "2xx"
// even when most calls carried a JSON-RPC error. Bucketing by outcome is
// what makes this chart agree with Overview.SuccessPct.
type StatusCodeSlice struct {
	Label string  `json:"label"`
	Pct   float64 `json:"pct"`
}

// Outcome bucket labels for StatusCodeSlice.Label, and the rule each one
// applies to a row (evaluated in this order -- the first match wins):
//
//   - OutcomeNotification: status_code = 204 (a JSON-RPC notification,
//     which carries no response body and so no success/failure verdict
//     of its own -- see mcpflow's "notifications → 204").
//   - OutcomeError: error_code (llm_calls: error) is non-empty, or
//     status_code >= 400.
//   - OutcomeSuccess: everything else.
//
// Overview.SuccessPct is defined as OutcomeSuccess's share of every row
// that is NOT OutcomeNotification -- notifications are excluded from
// both the numerator and the denominator, not counted as failures.
const (
	OutcomeSuccess      = "200"
	OutcomeNotification = "204"
	OutcomeError        = "error"
)

// SlowCall is one entry in the "slowest calls" list.
type SlowCall struct {
	Name string `json:"name"`
	MS   int64  `json:"ms"`
}

// Latency is the Overview page's latency panel.
type Latency struct {
	MedianMS float64    `json:"medianMs"`
	P95MS    float64    `json:"p95Ms"`
	Slowest  []SlowCall `json:"slowest"`
}

// TrafficPoint is one bucket of the traffic-over-time chart.
type TrafficPoint struct {
	Label    string `json:"label"`
	LlmCalls int64  `json:"llmCalls"`
	McpCalls int64  `json:"mcpCalls"`
}

// Traffic is the raw MCP-vs-LLM call split for the window.
type Traffic struct {
	LlmCalls int64 `json:"llmCalls"`
	McpCalls int64 `json:"mcpCalls"`
}

/* ---------------------------------------------------------------------- */
/* Models summary -- GET /api/v1/analytics/models. Field names are plain    */
/* snake_case (unlike Overview's camelCase, which mirrors an older TS      */
/* interface): this route is new with Phase 2 of the cross-model plan and   */
/* follows the gateway's normal REST convention (see sink.LLMCall,          */
/* Connector, etc.) instead of that one.                                    */
/* ---------------------------------------------------------------------- */

// ModelSummaryRow is one model's row in GET /api/v1/analytics/models,
// aggregated from llm_calls: one row per model name the client asked for.
type ModelSummaryRow struct {
	// Name is the requested model name (llm_calls.requested_model): a
	// registry name, or a vendor model id for an unregistered call.
	Name string `json:"name"`
	// Provider is the vendor that served most of the name's calls
	// (resolved_vendor), or the client's dialect when no call of the name
	// went through the registry.
	Provider string `json:"provider"`
	Calls    int64  `json:"calls"`
	// Tokens is input + output + cache (read + creation) tokens summed
	// across every call in the window.
	Tokens int64 `json:"tokens"`
	// CostUSD is the sum of the frozen-at-write cost_usd (pkg/pricing)
	// across every call in the window; nil when none of them had a known
	// price, mirroring sink.LLMCall.CostUSD's own nullability rather than
	// reporting a fabricated 0.
	CostUSD *float64 `json:"cost_usd"`
	// UsedBy is the number of distinct API keys (key_id) that called this
	// model in the window.
	UsedBy int64 `json:"used_by"`
	// LastSeen is the most recent call's timestamp.
	LastSeen time.Time `json:"last_seen"`
	// Status is always "active": every row here comes from observed
	// gateway traffic. The console merges in registry-registered models
	// with no traffic as "registered" client-side (GET /api/v1/models,
	// Phase 1) -- this Reader knows nothing about the registry.
	Status string `json:"status"`
}

// ModelHighestTraffic names the single highest-token model in a
// ModelsSummary's window.
type ModelHighestTraffic struct {
	Name   string `json:"name"`
	Tokens int64  `json:"tokens"`
}

// ModelsSummary is GET /api/v1/analytics/models?range=7d's response body.
type ModelsSummary struct {
	Range Range `json:"range"`
	// Models is ordered by Tokens descending.
	Models      []ModelSummaryRow `json:"models"`
	TotalModels int               `json:"total_models"`
	// HighestTraffic is nil when Models is empty.
	HighestTraffic *ModelHighestTraffic `json:"highest_traffic"`
}

/* ---------------------------------------------------------------------- */
/* Skills summary -- GET /api/v1/analytics/skills. Phase 5 of "skills &     */
/* commands on agent profiles" (see SKILLS-CONTRACT.md); field names follow */
/* ModelSummaryRow's snake_case convention, not Overview's camelCase.       */
/* ---------------------------------------------------------------------- */

// SkillSummaryRow is one skill or command's row in GET
// /api/v1/analytics/skills, aggregated from mcp_access_logs: one row per
// distinct skill_name a gateway__skill tools/call or a native command's
// prompts/get carried.
type SkillSummaryRow struct {
	// Name is the skill/command's name (mcp_access_logs.skill_name).
	Name string `json:"name"`
	// Kind is "skill" or "command", joined in from the registry
	// (internal/api/handlers.Analytics.Skills, not this package) by
	// Name -- this Reader knows nothing about the registry, only
	// observed traffic. A name that no longer resolves to a registry row
	// (the skill/command was since deleted) is still listed, with Kind
	// left "" rather than omitted: the traffic happened.
	Kind  string `json:"kind"`
	Calls int64  `json:"calls"`
	// UsedBy is the number of distinct API keys (key_id) that loaded
	// this skill or rendered this command in the window.
	UsedBy int64 `json:"used_by"`
	// LastSeen is the most recent call's timestamp.
	LastSeen time.Time `json:"last_seen"`
}

// SkillMostUsed names the single highest-Calls skill/command in a
// SkillsSummary's window, for the console's "Most used" tile.
type SkillMostUsed struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Calls int64  `json:"calls"`
}

// SkillsSummary is GET /api/v1/analytics/skills?range=7d's response body.
type SkillsSummary struct {
	Range Range `json:"range"`
	// Skills is ordered by Calls descending.
	Skills      []SkillSummaryRow `json:"skills"`
	TotalSkills int               `json:"total_skills"`
	// MostUsed is nil when Skills is empty.
	MostUsed *SkillMostUsed `json:"most_used"`
}

// Overview is GET /api/v1/analytics/overview's response body, matching
// web/src/lib/overview.ts's OverviewMetrics field for field.
type Overview struct {
	Range Range        `json:"range"`
	Kpis  OverviewKpis `json:"kpis"`
	// LlmUsage is per-model token/call counts, cost always 0 (see
	// LlmUsageRow).
	LlmUsage []LlmUsageRow `json:"llmUsage"`
	Traffic  Traffic       `json:"traffic"`

	// CostEstimated is always true today: every dollar figure in this
	// response (Kpis.TotalCost, LlmUsage[].Cost) comes from pkg/pricing's
	// rate card, computed at capture time from token counts, not from a
	// provider-reported invoice line. It will only be false once a
	// provider-reported cost is ingested for at least part of the window.
	// Set by the handler, not by any Reader implementation.
	CostEstimated bool `json:"costEstimated"`
	// PricingSource is "embedded" or "file" (see the PricingSource*
	// constants): which rate card the running gateway instance is
	// costing calls from. Set by the handler from analytics.Meta, not by
	// any Reader implementation.
	PricingSource string `json:"pricingSource"`
	// TopAgents holds the top agent-profile ranking (the TS field name
	// predates the "profile" terminology; the data source is
	// AccessLog.Profile, populated from X-Agent-Profile-Name).
	TopAgents        []NamedCount      `json:"topAgents"`
	StatusCodes      []StatusCodeSlice `json:"statusCodes"`
	SuccessPct       float64           `json:"successPct"`
	Latency          Latency           `json:"latency"`
	RequestsByClient []NamedCount      `json:"requestsByClient"`
	McpTools         []NamedCount      `json:"mcpTools"`
	TopConnectors    []NamedCount      `json:"topConnectors"`
	TrafficOverTime  []TrafficPoint    `json:"trafficOverTime"`
}

/* ---------------------------------------------------------------------- */
/* Session timeline -- GET /api/v1/analytics/sessions/{session_id}/timeline */
/* ---------------------------------------------------------------------- */

// Timeline event status buckets (TimelineEvent.Status), mirroring the
// Outcome* convention above: "notification" only applies to an MCP event
// (a JSON-RPC notification, status 204); an LLM event is either "success"
// or "error".
const (
	TimelineStatusSuccess      = "success"
	TimelineStatusError        = "error"
	TimelineStatusNotification = "notification"
)

// TimelineOrder is the direction a session timeline is returned in.
type TimelineOrder string

const (
	TimelineOrderAsc  TimelineOrder = "asc"
	TimelineOrderDesc TimelineOrder = "desc"
)

// ParseTimelineOrder validates an "order" query parameter; "" means
// ascending (the original behaviour).
func ParseTimelineOrder(s string) (TimelineOrder, bool) {
	switch TimelineOrder(s) {
	case "", TimelineOrderAsc:
		return TimelineOrderAsc, true
	case TimelineOrderDesc:
		return TimelineOrderDesc, true
	}
	return "", false
}

// TimelineOptions are the per-request knobs of Reader.SessionTimeline. It
// is a struct so paging (Limit, Cursor) can be added without changing the
// method signature; pages will follow the same (timestamp, id) order.
type TimelineOptions struct {
	Order TimelineOrder
}

// SortTimelineEvents orders events by the composite key (Timestamp, ID) in
// the requested direction. The ID tie-break makes the order total, so
// events with equal timestamps never swap between calls and a cursor on
// (Timestamp, ID) is well defined.
func SortTimelineEvents(events []TimelineEvent, order TimelineOrder) {
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if !a.Timestamp.Equal(b.Timestamp) {
			if order == TimelineOrderDesc {
				return a.Timestamp.After(b.Timestamp)
			}
			return a.Timestamp.Before(b.Timestamp)
		}
		if order == TimelineOrderDesc {
			return a.ID > b.ID
		}
		return a.ID < b.ID
	})
}

// TimelineEvent is one call in a session's timeline, from either plane.
type TimelineEvent struct {
	Timestamp time.Time `json:"ts"`
	// Plane is "mcp" or "llm".
	Plane string `json:"plane"`
	// Kind is the MCP JSON-RPC method (initialize, tools/call,
	// tools/list, ...) or, for an LLM event, "llm_call".
	Kind string `json:"kind"`
	// Name is the tool called (MCP tools/call only, else "") or the LLM
	// model.
	Name       string `json:"name"`
	Status     string `json:"status"` // one of the TimelineStatus* constants
	DurationMS int64  `json:"duration_ms"`
	KeyID      string `json:"key_id"`
	// ID is the event's request_id.
	ID string `json:"id"`
}

// SessionTimeline is GET
// /api/v1/analytics/sessions/{session_id}/timeline's response body: every
// event belonging to one gateway session, from both planes, merged and
// ordered by (ts, id) in Order (oldest-first by default).
//
// Ownership rule (docs/observability.md#session-timeline-ownership): a
// session's timeline is its own MCP events (session_id = SessionID) plus
// LLM events whose session_id equals SessionID OR equals a
// ClientSessionID seen on one of this session's own MCP events -- AND, in
// every case, whose key_id equals OwnerKeyID. This is what stops a
// caller-supplied X-Session-Id header (sink.AccessLog.ClientSessionID)
// from letting one key's calls read into another key's session: the tag
// alone is never sufficient, the key must also match.
//
// OwnerKeyID is the key_id of this session's earliest MCP event; for an
// LLM-only session (no MCP events at all -- a client that only ever calls
// the LLM plane with this session tag) it is the key_id of the earliest
// LLM event whose session_id equals SessionID.
type SessionTimeline struct {
	SessionID  string `json:"session_id"`
	OwnerKeyID string `json:"owner_key_id"`
	// Order echoes the direction Events are sorted in ("asc" | "desc"),
	// by (ts, id).
	Order  TimelineOrder   `json:"order"`
	Events []TimelineEvent `json:"events"`
	// TotalEvents is len(Events), included so the console doesn't need to
	// separately count.
	TotalEvents int `json:"total_events"`
	// ExcludedForeignEvents counts events that matched this session by tag
	// (session_id or ClientSessionID) but were recorded under a key other
	// than OwnerKeyID, and so are withheld from Events.
	ExcludedForeignEvents int `json:"excluded_foreign_events"`
}

/* ---------------------------------------------------------------------- */
/* Client -> model -> provider Sankey -- GET /api/v1/analytics/client-models */
/* ---------------------------------------------------------------------- */

// SankeyMetric is one of GET /analytics/client-models's supported "metric"
// query values: which llm_calls measure a node/link's Value carries.
type SankeyMetric string

const (
	SankeyMetricCalls  SankeyMetric = "calls"
	SankeyMetricTokens SankeyMetric = "tokens"
	SankeyMetricCost   SankeyMetric = "cost"
)

// ParseSankeyMetric validates a "metric" query parameter. ok is false for
// anything other than the three supported values -- callers default an
// empty raw value to SankeyMetricCalls before calling this, mirroring
// ParseRange's convention.
func ParseSankeyMetric(s string) (m SankeyMetric, ok bool) {
	switch SankeyMetric(s) {
	case SankeyMetricCalls, SankeyMetricTokens, SankeyMetricCost:
		return SankeyMetric(s), true
	default:
		return "", false
	}
}

// SankeyDefaultLimit is the default "limit" query value: the maximum
// number of MODEL nodes kept as their own node before the rest are folded
// into a single "other-models" node.
const SankeyDefaultLimit = 10

// SankeyQuery narrows GET /api/v1/analytics/client-models.
type SankeyQuery struct {
	// Range labels Period (a preset, or RangeCustom); Period is the
	// window the graph covers.
	Range  Range
	Period Period
	Metric SankeyMetric
	// Limit caps the number of distinct MODEL nodes; <= 0 means
	// SankeyDefaultLimit. Never negative by the time a Reader sees it --
	// the handler validates this.
	Limit int
	// ClientName, when non-empty, restricts the graph to one client family
	// (sink.LLMCall.ClientName's output, or "unknown" for a caller-supplied
	// filter matching the "" bucket -- see the handler).
	ClientName string
}

// Sankey node Type values (SankeyNode.Type / SankeyLink.SourceType /
// SankeyLink.TargetType): the three columns of GET
// /api/v1/analytics/client-models's graph, left to right.
const (
	SankeyNodeClient   = "CLIENT"
	SankeyNodeModel    = "MODEL"
	SankeyNodeProvider = "PROVIDER"
)

// SankeyOtherModelsKey/Label name the synthetic MODEL node that folds in
// every model beyond SankeyQuery.Limit (or the default), ranked by Value.
const (
	SankeyOtherModelsKey   = "other-models"
	SankeyOtherModelsLabel = "Other models"
)

// SankeyUnknownClientKey/Label name the CLIENT node for calls with no
// User-Agent at all (sink.LLMCall.ClientName == "").
const (
	SankeyUnknownClientKey   = "unknown"
	SankeyUnknownClientLabel = "Unknown"
)

// SankeyNode is one node in GET /api/v1/analytics/client-models's graph.
// ID is unique across the whole response ("TYPE:key", for chart
// libraries like nivo that need a globally unique node id); Key is unique
// only within Type and is what Link.Source/Target reference, so a link
// identifies a node by its (key, type) pair.
type SankeyNode struct {
	ID    string  `json:"id"`
	Key   string  `json:"key"`
	Label string  `json:"label"`
	Type  string  `json:"type"` // one of the SankeyNode* type constants
	Value float64 `json:"value"`
	// Sublabel is a secondary line under Label, set only where a column
	// has one to offer: the serving provider on a traffic-flow MODEL node.
	// Omitted (never "") everywhere else, including every client-models
	// node, so that endpoint's wire shape is unchanged.
	Sublabel string `json:"sublabel,omitempty"`
}

// SankeyLink is one edge between two SankeyNode rows, referenced by
// (key, type) pairs rather than by SankeyNode.ID. Links with Value 0 are
// omitted from the response entirely rather than included as a zero-width
// edge.
type SankeyLink struct {
	Source     string  `json:"source"`
	SourceType string  `json:"sourceType"`
	Target     string  `json:"target"`
	TargetType string  `json:"targetType"`
	Value      float64 `json:"value"`
}

// Sankey is GET /api/v1/analytics/client-models?range=7d&metric=calls's
// response body: a three-column CLIENT -> MODEL -> PROVIDER usage graph
// built from llm_calls, in a node/link shape Sankey chart libraries
// (nivo, recharts) render directly. A node's Value is the sum of its
// incoming links, except for the first column (CLIENT), whose Value is the
// sum of its outgoing links -- both are the node's total traffic either
// way, since every unit that enters a MODEL or PROVIDER node also leaves
// it (see buildSankey in pkg/sink/clickhouse).
type Sankey struct {
	Range  Range        `json:"range"`
	Metric SankeyMetric `json:"metric"`
	// Total is the metric's sum across every row in the window, computed
	// before any model folding -- it equals the sum of every CLIENT node's
	// Value.
	Total float64      `json:"total"`
	Nodes []SankeyNode `json:"nodes"`
	Links []SankeyLink `json:"links"`
}

/* ---------------------------------------------------------------------- */
/* Agent traffic flow Sankey -- GET /api/v1/analytics/traffic-flow         */
/* ---------------------------------------------------------------------- */

// Traffic-flow node Type values: the columns of GET
// /api/v1/analytics/traffic-flow's graph, left to right. CLIENT and MODEL
// reuse the client-models constants; PATH is the middle "which plane"
// column and CONNECTOR is the MCP plane's terminal column, sitting beside
// MODEL (the LLM plane's).
const (
	SankeyNodePath      = "PATH"
	SankeyNodeConnector = "CONNECTOR"
)

// PATH node keys/labels: one per gateway plane.
const (
	SankeyPathLLMKey   = "llm"
	SankeyPathLLMLabel = "LLM calls"
	SankeyPathMCPKey   = "mcp"
	SankeyPathMCPLabel = "MCP calls"
)

// SankeyOtherConnectorsKey/Label name the synthetic CONNECTOR node that
// folds in every connector beyond SankeyQuery.Limit, ranked by Value --
// the MCP-plane twin of SankeyOtherModelsKey.
const (
	SankeyOtherConnectorsKey   = "other-connectors"
	SankeyOtherConnectorsLabel = "Other connectors"
)

// SankeyGatewayToolsKey/Label name the CONNECTOR node for tools/call rows
// with no connector_id: the gateway's own native tools (gateway__skill and
// friends), which are served in-process rather than routed upstream.
const (
	SankeyGatewayToolsKey   = "gateway"
	SankeyGatewayToolsLabel = "Gateway tools"
)

// SankeyAgent is one entry of TrafficFlow.Agents: a client family seen in
// the window, with its total Value across both planes.
type SankeyAgent struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

// TrafficFlow is GET /api/v1/analytics/traffic-flow's response body: a
// CLIENT -> PATH -> MODEL | CONNECTOR graph joining both planes. Its Nodes
// and Links use the same SankeyNode/SankeyLink shape as Sankey so the same
// renderer draws either graph; a MODEL node additionally carries its
// provider in SankeyNode.Sublabel.
//
// Agents lists every client family with traffic in the window, Value desc,
// regardless of SankeyQuery.ClientName -- it is what a console's agent
// filter offers, so it must not shrink to the one selected family. Nodes,
// Links and Total honour the filter.
//
// Metric semantics: SankeyMetricCalls counts llm_calls rows on the LLM
// branch and tools/call mcp_access_logs rows on the MCP branch. Tokens and
// cost only exist for LLM calls, so for those metrics the MCP branch is
// omitted entirely (no PATH:mcp node) rather than shown as zero-width.
type TrafficFlow struct {
	Sankey
	Agents []SankeyAgent `json:"agents"`
}
