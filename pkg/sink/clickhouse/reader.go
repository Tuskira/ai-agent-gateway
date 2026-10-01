package clickhouse

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// defaultListLimit is used when a Filter's Limit is <= 0.
const defaultListLimit = 50

/* ---------------------------------------------------------------------- */
/* Logs / LLM logs: list + get one                                        */
/* ---------------------------------------------------------------------- */

// ListAccessLogs implements analytics.Reader.
func (s *Sink) ListAccessLogs(ctx context.Context, tenantID string, f analytics.AccessLogFilter) ([]sink.AccessLog, int, error) {
	where, args := accessLogWhere(tenantID, f)

	total, err := s.count(ctx, "mcp_access_logs", where, args)
	if err != nil {
		return nil, 0, fmt.Errorf("clickhouse: count access logs: %w", err)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	query := `SELECT timestamp, request_id, correlation_id, trace_id, tenant_id, principal, key_id, session_id, client_session_id,
		method, json_rpc_id, connector_id, tool_name, skill_name, profile, status_code, error_code, duration_ms,
		bytes_in, bytes, client_ip, user_agent, source, ` + "`user`" + `
		FROM mcp_access_logs WHERE ` + where + ` ORDER BY timestamp DESC LIMIT ? OFFSET ?`
	rows, err := s.c.Query(ctx, query, append(append([]any{}, args...), limit, f.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("clickhouse: list access logs: %w", err)
	}
	defer rows.Close()

	var out []sink.AccessLog
	for rows.Next() {
		var a sink.AccessLog
		var statusCode uint16
		var durationMS uint32
		var bytesIn, bytesOut uint64
		if err := rows.Scan(&a.Timestamp, &a.RequestID, &a.CorrelationID, &a.TraceID, &a.TenantID, &a.Principal, &a.KeyID, &a.SessionID, &a.ClientSessionID,
			&a.Method, &a.JSONRPCID, &a.ConnectorID, &a.ToolName, &a.SkillName, &a.Profile, &statusCode, &a.ErrorCode, &durationMS,
			&bytesIn, &bytesOut, &a.ClientIP, &a.UserAgent, &a.Source, &a.User); err != nil {
			return nil, 0, fmt.Errorf("clickhouse: scan access log row: %w", err)
		}
		a.StatusCode = int(statusCode)
		a.DurationMS = int64(durationMS)
		a.BytesIn = int64(bytesIn)
		a.Bytes = int64(bytesOut)
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// GetAccessLog implements analytics.Reader.
func (s *Sink) GetAccessLog(ctx context.Context, tenantID, requestID string) (*sink.AccessLog, error) {
	query := `SELECT timestamp, request_id, correlation_id, trace_id, tenant_id, principal, key_id, session_id, client_session_id,
		method, json_rpc_id, connector_id, tool_name, skill_name, profile, status_code, error_code, duration_ms,
		bytes_in, bytes, client_ip, user_agent, headers, request_body, response_body, truncated, source, ` + "`user`" + `
		FROM mcp_access_logs WHERE tenant_id = ? AND request_id = ? ORDER BY timestamp DESC LIMIT 1`

	var a sink.AccessLog
	var statusCode uint16
	var durationMS uint32
	var bytesIn, bytesOut uint64
	var headersRaw, reqBody, respBody string
	var truncated uint8

	err := s.c.QueryRow(ctx, query, tenantID, requestID).Scan(
		&a.Timestamp, &a.RequestID, &a.CorrelationID, &a.TraceID, &a.TenantID, &a.Principal, &a.KeyID, &a.SessionID, &a.ClientSessionID,
		&a.Method, &a.JSONRPCID, &a.ConnectorID, &a.ToolName, &a.SkillName, &a.Profile, &statusCode, &a.ErrorCode, &durationMS,
		&bytesIn, &bytesOut, &a.ClientIP, &a.UserAgent, &headersRaw, &reqBody, &respBody, &truncated, &a.Source, &a.User,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, analytics.ErrNotFound
		}
		return nil, fmt.Errorf("clickhouse: get access log: %w", err)
	}

	a.StatusCode = int(statusCode)
	a.DurationMS = int64(durationMS)
	a.BytesIn = int64(bytesIn)
	a.Bytes = int64(bytesOut)
	a.Truncated = truncated != 0
	if headersRaw != "" {
		_ = json.Unmarshal([]byte(headersRaw), &a.Headers)
	}
	if reqBody != "" {
		a.RequestBody = []byte(reqBody)
	}
	if respBody != "" {
		a.ResponseBody = []byte(respBody)
	}
	return &a, nil
}

// ListLLMCalls implements analytics.Reader.
func (s *Sink) ListLLMCalls(ctx context.Context, tenantID string, f analytics.LLMCallFilter) ([]sink.LLMCall, int, error) {
	where, args := llmCallWhere(tenantID, f)

	total, err := s.count(ctx, "llm_calls", where, args)
	if err != nil {
		return nil, 0, fmt.Errorf("clickhouse: count llm calls: %w", err)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	// cost_usd is cast with toFloat64(...) -- the column itself is
	// Nullable(Decimal(38,12)) (see migrate.go), which the driver can't
	// scan directly into *float64; the cast produces a Nullable(Float64)
	// that can, and ClickHouse propagates a NULL cost through the cast
	// unchanged (verified: toFloat64(NULL) stays NULL, never 0).
	query := `SELECT timestamp, request_id, tenant_id, principal, key_id, session_id, provider, upstream_host,
		model, path, status_code, duration_ms, stream, input_tokens, output_tokens, cache_read_tokens,
		cache_creation_tokens, stop_reason, provider_request_id, error, client_ip, client_name, user_agent,
		requested_model, resolved_vendor, resolved_model, translated, fallback_index, source, ` + "`user`" + `, toFloat64(cost_usd) AS cost_usd
		FROM llm_calls WHERE ` + where + ` ORDER BY timestamp DESC LIMIT ? OFFSET ?`
	rows, err := s.c.Query(ctx, query, append(append([]any{}, args...), limit, f.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("clickhouse: list llm calls: %w", err)
	}
	defer rows.Close()

	var out []sink.LLMCall
	for rows.Next() {
		var l sink.LLMCall
		var statusCode uint16
		var durationMS uint32
		var stream uint8
		var inTok, outTok, cacheReadTok, cacheCreateTok uint64
		var translated uint8
		var fallbackIndex int32
		if err := rows.Scan(&l.Timestamp, &l.RequestID, &l.TenantID, &l.Principal, &l.KeyID, &l.SessionID, &l.Provider, &l.UpstreamHost,
			&l.Model, &l.Path, &statusCode, &durationMS, &stream, &inTok, &outTok, &cacheReadTok,
			&cacheCreateTok, &l.StopReason, &l.ProviderRequestID, &l.Error, &l.ClientIP, &l.ClientName, &l.UserAgent,
			&l.RequestedModel, &l.ResolvedVendor, &l.ResolvedModel, &translated, &fallbackIndex, &l.Source, &l.User, &l.CostUSD); err != nil {
			return nil, 0, fmt.Errorf("clickhouse: scan llm call row: %w", err)
		}
		l.StatusCode = int(statusCode)
		l.DurationMS = int64(durationMS)
		l.Stream = stream != 0
		l.Translated = translated != 0
		l.FallbackIndex = int(fallbackIndex)
		l.InputTokens, l.OutputTokens, l.CacheReadTokens, l.CacheCreationTokens = int64(inTok), int64(outTok), int64(cacheReadTok), int64(cacheCreateTok)
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// GetLLMCall implements analytics.Reader.
func (s *Sink) GetLLMCall(ctx context.Context, tenantID, requestID string) (*sink.LLMCall, error) {
	// cost_usd cast to Float64 -- see the matching comment on ListLLMCalls's
	// query; same Nullable(Decimal(38,12)) column, same reason.
	query := `SELECT timestamp, request_id, tenant_id, principal, key_id, session_id, provider, upstream_host,
		model, path, status_code, duration_ms, stream, input_tokens, output_tokens, cache_read_tokens,
		cache_creation_tokens, stop_reason, provider_request_id, headers, request_body, response_body,
		messages, system, tools, truncated, error, body_ref, client_ip, client_name, user_agent,
		requested_model, resolved_vendor, resolved_model, translated, fallback_index, source, ` + "`user`" + `, toFloat64(cost_usd) AS cost_usd
		FROM llm_calls WHERE tenant_id = ? AND request_id = ? ORDER BY timestamp DESC LIMIT 1`

	var l sink.LLMCall
	var statusCode uint16
	var durationMS uint32
	var stream uint8
	var inTok, outTok, cacheReadTok, cacheCreateTok uint64
	var headersRaw, reqBody, respBody, messages, system, tools string
	var truncated, translated uint8
	var fallbackIndex int32

	err := s.c.QueryRow(ctx, query, tenantID, requestID).Scan(
		&l.Timestamp, &l.RequestID, &l.TenantID, &l.Principal, &l.KeyID, &l.SessionID, &l.Provider, &l.UpstreamHost,
		&l.Model, &l.Path, &statusCode, &durationMS, &stream, &inTok, &outTok, &cacheReadTok,
		&cacheCreateTok, &l.StopReason, &l.ProviderRequestID, &headersRaw, &reqBody, &respBody,
		&messages, &system, &tools, &truncated, &l.Error, &l.BodyRef, &l.ClientIP, &l.ClientName, &l.UserAgent,
		&l.RequestedModel, &l.ResolvedVendor, &l.ResolvedModel, &translated, &fallbackIndex, &l.Source, &l.User, &l.CostUSD,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, analytics.ErrNotFound
		}
		return nil, fmt.Errorf("clickhouse: get llm call: %w", err)
	}

	l.StatusCode = int(statusCode)
	l.DurationMS = int64(durationMS)
	l.Stream = stream != 0
	l.InputTokens, l.OutputTokens, l.CacheReadTokens, l.CacheCreationTokens = int64(inTok), int64(outTok), int64(cacheReadTok), int64(cacheCreateTok)
	l.Truncated = truncated != 0
	l.Translated = translated != 0
	l.FallbackIndex = int(fallbackIndex)
	if headersRaw != "" {
		_ = json.Unmarshal([]byte(headersRaw), &l.Headers)
	}
	if reqBody != "" {
		l.RequestBody = []byte(reqBody)
	}
	if respBody != "" {
		l.ResponseBody = []byte(respBody)
	}
	if messages != "" {
		l.Messages = []byte(messages)
	}
	if system != "" {
		l.System = []byte(system)
	}
	if tools != "" {
		l.Tools = []byte(tools)
	}
	return &l, nil
}

func accessLogWhere(tenantID string, f analytics.AccessLogFilter) (string, []any) {
	clauses := []string{"tenant_id = ?"}
	args := []any{tenantID}
	if f.Method != "" {
		clauses = append(clauses, "method = ?")
		args = append(args, f.Method)
	}
	if f.ConnectorID != "" {
		clauses = append(clauses, "connector_id = ?")
		args = append(args, f.ConnectorID)
	}
	if f.ToolName != "" {
		clauses = append(clauses, "tool_name = ?")
		args = append(args, f.ToolName)
	}
	if f.SessionID != "" {
		clauses = append(clauses, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.Principal != "" {
		clauses = append(clauses, "principal = ?")
		args = append(args, f.Principal)
	}
	if f.Status != 0 {
		clauses = append(clauses, "status_code = ?")
		args = append(args, uint16(f.Status))
	}
	if f.Source != "" {
		clauses = append(clauses, "source = ?")
		args = append(args, f.Source)
	}
	if f.User != "" {
		clauses = append(clauses, "`user` = ?")
		args = append(args, f.User)
	}
	if !f.From.IsZero() {
		clauses = append(clauses, "timestamp >= ?")
		args = append(args, f.From)
	}
	if !f.To.IsZero() {
		clauses = append(clauses, "timestamp <= ?")
		args = append(args, f.To)
	}
	return strings.Join(clauses, " AND "), args
}

func llmCallWhere(tenantID string, f analytics.LLMCallFilter) (string, []any) {
	clauses := []string{"tenant_id = ?"}
	args := []any{tenantID}
	if f.Model != "" {
		clauses = append(clauses, "model = ?")
		args = append(args, f.Model)
	}
	if f.SessionID != "" {
		clauses = append(clauses, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.Principal != "" {
		clauses = append(clauses, "principal = ?")
		args = append(args, f.Principal)
	}
	if f.ClientName != "" {
		clauses = append(clauses, "client_name = ?")
		args = append(args, f.ClientName)
	}
	if f.Status != 0 {
		clauses = append(clauses, "status_code = ?")
		args = append(args, uint16(f.Status))
	}
	if f.Source != "" {
		clauses = append(clauses, "source = ?")
		args = append(args, f.Source)
	}
	if f.User != "" {
		clauses = append(clauses, "`user` = ?")
		args = append(args, f.User)
	}
	if !f.From.IsZero() {
		clauses = append(clauses, "timestamp >= ?")
		args = append(args, f.From)
	}
	if !f.To.IsZero() {
		clauses = append(clauses, "timestamp <= ?")
		args = append(args, f.To)
	}
	return strings.Join(clauses, " AND "), args
}

func (s *Sink) count(ctx context.Context, table, where string, args []any) (int, error) {
	var n uint64
	if err := s.c.QueryRow(ctx, "SELECT count() FROM "+table+" WHERE "+where, args...).Scan(&n); err != nil {
		return 0, err
	}
	return int(n), nil
}

/* ---------------------------------------------------------------------- */
/* Overview                                                                */
/* ---------------------------------------------------------------------- */

// Overview implements analytics.Reader. It runs several small aggregate
// queries against the current window [now-r.Window(), now) and, for the
// KPI deltas, the immediately preceding window of the same length.
func (s *Sink) Overview(ctx context.Context, tenantID string, r analytics.Range) (*analytics.Overview, error) {
	now := time.Now().UTC()
	curFrom, curTo := now.Add(-r.Window()), now
	prevFrom, prevTo := curFrom.Add(-r.Window()), curFrom

	cur, err := s.traffic(ctx, tenantID, curFrom, curTo)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview traffic: %w", err)
	}
	prev, err := s.traffic(ctx, tenantID, prevFrom, prevTo)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview traffic (previous window): %w", err)
	}

	curTokens, err := s.totalTokens(ctx, tenantID, curFrom, curTo)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview tokens: %w", err)
	}
	prevTokens, err := s.totalTokens(ctx, tenantID, prevFrom, prevTo)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview tokens (previous window): %w", err)
	}

	curCost, err := s.totalCost(ctx, tenantID, curFrom, curTo)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview cost: %w", err)
	}
	prevCost, err := s.totalCost(ctx, tenantID, prevFrom, prevTo)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview cost (previous window): %w", err)
	}

	successCur, totalNonNotifCur, err := s.successCounts(ctx, tenantID, curFrom, curTo)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview success rate: %w", err)
	}

	llmUsage, err := s.llmUsage(ctx, tenantID, curFrom, curTo)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview llm usage: %w", err)
	}
	topAgents, err := s.topNamed(ctx, "mcp_access_logs", "profile", tenantID, curFrom, curTo, 10)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview top profiles: %w", err)
	}
	statusCodes, err := s.statusCodes(ctx, tenantID, curFrom, curTo)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview status codes: %w", err)
	}
	latency, err := s.latency(ctx, tenantID, curFrom, curTo)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview latency: %w", err)
	}
	requestsByClient, err := s.requestsByClient(ctx, tenantID, curFrom, curTo)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview requests by client: %w", err)
	}
	mcpTools, err := s.topNamed(ctx, "mcp_access_logs", "tool_name", tenantID, curFrom, curTo, 10)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview mcp tools: %w", err)
	}
	topConnectors, err := s.topNamed(ctx, "mcp_access_logs", "connector_id", tenantID, curFrom, curTo, 10)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview top connectors: %w", err)
	}
	trafficOverTime, err := s.trafficOverTime(ctx, tenantID, curFrom, curTo, r.Bucket())
	if err != nil {
		return nil, fmt.Errorf("clickhouse: overview traffic over time: %w", err)
	}

	return &analytics.Overview{
		Range: r,
		Kpis: analytics.OverviewKpis{
			LlmAgentCalls: countKpi(cur.llm, prev.llm),
			McpToolCalls:  countKpi(cur.mcpToolCalls, prev.mcpToolCalls),
			TotalTokens:   countKpi(curTokens, prevTokens),
			// Summed from the frozen-at-write cost_usd column (pkg/pricing);
			// unpriced (NULL) calls are skipped. Sub-dollar totals are why the KPI
			// value is a float (see analytics.CountKpi).
			TotalCost:   costKpi(curCost, prevCost),
			SuccessRate: analytics.SuccessRateKpi{Value: pct(successCur, totalNonNotifCur), Sub: fmt.Sprintf("%d / %d responses", successCur, totalNonNotifCur)},
		},
		LlmUsage:         llmUsage,
		Traffic:          analytics.Traffic{LlmCalls: cur.llm, McpCalls: cur.mcp},
		TopAgents:        topAgents,
		StatusCodes:      statusCodes,
		SuccessPct:       pct(successCur, totalNonNotifCur),
		Latency:          latency,
		RequestsByClient: requestsByClient,
		McpTools:         mcpTools,
		TopConnectors:    topConnectors,
		TrafficOverTime:  trafficOverTime,
	}, nil
}

