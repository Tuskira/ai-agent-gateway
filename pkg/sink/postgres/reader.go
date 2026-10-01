package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// The capture table is the LLM plane's source of record, so it can serve the
// LLM-logs list and detail on its own: a deployment without ClickHouse still
// gets LLM Logs from Postgres. The shape mirrors pkg/sink/clickhouse's
// reader: the list omits bodies, the detail carries them.
var _ analytics.LLMCallReader = (*Sink)(nil)

const defaultListLimit = 50

const llmListCols = `timestamp, request_id, tenant_id, principal, key_id, session_id, provider, upstream_host,
	model, path, status_code, duration_ms, stream, input_tokens, output_tokens, cache_read_tokens,
	cache_creation_tokens, stop_reason, provider_request_id, error, cost_usd, client_name, user_agent,
	requested_model, resolved_vendor, resolved_model, translated, fallback_index`

// ListLLMCalls implements analytics.LLMCallReader.
func (s *Sink) ListLLMCalls(ctx context.Context, tenantID string, f analytics.LLMCallFilter) ([]sink.LLMCall, int, error) {
	where, args := llmCallWhere(tenantID, f)
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+s.table+" WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("sink(postgres): count llm calls: %w", err)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY timestamp DESC LIMIT %d OFFSET %d",
		llmListCols, s.table, where, limit, max(f.Offset, 0))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("sink(postgres): list llm calls: %w", err)
	}
	defer rows.Close()
	out := []sink.LLMCall{}
	for rows.Next() {
		var l sink.LLMCall
		var n nullCols
		if err := rows.Scan(listDest(&l, &n)...); err != nil {
			return nil, 0, fmt.Errorf("sink(postgres): scan llm call: %w", err)
		}
		n.apply(&l)
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// GetLLMCall implements analytics.LLMCallReader.
func (s *Sink) GetLLMCall(ctx context.Context, tenantID, requestID string) (*sink.LLMCall, error) {
	q := "SELECT " + llmListCols + `, headers, request_body, response_body, messages, system, tools, truncated, body_ref
		FROM ` + s.table + " WHERE tenant_id = $1 AND request_id = $2"
	var l sink.LLMCall
	n := new(nullCols)
	var headers, messages, system, tools, bodyRef sql.NullString
	var truncated sql.NullBool
	dest := append(listDest(&l, n), &headers, &l.RequestBody, &l.ResponseBody, &messages, &system, &tools, &truncated, &bodyRef)
	if err := s.db.QueryRowContext(ctx, q, tenantID, requestID).Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, analytics.ErrNotFound
		}
		return nil, fmt.Errorf("sink(postgres): get llm call: %w", err)
	}
	n.apply(&l)
	if headers.String != "" && headers.String != "null" {
		_ = json.Unmarshal([]byte(headers.String), &l.Headers)
	}
	l.Messages, l.System, l.Tools = rawJSON(messages), rawJSON(system), rawJSON(tools)
	l.Truncated, l.BodyRef = truncated.Bool, bodyRef.String
	return &l, nil
}

// nullCols holds one row's nullable columns (rows written before a column
// existed carry NULL) until apply copies them onto the LLMCall.
type nullCols struct {
	tenant, principal, key, session, provider, host, model, path, stop, provReq, errText sql.NullString
	clientName, userAgent, requested, vendor, resolved                                   sql.NullString
	status, duration, in, out, cacheRead, cacheCreate                                    sql.NullInt64
	stream                                                                               sql.NullBool
	cost                                                                                 sql.NullFloat64
}

// listDest returns Scan destinations for llmListCols.
func listDest(l *sink.LLMCall, n *nullCols) []any {
	return []any{&l.Timestamp, &l.RequestID, &n.tenant, &n.principal, &n.key, &n.session, &n.provider, &n.host,
		&n.model, &n.path, &n.status, &n.duration, &n.stream, &n.in, &n.out, &n.cacheRead,
		&n.cacheCreate, &n.stop, &n.provReq, &n.errText, &n.cost, &n.clientName, &n.userAgent,
		&n.requested, &n.vendor, &n.resolved, &l.Translated, &l.FallbackIndex}
}

func (n *nullCols) apply(l *sink.LLMCall) {
	l.TenantID, l.Principal, l.KeyID, l.SessionID = n.tenant.String, n.principal.String, n.key.String, n.session.String
	l.Provider, l.UpstreamHost, l.Model, l.Path = n.provider.String, n.host.String, n.model.String, n.path.String
	l.StopReason, l.ProviderRequestID, l.Error = n.stop.String, n.provReq.String, n.errText.String
	l.ClientName, l.UserAgent = n.clientName.String, n.userAgent.String
	l.RequestedModel, l.ResolvedVendor, l.ResolvedModel = n.requested.String, n.vendor.String, n.resolved.String
	l.StatusCode, l.DurationMS, l.Stream = int(n.status.Int64), n.duration.Int64, n.stream.Bool
	l.InputTokens, l.OutputTokens = n.in.Int64, n.out.Int64
	l.CacheReadTokens, l.CacheCreationTokens = n.cacheRead.Int64, n.cacheCreate.Int64
	if n.cost.Valid {
		c := n.cost.Float64
		l.CostUSD = &c
	}
}

func rawJSON(s sql.NullString) []byte {
	if !s.Valid || s.String == "" || s.String == "null" {
		return nil
	}
	return []byte(s.String)
}

func llmCallWhere(tenantID string, f analytics.LLMCallFilter) (string, []any) {
	clauses, args := []string{"tenant_id = $1"}, []any{tenantID}
	add := func(cond string, v any) {
		args = append(args, v)
		clauses = append(clauses, fmt.Sprintf(cond, len(args)))
	}
	if f.Model != "" {
		add("model = $%d", f.Model)
	}
	if f.SessionID != "" {
		add("session_id = $%d", f.SessionID)
	}
	if f.Principal != "" {
		add("principal = $%d", f.Principal)
	}
	if f.ClientName != "" {
		add("client_name = $%d", f.ClientName)
	}
	if f.Status != 0 {
		add("status_code = $%d", f.Status)
	}
	if !f.From.IsZero() {
		add("timestamp >= $%d", f.From)
	}
	if !f.To.IsZero() {
		add("timestamp <= $%d", f.To)
	}
	return strings.Join(clauses, " AND "), args
}
