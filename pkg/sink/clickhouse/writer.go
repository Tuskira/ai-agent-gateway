package clickhouse

import (
	"context"
	"encoding/json"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/shopspring/decimal"
)

// batchWriter performs the actual ClickHouse insert for one flushed
// batch. It is the seam between Sink's generic queue/batch engine (run,
// in sink.go) and real ClickHouse I/O: production uses chWriter (below),
// unit tests (sink_test.go) use a fake that just records calls, so the
// batching/overflow/close-flush behavior can be tested without a live
// ClickHouse server.
type batchWriter interface {
	InsertAccess(ctx context.Context, rows []*sink.AccessLog) error
	InsertLLM(ctx context.Context, rows []*sink.LLMCall) error
}

// chWriter is the production batchWriter: one INSERT batch per flush,
// via conn.PrepareBatch. Column order in each INSERT's column list must
// match the Append call's argument order and migrate.go's CREATE TABLE
// column order.
type chWriter struct {
	c conn
}

// insertAccessLogsSQL's column list ends with source, `user`: `user` is
// backtick-quoted because ClickHouse parses an unquoted `user` as the
// user() function, not the column (see migrate.go's doc comment).
const insertAccessLogsSQL = "INSERT INTO mcp_access_logs (" + `
	timestamp, request_id, correlation_id, trace_id, span_id, tenant_id, principal, key_id, session_id, client_session_id,
	method, json_rpc_id, connector_id, tool_name, skill_name, profile, status_code, error_code, duration_ms,
	bytes_in, bytes, client_ip, user_agent, headers, request_body, response_body, truncated, source, ` + "`user`" + `
)`

func (w *chWriter) InsertAccess(ctx context.Context, rows []*sink.AccessLog) error {
	batch, err := w.c.PrepareBatch(ctx, insertAccessLogsSQL)
	if err != nil {
		return err
	}
	defer batch.Close()

	for _, r := range rows {
		if err := batch.Append(
			r.Timestamp, r.RequestID, r.CorrelationID, r.TraceID, "" /* span_id: no source, see migrate.go */, r.TenantID, r.Principal, r.KeyID, r.SessionID, r.ClientSessionID,
			r.Method, r.JSONRPCID, r.ConnectorID, r.ToolName, r.SkillName, r.Profile, u16(r.StatusCode), r.ErrorCode, u32(r.DurationMS),
			u64(r.BytesIn), u64(r.Bytes), r.ClientIP, r.UserAgent, headersJSON(r.Headers), string(r.RequestBody), string(r.ResponseBody), boolUint8(r.Truncated),
			sourceOrDefault(r.Source), r.User,
		); err != nil {
			return err
		}
	}
	return batch.Send()
}

// insertLLMCallsSQL's column list ends with source, `user` -- same
// backtick-quoting reason as insertAccessLogsSQL above.
const insertLLMCallsSQL = "INSERT INTO llm_calls (" + `
	timestamp, request_id, tenant_id, principal, key_id, session_id, provider, upstream_host, model, path,
	status_code, duration_ms, stream, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
	stop_reason, provider_request_id, headers, request_body, response_body, messages, system, tools, truncated, error, cost_usd,
	body_ref, client_ip, client_name, user_agent, requested_model, resolved_vendor, resolved_model, translated, fallback_index, source, ` + "`user`" + `,
	skills_used, mcp_tools_used
)`

func (w *chWriter) InsertLLM(ctx context.Context, rows []*sink.LLMCall) error {
	batch, err := w.c.PrepareBatch(ctx, insertLLMCallsSQL)
	if err != nil {
		return err
	}
	defer batch.Close()

	for _, r := range rows {
		if err := batch.Append(
			r.Timestamp, r.RequestID, r.TenantID, r.Principal, r.KeyID, r.SessionID, r.Provider, r.UpstreamHost, r.Model, r.Path,
			u16(r.StatusCode), u32(r.DurationMS), boolUint8(r.Stream),
			u64(r.InputTokens), u64(r.OutputTokens), u64(r.CacheReadTokens), u64(r.CacheCreationTokens),
			r.StopReason, r.ProviderRequestID, headersJSON(r.Headers), string(r.RequestBody), string(r.ResponseBody), string(r.Messages), string(r.System), string(r.Tools),
			boolUint8(r.Truncated), r.Error, decCost(r.CostUSD),
			r.BodyRef, r.ClientIP, r.ClientName, r.UserAgent, r.RequestedModel, r.ResolvedVendor, r.ResolvedModel, boolUint8(r.Translated), int32(r.FallbackIndex),
			sourceOrDefault(r.Source), r.User,
			nonNilStrings(r.SkillsUsed), nonNilStrings(r.MCPToolsUsed),
		); err != nil {
			return err
		}
	}
	return batch.Send()
}

// nonNilStrings gives Array columns an empty, never nil, slice.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// sourceOrDefault reports "gateway" for a record with no Source set,
// mirroring the column's own ClickHouse DEFAULT so every gateway-proxied
// call (which never sets sink.AccessLog/LLMCall.Source) is explicit about
// its source rather than relying on an empty string being interpreted as
// the default only by the schema.
func sourceOrDefault(s string) string {
	if s == "" {
		return "gateway"
	}
	return s
}

// headersJSON renders a masked-header map as a JSON string, "" when
// empty (rather than the misleading literal "null" or "{}").
func headersJSON(h map[string]string) string {
	if len(h) == 0 {
		return ""
	}
	b, err := json.Marshal(h)
	if err != nil {
		return ""
	}
	return string(b)
}

func boolUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// u16/u32/u64 convert a signed record field to the unsigned ClickHouse
// column type, clamping a negative value (which should never happen for
// these fields) to 0 rather than wrapping to a huge unsigned number.
func u16(n int) uint16 {
	if n < 0 {
		return 0
	}
	if n > 0xffff {
		return 0xffff
	}
	return uint16(n)
}

func u32(n int64) uint32 {
	if n < 0 {
		return 0
	}
	if n > 0xffffffff {
		return 0xffffffff
	}
	return uint32(n)
}

func u64(n int64) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

// decCost converts a cost for the Nullable(Decimal(38,12)) column: nil stays
// NULL (as in Postgres), and the value is rounded to 12 places because the
// driver would truncate (1.3199999999999998e-06 must store as 0.00000132).
func decCost(p *float64) *decimal.Decimal {
	if p == nil {
		return nil
	}
	d := decimal.NewFromFloat(*p).Round(12)
	return &d
}