type trafficCounts struct {
	mcp          int64 // every mcp_access_logs row (tools/list, tools/call, initialize, ...)
	llm          int64 // every llm_calls row
	mcpToolCalls int64 // mcp_access_logs rows where method = 'tools/call'
}

func (s *Sink) traffic(ctx context.Context, tenantID string, from, to time.Time) (trafficCounts, error) {
	var tc trafficCounts
	var err error
	if tc.mcp, err = s.scalarCount(ctx, `SELECT count() FROM mcp_access_logs WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?`, tenantID, from, to); err != nil {
		return tc, err
	}
	if tc.llm, err = s.scalarCount(ctx, `SELECT count() FROM llm_calls WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?`, tenantID, from, to); err != nil {
		return tc, err
	}
	if tc.mcpToolCalls, err = s.scalarCount(ctx, `SELECT count() FROM mcp_access_logs WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ? AND method = 'tools/call'`, tenantID, from, to); err != nil {
		return tc, err
	}
	return tc, nil
}

func (s *Sink) scalarCount(ctx context.Context, query string, args ...any) (int64, error) {
	var n uint64
	if err := s.c.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return int64(n), nil
}

func (s *Sink) totalTokens(ctx context.Context, tenantID string, from, to time.Time) (int64, error) {
	query := `SELECT sum(input_tokens + output_tokens + cache_read_tokens + cache_creation_tokens)
		FROM llm_calls WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?`
	var sum uint64
	if err := s.c.QueryRow(ctx, query, tenantID, from, to).Scan(&sum); err != nil {
		return 0, err
	}
	return int64(sum), nil
}

