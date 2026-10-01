package clickhouse

import "context"

// migrationMCPAccessLogs and migrationLLMCalls are the sink's one baseline
// migration: two idempotent CREATE TABLE IF NOT EXISTS statements, applied
// at startup (see applyMigrations). Column order here is the contract
// insertAccessLogs/insertLLMCalls (writer.go) append values in -- keep
// them in sync.
//
// profile (mcp_access_logs) is not part of the design doc's original
// column list; it was added so GET /analytics/overview can rank agent
// profiles (topAgents), fed by sink.AccessLog.Profile.
//
// span_id (mcp_access_logs) is part of the design doc's column list but
// has no source today: sink.AccessLog carries a trace id (see
// pkg/trace), not a span id, so the column exists for forward
// compatibility and is always written empty.
//
// source and `user` (both tables) back the interceptor-ingest feature
// (sink.AccessLog/LLMCall.Source/User, POST /api/v1/ingest): source tells
// a gateway-proxied row from an ingested one, defaulting to "gateway" so
// every row written before this column existed reads back correctly
// without a backfill; `user` is the ingested row's self-reported caller
// identity, "" on a gateway-proxied row. `user` is backtick-quoted on
// every reference (here, in writer.go's INSERT column lists, and in
// reader.go's SELECT/WHERE clauses): unquoted, ClickHouse parses it as
// the user() function, not a column name.
const migrationMCPAccessLogs = `
CREATE TABLE IF NOT EXISTS mcp_access_logs (
	timestamp DateTime64(3),
	request_id String,
	correlation_id String,
	trace_id String,
	span_id String,
	tenant_id String,
	principal String,
	key_id String,
	session_id String,
	client_session_id String,
	method String,
	json_rpc_id String,
	connector_id String,
	tool_name String,
	skill_name String,
	profile String,
	status_code UInt16,
	error_code String,
	duration_ms UInt32,
	bytes_in UInt64,
	bytes UInt64,
	client_ip String,
	user_agent String,
	headers String,
	request_body String,
	response_body String,
	truncated UInt8,
	source LowCardinality(String) DEFAULT 'gateway',
	` + "`user`" + ` String DEFAULT ''
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (tenant_id, timestamp, request_id)
TTL toDateTime(timestamp) + INTERVAL 90 DAY
`

const migrationLLMCalls = `
CREATE TABLE IF NOT EXISTS llm_calls (
	timestamp DateTime64(3),
	request_id String,
	tenant_id String,
	principal String,
	key_id String,
	session_id String,
	provider String,
	upstream_host String,
	model String,
	path String,
	status_code UInt16,
	duration_ms UInt32,
	stream UInt8,
	input_tokens UInt64,
	output_tokens UInt64,
	cache_read_tokens UInt64,
	cache_creation_tokens UInt64,
	stop_reason String,
	provider_request_id String,
	headers String,
	request_body String,
	response_body String,
	messages String,
	system String,
	tools String,
	truncated UInt8,
	error String,
	cost_usd Nullable(Decimal(38, 12)),
	body_ref String,
	client_ip String,
	client_name String,
	user_agent String,
	requested_model String,
	resolved_vendor String,
	resolved_model String,
	translated UInt8,
	fallback_index Int32,
	source LowCardinality(String) DEFAULT 'gateway',
	` + "`user`" + ` String DEFAULT ''
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (tenant_id, timestamp, request_id)
TTL toDateTime(timestamp) + INTERVAL 90 DAY
`

// alterLLMCallsCostUSD upgrades an existing llm_calls table to carry cost_usd
// as an exact, nullable Decimal (money must not be a float; NULL = unknown
// cost, as in Postgres). ADD handles tables created before the column existed;
// MODIFY converts an older Float64 or non-nullable Decimal column in place
// (rows written before this keep their stored value). Both idempotent.
var alterLLMCallsCostUSD = []string{
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS cost_usd Nullable(Decimal(38, 12))`,
	`ALTER TABLE llm_calls MODIFY COLUMN IF EXISTS cost_usd Nullable(Decimal(38, 12))`,
}

// alterLLMCallsBodyRef upgrades an existing llm_calls table to carry body_ref,
// the sink.BodyStore ref of a call whose bodies were offloaded ("" = inline).
// Idempotent.
var alterLLMCallsBodyRef = []string{
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS body_ref String`,
}

