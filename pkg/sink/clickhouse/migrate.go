package clickhouse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
)

// migrationMCPAccessLogs and migrationLLMCalls are the sink's one baseline
// migration: two idempotent CREATE TABLE IF NOT EXISTS statements, applied
// by ensureSchema / Migrate (see below). Column order here is the contract
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

// alterLLMCallsProfile upgrades an existing llm_calls table to carry
// sink.LLMCall.Profile: the agent profile that made the call (the API
// key's bound profile, else the caller's X-Agent-Profile-Name header), the
// LLM-plane twin of mcp_access_logs.profile. Idempotent; rows written
// before carry "".
var alterLLMCallsProfile = []string{
	`ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS profile String`,
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

// migrationUsageCanonical is the one definition of LLM token usage that
// every token-monitoring query reads, so totals, breakdowns and charts can
// never disagree:
//
//   - model is the name the caller asked for (requested_model, else model),
//     so a registry alias is one row whatever target answered;
//   - provider is the one the call was costed as -- the registry target's
//     vendor (openai_compat costs as openai; a label as itself), else the
//     client's dialect -- and stored tokens follow its convention: for
//     openai and gemini input INCLUDES the cache reads (pkg/pricing
//     tokenCost, internal/llmplane foldsCache), so prompt_tokens subtracts
//     them back, never below zero;
//   - total_tokens = prompt + completion; cache reads/writes are separate;
//   - only usage counts: calls refused before any target (fallback_index
//     -1), failed calls, and the free token-count / batch-management paths
//     are excluded -- the same list the LLM plane leaves unpriced
//     (pricing.UnpricedPath), so the two cannot drift.
//
// CREATE OR REPLACE keeps it current whenever the schema is applied (see
// ensureSchema); it reads columns added by the ALTERs above, so it runs
// after them.
var migrationUsageCanonical = `
CREATE OR REPLACE VIEW llm_usage_canonical AS
SELECT
	timestamp,
	request_id,
	tenant_id,
	key_id,
	session_id,
	source,
	profile,
	caller_model AS model,
	pricing_provider AS provider,
	if(pricing_provider IN ('openai', 'gemini'),
		if(input_tokens > cache_read_tokens, toUInt64(input_tokens - cache_read_tokens), toUInt64(0)),
		input_tokens) AS prompt_tokens,
	output_tokens AS completion_tokens,
	cache_read_tokens,
	cache_creation_tokens AS cache_write_tokens,
	prompt_tokens + completion_tokens AS total_tokens,
	cost_usd,
	toBool(isNotNull(cost_usd)) AS priced
FROM (
	SELECT
		timestamp, request_id, tenant_id, key_id, session_id, source, profile,
		if(requested_model != '', requested_model, model) AS caller_model,
		multiIf(resolved_vendor = '', provider, resolved_vendor = 'openai_compat', 'openai', resolved_vendor) AS pricing_provider,
		input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cost_usd
	FROM llm_calls
	WHERE fallback_index != -1
		AND status_code < 400
		AND NOT (` + unpricedPathSQL() + `)
)
`

// unpricedPathSQL is pricing.UnpricedPath as a ClickHouse condition on path.
// ponytail: the list is Go constants with no quotes or backslashes, so plain
// quoting is enough; escape here if it ever takes outside input.
func unpricedPathSQL() string {
	conds := make([]string, 0, len(pricing.UnpricedPathSuffixes)+1)
	for _, s := range pricing.UnpricedPathSuffixes {
		conds = append(conds, "endsWith(path, '"+s+"')")
	}
	return strings.Join(append(conds, "position(path, '"+pricing.UnpricedPathSegment+"') > 0"), " OR ")
}

// ledgerTable records which schema versions have been applied to this
// database, so a gateway start can skip DDL entirely once the schema is in
// place (see ensureSchema). A version is a fingerprint of the statements
// themselves (schemaFingerprint), so adding or changing a migration changes
// it with no version number to bump. Plain MergeTree like the sink's other
// tables, so it works unchanged on single-node and replicated or managed
// ClickHouse with no ON CLUSTER. One row per apply; duplicates (several
// pods applying a fresh schema at once, or one per release) are harmless.
const ledgerTable = "gateway_schema_migrations"

const migrationLedger = `
CREATE TABLE IF NOT EXISTS ` + ledgerTable + ` (
	fingerprint String,
	gateway_version String,
	statements UInt32,
	applied_at DateTime64(3)
)
ENGINE = MergeTree
ORDER BY (fingerprint, applied_at)
`

// schemaStatements is the sink's whole schema, in apply order: the ledger,
// both tables, the in-place column upgrades, then the view (which reads
// columns the ALTERs add). Every statement is idempotent.
func schemaStatements() []string {
	stmts := []string{migrationLedger, migrationMCPAccessLogs, migrationLLMCalls}
	stmts = append(stmts, alterLLMCallsCostUSD...)
	stmts = append(stmts, alterLLMCallsBodyRef...)
	stmts = append(stmts, alterLLMCallsClientIP...)
	stmts = append(stmts, alterLLMCallsModelRegistry...)
	stmts = append(stmts, alterLLMCallsClientInfo...)
	stmts = append(stmts, alterLLMCallsDiscovery...)
	stmts = append(stmts, alterLLMCallsProfile...)
	stmts = append(stmts, alterMCPAccessLogsClientSessionID...)
	stmts = append(stmts, alterMCPAccessLogsSkillName...)
	stmts = append(stmts, alterMCPAccessLogsIngest...)
	stmts = append(stmts, alterLLMCallsIngest...)
	return append(stmts, migrationUsageCanonical)
}

// schemaFingerprint is the hex sha256 of stmts in order, each terminated by
// a NUL byte so statement boundaries are part of the hash.
func schemaFingerprint(stmts []string) string {
	h := sha256.New()
	for _, s := range stmts {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ensureSchema is the gateway-start path (New). It first asks the ledger
// whether the current schema fingerprint is already recorded; if so it
// issues no DDL at all, which is what keeps many replicas starting at once
// from racing each other's ALTERs on replicated ClickHouse. Otherwise
// (fresh install, or `gateway migrate` did not run for this release) it
// applies the schema itself, with retry, and records it.
func ensureSchema(ctx context.Context, c conn, version string, logger *slog.Logger) error {
	stmts := schemaStatements()
	fp := schemaFingerprint(stmts)
	applied, err := schemaApplied(ctx, c, fp, logger)
	if err != nil {
		return fmt.Errorf("check schema ledger: %w", err)
	}
	if applied {
		logger.Info("clickhouse sink: schema already applied, skipping migrations", "fingerprint", fp)
		return nil
	}
	logger.Info("clickhouse sink: schema not recorded, applying migrations", "fingerprint", fp)
	return applySchema(ctx, c, stmts, fp, version, logger)
}

// schemaApplied reports whether fp is recorded in the ledger. A missing
// ledger table (code 60, UNKNOWN_TABLE) means nothing has been recorded.
func schemaApplied(ctx context.Context, c conn, fp string, logger *slog.Logger) (bool, error) {
	var n uint64
	err := withRetry(ctx, logger, "SELECT FROM "+ledgerTable, func(ctx context.Context) error {
		return c.QueryRow(ctx, "SELECT count() FROM "+ledgerTable+" WHERE fingerprint = ?", fp).Scan(&n)
	})
	if err != nil {
		var ex *clickhouse.Exception
		if errors.As(err, &ex) && ex.Code == codeUnknownTable {
			return false, nil
		}
		return false, err
	}
	return n > 0, nil
}

// applySchema runs stmts in order, each with withRetry, then records fp in
// the ledger. Used both by ensureSchema and, unconditionally, by Migrate.
func applySchema(ctx context.Context, c conn, stmts []string, fp, version string, logger *slog.Logger) error {
	for _, stmt := range stmts {
		if err := withRetry(ctx, logger, summarize(stmt), func(ctx context.Context) error {
			return c.Exec(ctx, stmt)
		}); err != nil {
			return fmt.Errorf("%s: %w", summarize(stmt), err)
		}
	}
	err := withRetry(ctx, logger, "INSERT INTO "+ledgerTable, func(ctx context.Context) error {
		return c.Exec(ctx, "INSERT INTO "+ledgerTable+" (fingerprint, gateway_version, statements, applied_at) SELECT ?, ?, ?, now64(3)",
			fp, version, uint32(len(stmts)))
	})
	if err != nil {
		return fmt.Errorf("record schema in %s: %w", ledgerTable, err)
	}
	logger.Info("clickhouse sink: schema applied", "fingerprint", fp, "statements", len(stmts))
	return nil
}

// summarize returns a statement's first words, for logs and errors.
func summarize(stmt string) string {
	f := strings.Fields(stmt)
	if len(f) > 9 {
		f = f[:9]
	}
	return strings.Join(f, " ")
}

const codeUnknownTable = 60

// retryableCodes are ClickHouse error codes after which the same schema
// statement can succeed if simply sent again:
//
//   - 517 CANNOT_ASSIGN_ALTER: on replicated tables, the replica serving
//     the ALTER has not caught up with an ALTER another client just made;
//     the server's message itself says "Please retry this query". This is
//     what several replicas starting at once hit.
//   - 473 DEADLOCK_AVOIDED: a table lock wait timed out while another DDL
//     held it ("Possible deadlock avoided. Client should retry").
//   - 242 TABLE_IS_READ_ONLY: a replicated table is read-only while its
//     replica re-establishes its coordination (Keeper) session.
//   - 999 KEEPER_EXCEPTION: the coordination session was lost or timed out
//     mid-DDL; bounded retries cover reconnects, a persistent fault still
//     fails after the last attempt.
var retryableCodes = map[int32]string{
	517: "CANNOT_ASSIGN_ALTER",
	473: "DEADLOCK_AVOIDED",
	242: "TABLE_IS_READ_ONLY",
	999: "KEEPER_EXCEPTION",
}

// retryPolicy bounds withRetry. The defaults sum to at most ~16s of
// waiting per statement, inside New's 30s budget; ctx cuts it shorter.
type retryPolicy struct {
	attempts  int
	baseDelay time.Duration
	maxDelay  time.Duration
}

var schemaRetry = retryPolicy{attempts: 8, baseDelay: 250 * time.Millisecond, maxDelay: 4 * time.Second}

// withRetry calls fn until it succeeds, fails with an error that is not a
// retryable ClickHouse exception, runs out of attempts, or ctx is done.
// Each retry is logged with the error code and attempt number.
func withRetry(ctx context.Context, logger *slog.Logger, what string, fn func(context.Context) error) error {
	return schemaRetry.do(ctx, logger, what, fn)
}

func (p retryPolicy) do(ctx context.Context, logger *slog.Logger, what string, fn func(context.Context) error) error {
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		var ex *clickhouse.Exception
		if !errors.As(err, &ex) {
			return err
		}
		name, ok := retryableCodes[ex.Code]
		if !ok || attempt >= p.attempts {
			return err
		}
		delay := p.backoff(attempt)
		logger.Warn("clickhouse sink: schema statement failed with a retryable error, retrying",
			"statement", what, "code", ex.Code, "code_name", name,
			"attempt", attempt, "max_attempts", p.attempts, "retry_in", delay, "error", err)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w (retry stopped: %w)", err, ctx.Err())
		case <-t.C:
		}
	}
}

// backoff doubles from baseDelay per attempt up to maxDelay, then picks a
// random point in its upper half so replicas retrying together spread out.
func (p retryPolicy) backoff(attempt int) time.Duration {
	d := p.baseDelay << (attempt - 1)
	if d <= 0 || d > p.maxDelay {
		d = p.maxDelay
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + rand.N(half+1)
}