// totalCost sums the frozen-at-write cost_usd across the window. Calls with an
// unknown cost are NULL and skipped by sum(); ifNull covers a window where every
// call is NULL (or there are none), which sum() returns as NULL.
func (s *Sink) totalCost(ctx context.Context, tenantID string, from, to time.Time) (float64, error) {
	// sum(cost_usd) is exact Decimal arithmetic; cast to Float64 only for
	// transport (the KPI value is a float on the wire anyway).
	query := `SELECT toFloat64(ifNull(sum(cost_usd), 0)) FROM llm_calls WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?`
	var sum float64
	if err := s.c.QueryRow(ctx, query, tenantID, from, to).Scan(&sum); err != nil {
		return 0, err
	}
	return sum, nil
}

// successCounts reports (success, totalNonNotification) across both
// tables in the window, applying the same outcome rule as statusCodes
// (see analytics.Outcome* and outcomeCaseSQL): success counts
// OutcomeSuccess rows; totalNonNotification counts every row that is
// NOT OutcomeNotification (204). Notifications are excluded from both
// sides rather than counted as failures, so
// Overview.SuccessPct = success / totalNonNotification agrees with the
// "200" slice's share of the non-204 rows in the statusCodes chart.
func (s *Sink) successCounts(ctx context.Context, tenantID string, from, to time.Time) (success, totalNonNotification int64, err error) {
	query := `SELECT
		countIf(status_code != 204 AND NOT (error_code != '' OR status_code >= 400)),
		countIf(status_code != 204)
	FROM (
		SELECT status_code, error_code FROM mcp_access_logs WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?
		UNION ALL
		SELECT status_code, error AS error_code FROM llm_calls WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?
	)`
	var succ, tot uint64
	if err := s.c.QueryRow(ctx, query, tenantID, from, to, tenantID, from, to).Scan(&succ, &tot); err != nil {
		return 0, 0, err
	}
	return int64(succ), int64(tot), nil
}