// alterLLMCallsClientIP upgrades an existing llm_calls table to carry the
// caller's address. Idempotent.
var alterLLMCallsClientIP = []string{
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS client_ip String`,
}

// alterLLMCallsModelRegistry upgrades an existing llm_calls table with the
// model registry columns (sink.LLMCall.RequestedModel & co.): what the
// client asked for vs. the vendor/model the call resolved to. Idempotent;
// rows written before carry "" / 0, i.e. "unregistered, not translated".
var alterLLMCallsModelRegistry = []string{
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS requested_model String`,
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS resolved_vendor String`,
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS resolved_model String`,
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS translated UInt8`,
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS fallback_index Int32`,
}

// alterLLMCallsClientInfo upgrades an existing llm_calls table to carry
// sink.LLMCall.ClientName/UserAgent: which client family made the call and
// its raw User-Agent header. Idempotent; rows written before carry "".
var alterLLMCallsClientInfo = []string{
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS client_name String`,
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS user_agent String`,
}

// alterMCPAccessLogsClientSessionID upgrades an existing mcp_access_logs
// table to carry the caller-supplied session tag (sink.AccessLog.
// ClientSessionID) alongside the gateway-negotiated session_id -- see the
// Session Timeline ownership rule in docs/observability.md. Idempotent;
// rows written before carry "".
var alterMCPAccessLogsClientSessionID = []string{
	`ALTER TABLE mcp_access_logs ADD COLUMN IF NOT EXISTS client_session_id String`,
}

// alterMCPAccessLogsSkillName upgrades an existing mcp_access_logs table
// to carry the skill or command a request loaded (sink.AccessLog.
// SkillName): a gateway__skill tools/call, or a native command's
// prompts/get -- see docs/profiles.md, "Skills and commands". Idempotent;
// rows written before carry "".
var alterMCPAccessLogsSkillName = []string{
	`ALTER TABLE mcp_access_logs ADD COLUMN IF NOT EXISTS skill_name String`,
}

// alterMCPAccessLogsIngest and alterLLMCallsIngest upgrade an existing
// table (created before the interceptor-ingest feature) to carry source
// and `user` -- see the CREATE TABLE column doc comment above for what
// they mean and why `user` must stay backtick-quoted. Idempotent; rows
// written before read back as source='gateway', user=”.
var alterMCPAccessLogsIngest = []string{
	`ALTER TABLE mcp_access_logs ADD COLUMN IF NOT EXISTS source LowCardinality(String) DEFAULT 'gateway'`,
	"ALTER TABLE mcp_access_logs ADD COLUMN IF NOT EXISTS `user` String DEFAULT ''",
}

var alterLLMCallsIngest = []string{
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS source LowCardinality(String) DEFAULT 'gateway'`,
	"ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS `user` String DEFAULT ''",
}

// alterLLMCallsDiscovery upgrades an existing llm_calls table to carry the
// skills and MCP tools a response asked to use (sink.LLMCall.SkillsUsed /
// MCPToolsUsed; see internal/discovery). Idempotent; older rows read back [].
var alterLLMCallsDiscovery = []string{
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS skills_used Array(LowCardinality(String))`,
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS mcp_tools_used Array(String)`,
}

// applyMigrations creates both tables if they don't already exist, then
// applies in-place column upgrades. Idempotent: safe to run on every startup.
func applyMigrations(ctx context.Context, c conn) error {
	stmts := []string{migrationMCPAccessLogs, migrationLLMCalls}
	stmts = append(stmts, alterLLMCallsCostUSD...)
	stmts = append(stmts, alterLLMCallsBodyRef...)
	stmts = append(stmts, alterLLMCallsClientIP...)
	stmts = append(stmts, alterLLMCallsModelRegistry...)
	stmts = append(stmts, alterLLMCallsClientInfo...)
	stmts = append(stmts, alterLLMCallsDiscovery...)
	stmts = append(stmts, alterMCPAccessLogsClientSessionID...)
	stmts = append(stmts, alterMCPAccessLogsSkillName...)
	stmts = append(stmts, alterMCPAccessLogsIngest...)
	stmts = append(stmts, alterLLMCallsIngest...)
	for _, stmt := range stmts {
		if err := c.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}