func (s *Sink) llmUsage(ctx context.Context, tenantID string, from, to time.Time) ([]analytics.LlmUsageRow, error) {
	query := `SELECT model, count() AS calls, sum(input_tokens + output_tokens + cache_read_tokens + cache_creation_tokens) AS tokens, toFloat64(ifNull(sum(cost_usd), 0)) AS cost
		FROM llm_calls WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?
		GROUP BY model ORDER BY tokens DESC LIMIT 20`
	rows, err := s.c.Query(ctx, query, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []analytics.LlmUsageRow{}
	for rows.Next() {
		var model string
		var calls, tokens uint64
		var cost float64
		if err := rows.Scan(&model, &calls, &tokens, &cost); err != nil {
			return nil, err
		}
		out = append(out, analytics.LlmUsageRow{Model: model, Calls: int64(calls), Tokens: int64(tokens), Cost: cost})
	}
	return out, rows.Err()
}

// topNamed ranks a mcp_access_logs column's non-empty values by row
// count. table/column are always package-internal constants (never
// caller input), so building the query string with them is safe; every
// actual value (tenantID, from, to, limit) is still passed as a bound
// parameter.
func (s *Sink) topNamed(ctx context.Context, table, column, tenantID string, from, to time.Time, limit int) ([]analytics.NamedCount, error) {
	query := fmt.Sprintf(`SELECT %s AS name, count() AS c FROM %s
		WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ? AND %s != ''
		GROUP BY name ORDER BY c DESC LIMIT ?`, column, table, column)
	rows, err := s.c.Query(ctx, query, tenantID, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []analytics.NamedCount{}
	for rows.Next() {
		var name string
		var c uint64
		if err := rows.Scan(&name, &c); err != nil {
			return nil, err
		}
		out = append(out, analytics.NamedCount{Name: name, Count: int64(c)})
	}
	return out, rows.Err()
}

// outcomeCaseSQL buckets a (status_code, error_code) row into one of
// analytics.Outcome{Notification,Error,Success} -- see that type's doc
// comment for the rule and evaluation order. Built with fmt.Sprintf from
// the same Go constants callers compare against, so the SQL literal and
// the Go label set can never drift apart.
var outcomeCaseSQL = fmt.Sprintf(
	`multiIf(status_code = 204, '%s', (error_code != '' OR status_code >= 400), '%s', '%s')`,
	analytics.OutcomeNotification, analytics.OutcomeError, analytics.OutcomeSuccess,
)

// statusCodes buckets every row in the window by outcome (see
// analytics.Outcome* and outcomeCaseSQL), not by raw HTTP status: every
// MCP call answers HTTP 200 regardless of whether the JSON-RPC call
// itself failed, so a raw 2xx/4xx/5xx split would always show ~100%
// "2xx". Pct is each bucket's share of ALL rows in the window (the
// notification slice included) -- successCounts is the one that
// excludes notifications from its denominator, for SuccessPct.
func (s *Sink) statusCodes(ctx context.Context, tenantID string, from, to time.Time) ([]analytics.StatusCodeSlice, error) {
	query := fmt.Sprintf(`SELECT %s AS label, count() AS c
		FROM (
			SELECT status_code, error_code FROM mcp_access_logs WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?
			UNION ALL
			SELECT status_code, error AS error_code FROM llm_calls WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?
		)
		GROUP BY label ORDER BY label`, outcomeCaseSQL)
	rows, err := s.c.Query(ctx, query, tenantID, from, to, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type bucket struct {
		label string
		count uint64
	}
	var buckets []bucket
	var total uint64
	for rows.Next() {
		var b bucket
		if err := rows.Scan(&b.label, &b.count); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
		total += b.count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]analytics.StatusCodeSlice, 0, len(buckets))
	for _, b := range buckets {
		p := 0.0
		if total > 0 {
			p = float64(b.count) / float64(total) * 100
		}
		out = append(out, analytics.StatusCodeSlice{Label: b.label, Pct: p})
	}
	return out, nil
}

func (s *Sink) latency(ctx context.Context, tenantID string, from, to time.Time) (analytics.Latency, error) {
	quantileQuery := `SELECT quantile(0.5)(duration_ms), quantile(0.95)(duration_ms)
		FROM (
			SELECT duration_ms FROM mcp_access_logs WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?
			UNION ALL
			SELECT duration_ms FROM llm_calls WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?
		)`
	var median, p95 float64
	if err := s.c.QueryRow(ctx, quantileQuery, tenantID, from, to, tenantID, from, to).Scan(&median, &p95); err != nil {
		return analytics.Latency{}, err
	}

	slowQuery := `SELECT name, duration_ms FROM (
			SELECT if(tool_name != '', tool_name, method) AS name, duration_ms FROM mcp_access_logs WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?
			UNION ALL
			SELECT model AS name, duration_ms FROM llm_calls WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?
		)
		ORDER BY duration_ms DESC LIMIT 5`
	rows, err := s.c.Query(ctx, slowQuery, tenantID, from, to, tenantID, from, to)
	if err != nil {
		return analytics.Latency{}, err
	}
	defer rows.Close()

	slowest := []analytics.SlowCall{}
	for rows.Next() {
		var name string
		var ms uint32
		if err := rows.Scan(&name, &ms); err != nil {
			return analytics.Latency{}, err
		}
		slowest = append(slowest, analytics.SlowCall{Name: name, MS: int64(ms)})
	}
	if err := rows.Err(); err != nil {
		return analytics.Latency{}, err
	}

	return analytics.Latency{MedianMS: median, P95MS: p95, Slowest: slowest}, nil
}

// requestsByClient groups mcp_access_logs.user_agent into the gateway's
// named client families plus "other". Bucketing happens in Go (via
// sink.ClientFamily) rather than SQL, so the substring-match rule stays in
// one obviously-correct place, shared with the LLM plane's
// sink.LLMCall.ClientName.
func (s *Sink) requestsByClient(ctx context.Context, tenantID string, from, to time.Time) ([]analytics.NamedCount, error) {
	query := `SELECT user_agent, count() AS c FROM mcp_access_logs
		WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?
		GROUP BY user_agent`
	rows, err := s.c.Query(ctx, query, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int64{}
	for rows.Next() {
		var ua string
		var c uint64
		if err := rows.Scan(&ua, &c); err != nil {
			return nil, err
		}
		counts[requestsByClientFamily(ua)] += int64(c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]analytics.NamedCount, 0, len(counts))
	for name, c := range counts {
		out = append(out, analytics.NamedCount{Name: name, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// requestsByClientFamily wraps sink.ClientFamily for this chart: unlike
// sink.LLMCall.ClientName, requestsByClient has no separate "no client
// info" bucket, so a missing User-Agent folds into "other" like any other
// unrecognized client instead of coming back as "".
func requestsByClientFamily(userAgent string) string {
	if fam := sink.ClientFamily(userAgent); fam != "" {
		return fam
	}
	return "other"
}

// trafficOverTime buckets both tables' row counts hourly (24h range) or
// daily (7d/30d), across [from, to), filling any bucket with no rows as
// 0 rather than omitting it (so the chart's x-axis has no gaps).
func (s *Sink) trafficOverTime(ctx context.Context, tenantID string, from, to time.Time, bucket time.Duration) ([]analytics.TrafficPoint, error) {
	truncFn := "toStartOfHour"
	labelFormat := "3PM"
	if bucket >= 24*time.Hour {
		truncFn = "toStartOfDay"
		labelFormat = "Jan 2"
	}

	mcpCounts, err := s.bucketedCounts(ctx, "mcp_access_logs", truncFn, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	llmCounts, err := s.bucketedCounts(ctx, "llm_calls", truncFn, tenantID, from, to)
	if err != nil {
		return nil, err
	}

	start := from.Truncate(bucket)
	points := []analytics.TrafficPoint{}
	for t := start; t.Before(to); t = t.Add(bucket) {
		key := t.Unix()
		points = append(points, analytics.TrafficPoint{
			Label:    t.Format(labelFormat),
			McpCalls: mcpCounts[key],
			LlmCalls: llmCounts[key],
		})
	}
	return points, nil
}

func (s *Sink) bucketedCounts(ctx context.Context, table, truncFn, tenantID string, from, to time.Time) (map[int64]int64, error) {
	query := fmt.Sprintf(`SELECT %s(timestamp) AS bucket, count() AS c FROM %s
		WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ? GROUP BY bucket`, truncFn, table)
	rows, err := s.c.Query(ctx, query, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]int64{}
	for rows.Next() {
		var bucketTime time.Time
		var c uint64
		if err := rows.Scan(&bucketTime, &c); err != nil {
			return nil, err
		}
		out[bucketTime.Unix()] = int64(c)
	}
	return out, rows.Err()
}

/* ---------------------------------------------------------------------- */
/* Models summary                                                          */
/* ---------------------------------------------------------------------- */

// ModelsSummary implements analytics.Reader.
func (s *Sink) ModelsSummary(ctx context.Context, tenantID string, r analytics.Range) (*analytics.ModelsSummary, error) {
	now := time.Now().UTC()
	from, to := now.Add(-r.Window()), now

	rows, err := s.modelUsage(ctx, tenantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: models summary: %w", err)
	}

	summary := &analytics.ModelsSummary{Range: r, Models: rows, TotalModels: len(rows)}
	for _, row := range rows {
		if summary.HighestTraffic == nil || row.Tokens > summary.HighestTraffic.Tokens {
			summary.HighestTraffic = &analytics.ModelHighestTraffic{Name: row.Name, Tokens: row.Tokens}
		}
	}
	return summary, nil
}

// modelUsage groups llm_calls by the model NAME THE CLIENT ASKED FOR
// (requested_model), one row per name: that is what the model registry
// registers and what the console's Models page lists. The row's provider is
// the vendor that served most of the name's calls (resolved_vendor, the
// registry target that answered), so an alias whose targets span vendors is
// still one row.
//
// Rows that carry no registry columns fall back to what they do carry:
// model for the name (rows written before requested_model existed) and
// provider, the client's dialect, for the vendor (an unregistered name,
// which is forwarded to that dialect's own vendor; or a call refused before
// any target was tried).
//
// Rows ordered by Tokens descending, matching llmUsage's convention.
func (s *Sink) modelUsage(ctx context.Context, tenantID string, from, to time.Time) ([]analytics.ModelSummaryRow, error) {
	query := `SELECT if(requested_model != '', requested_model, model) AS name,
			topKIf(1)(resolved_vendor, resolved_vendor != '') AS vendors,
			topK(1)(provider) AS dialects,
			count() AS calls,
			sum(input_tokens + output_tokens + cache_read_tokens + cache_creation_tokens) AS tokens,
			toFloat64(ifNull(sum(cost_usd), 0)) AS cost_sum,
			countIf(isNotNull(cost_usd)) AS priced_calls,
			uniqExact(key_id) AS used_by,
			max(timestamp) AS last_seen
		FROM llm_calls
		WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ? AND (requested_model != '' OR model != '')
		GROUP BY name
		ORDER BY tokens DESC, name`
	rowsRS, err := s.c.Query(ctx, query, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	defer rowsRS.Close()

	out := []analytics.ModelSummaryRow{}
	for rowsRS.Next() {
		var name string
		var vendors, dialects []string
		var calls, tokens, usedBy, pricedCalls uint64
		var costSum float64
		var lastSeen time.Time
		if err := rowsRS.Scan(&name, &vendors, &dialects, &calls, &tokens, &costSum, &pricedCalls, &usedBy, &lastSeen); err != nil {
			return nil, err
		}
		provider := ""
		if len(vendors) > 0 {
			provider = vendors[0]
		} else if len(dialects) > 0 {
			provider = dialects[0]
		}
		row := analytics.ModelSummaryRow{
			Name:     name,
			Provider: provider,
			Calls:    int64(calls),
			Tokens:   int64(tokens),
			UsedBy:   int64(usedBy),
			LastSeen: lastSeen,
			Status:   "active",
		}
		// Only stamp a cost when at least one call in the group had a
		// known price -- an all-NULL group must report nil, not a
		// fabricated 0 (see analytics.ModelSummaryRow.CostUSD).
		if pricedCalls > 0 {
			cost := costSum
			row.CostUSD = &cost
		}
		out = append(out, row)
	}
	return out, rowsRS.Err()
}

/* ---------------------------------------------------------------------- */
/* Skills summary                                                          */
/* ---------------------------------------------------------------------- */

// SkillsSummary implements analytics.Reader.
func (s *Sink) SkillsSummary(ctx context.Context, tenantID string, r analytics.Range) (*analytics.SkillsSummary, error) {
	now := time.Now().UTC()
	from, to := now.Add(-r.Window()), now

	rows, err := s.skillUsage(ctx, tenantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: skills summary: %w", err)
	}

	summary := &analytics.SkillsSummary{Range: r, Skills: rows, TotalSkills: len(rows)}
	for _, row := range rows {
		if summary.MostUsed == nil || row.Calls > summary.MostUsed.Calls {
			summary.MostUsed = &analytics.SkillMostUsed{Name: row.Name, Kind: row.Kind, Calls: row.Calls}
		}
	}
	return summary, nil
}

// skillUsage groups mcp_access_logs by skill_name, one row per skill or
// command a gateway__skill tools/call or a native command's prompts/get
// carried in the window. Kind is always left "" here -- it comes from the
// skill/command registry, which this package knows nothing about; see
// internal/api/handlers.Analytics.Skills for the join.
//
// Rows ordered by Calls descending, matching modelUsage's Tokens-descending
// convention.
func (s *Sink) skillUsage(ctx context.Context, tenantID string, from, to time.Time) ([]analytics.SkillSummaryRow, error) {
	query := `SELECT skill_name AS name,
			count() AS calls,
			uniqExact(key_id) AS used_by,
			max(timestamp) AS last_seen
		FROM mcp_access_logs
		WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ? AND skill_name != ''
		GROUP BY name
		ORDER BY calls DESC, name`
	rowsRS, err := s.c.Query(ctx, query, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	defer rowsRS.Close()

	out := []analytics.SkillSummaryRow{}
	for rowsRS.Next() {
		var name string
		var calls, usedBy uint64
		var lastSeen time.Time
		if err := rowsRS.Scan(&name, &calls, &usedBy, &lastSeen); err != nil {
			return nil, err
		}
		out = append(out, analytics.SkillSummaryRow{
			Name:     name,
			Calls:    int64(calls),
			UsedBy:   int64(usedBy),
			LastSeen: lastSeen,
		})
	}
	return out, rowsRS.Err()
}

// discoveryGroupLimit bounds the groups one discovery query returns, so a
// hostile client inventing tool names cannot make the response unbounded.
const discoveryGroupLimit = 5000

// SkillUsage implements analytics.Reader: llm_calls.skills_used grouped by
// skill name.
func (s *Sink) SkillUsage(ctx context.Context, tenantID string, r analytics.Range) ([]analytics.SkillUsage, error) {
	now := time.Now().UTC()
	from, to := now.Add(-r.Window()), now
	query := `SELECT skill AS name, count() AS calls, uniqExact(key_id) AS used_by, max(timestamp) AS last_seen
		FROM llm_calls ARRAY JOIN skills_used AS skill
		WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ? AND skill != ''
		GROUP BY name
		ORDER BY calls DESC, name
		LIMIT ` + fmt.Sprint(discoveryGroupLimit)
	rows, err := s.c.Query(ctx, query, tenantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: skill usage: %w", err)
	}
	defer rows.Close()
	out := []analytics.SkillUsage{}
	for rows.Next() {
		var u analytics.SkillUsage
		var calls, usedBy uint64
		if err := rows.Scan(&u.Name, &calls, &usedBy, &u.LastSeen); err != nil {
			return nil, fmt.Errorf("clickhouse: scan skill usage: %w", err)
		}
		u.Calls, u.UsedBy = int64(calls), int64(usedBy)
		out = append(out, u)
	}
	return out, rows.Err()
}

// MCPToolUsage implements analytics.Reader: llm_calls.mcp_tools_used grouped
// by "server__tool" value, split at the first "__".
func (s *Sink) MCPToolUsage(ctx context.Context, tenantID string, r analytics.Range) ([]analytics.MCPToolUsage, error) {
	now := time.Now().UTC()
	from, to := now.Add(-r.Window()), now
	query := `SELECT ref, count() AS calls, groupUniqArray(1000)(key_id) AS keys, max(timestamp) AS last_seen
		FROM llm_calls ARRAY JOIN mcp_tools_used AS ref
		WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ? AND ref != ''
		GROUP BY ref
		ORDER BY calls DESC, ref
		LIMIT ` + fmt.Sprint(discoveryGroupLimit)
	rows, err := s.c.Query(ctx, query, tenantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: mcp tool usage: %w", err)
	}
	defer rows.Close()
	out := []analytics.MCPToolUsage{}
	for rows.Next() {
		var ref string
		var calls uint64
		var u analytics.MCPToolUsage
		if err := rows.Scan(&ref, &calls, &u.Keys, &u.LastSeen); err != nil {
			return nil, fmt.Errorf("clickhouse: scan mcp tool usage: %w", err)
		}
		server, tool, ok := strings.Cut(ref, "__")
		if !ok || server == "" || tool == "" {
			continue
		}
		u.Server, u.Tool, u.Calls = server, tool, int64(calls)
		out = append(out, u)
	}
	return out, rows.Err()
}

// MCPServerCalls implements analytics.Reader: distinct llm_calls.request_id
// per server row. The "server__tool" ref is split at "__"; when the tool part
// itself starts "<alias>__" and alias is a known connector (aliases, lowercased
// name or slug -> canonical slug) the row is that connector, via the gateway.
// Counting request_id rather than array rows keeps a call that used two tools
// of one server at one.
func (s *Sink) MCPServerCalls(ctx context.Context, tenantID string, r analytics.Range, aliases map[string]string) ([]analytics.MCPServerCalls, error) {
	now := time.Now().UTC()
	from, to := now.Add(-r.Window()), now
	names, slugs := []string{""}, []string{""} // transform() needs non-empty arrays
	for n, sl := range aliases {
		names, slugs = append(names, n), append(slugs, sl)
	}
	query := `SELECT key, via, uniqExact(request_id) AS calls FROM (
			SELECT request_id,
				splitByString('__', ref) AS p,
				transform(lowerUTF8(p[2]), ?, ?, '') AS slug,
				(length(p) >= 3 AND slug != '') AS via,
				if(via, slug, p[1]) AS key
			FROM llm_calls ARRAY JOIN mcp_tools_used AS ref
			WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ? AND ref != ''
				AND length(p) >= 2 AND p[1] != '' AND p[2] != ''
		)
		GROUP BY key, via
		ORDER BY calls DESC, key
		LIMIT ` + fmt.Sprint(discoveryGroupLimit)
	rows, err := s.c.Query(ctx, query, names, slugs, tenantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: mcp server calls: %w", err)
	}
	defer rows.Close()
	out := []analytics.MCPServerCalls{}
	for rows.Next() {
		var c analytics.MCPServerCalls
		var via uint8
		var calls uint64
		if err := rows.Scan(&c.Server, &via, &calls); err != nil {
			return nil, fmt.Errorf("clickhouse: scan mcp server calls: %w", err)
		}
		c.Via, c.Calls = via != 0, int64(calls)
		out = append(out, c)
	}
	return out, rows.Err()
}

func countKpi(cur, prev int64) analytics.CountKpi {
	return analytics.CountKpi{Value: float64(cur), Delta: deltaPct(float64(cur), float64(prev))}
}

// costKpi is countKpi for a sub-dollar money value: the KPI value carries
// the exact float rather than truncating to whole dollars.
func costKpi(cur, prev float64) analytics.CountKpi {
	return analytics.CountKpi{Value: cur, Delta: deltaPct(cur, prev)}
}

// deltaPct computes the signed percent change of cur versus prev. A
// zero-to-nonzero jump is reported as +100% "up" (there's no meaningful
// percentage of zero); zero-to-zero is 0% "up".
func deltaPct(cur, prev float64) analytics.KpiDelta {
	if prev == 0 {
		if cur == 0 {
			return analytics.KpiDelta{Pct: 0, Direction: "up"}
		}
		return analytics.KpiDelta{Pct: 100, Direction: "up"}
	}
	p := (cur - prev) / prev * 100
	dir := "up"
	if p < 0 {
		dir = "down"
	}
	return analytics.KpiDelta{Pct: p, Direction: dir}
}

func pct(n, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}
