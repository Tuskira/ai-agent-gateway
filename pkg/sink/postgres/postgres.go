// Package postgres is a sink.BatchSink that persists LLMCall rows to a Postgres
// table — the lossless, single-DB capture store. The recorder writes each call
// synchronously through it, committed before the request completes, so nothing
// is lost on a crash. WriteBatch is idempotent (request_id primary key + ON
// CONFLICT DO NOTHING), so a retried capture is a no-op rather than a duplicate.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// Options configures the Postgres sink.
type Options struct {
	DSN   string // pgx-compatible connection URL (e.g. config.Database.DSN())
	Table string // default "llm_calls"
}

// Sink writes captured calls to a Postgres table. It implements sink.BatchSink.
type Sink struct {
	db    *sql.DB
	table string
}

var (
	_ sink.BatchSink   = (*Sink)(nil)
	_ sink.SpendReader = (*Sink)(nil)
)

func createTableSQL(table string) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  timestamp             timestamptz NOT NULL,
  request_id            text PRIMARY KEY,
  tenant_id             text,
  principal             text,
  key_id                text,
  session_id            text,
  provider              text,
  model                 text,
  path                  text,
  status_code           integer,
  duration_ms           bigint,
  stream                boolean,
  input_tokens          bigint,
  output_tokens         bigint,
  cache_read_tokens     bigint,
  cache_creation_tokens bigint,
  stop_reason           text,
  upstream_host         text,
  provider_request_id   text,
  headers               text,
  -- bytea, not text: response bodies can be binary (e.g. Bedrock's
  -- application/vnd.amazon.eventstream frames contain 0x00, which a text
  -- column rejects as an invalid UTF-8 byte sequence).
  request_body          bytea,
  response_body         bytea,
  messages              text,
  system                text,
  tools                 text,
  truncated             boolean,
  body_ref              text,
  error                 text,
  cost_usd              numeric(20,10),
  client_name           text,
  user_agent            text,
  -- Model registry (docs/llm-plane.md "Model registry"): what the client
  -- asked for vs. where the call went.
  requested_model       text,
  resolved_vendor       text,
  resolved_model        text,
  translated            boolean NOT NULL DEFAULT false,
  fallback_index        integer NOT NULL DEFAULT 0
)`, table)
}

// New connects, ensures the table exists, and returns the sink.
func New(ctx context.Context, o Options) (*Sink, error) {
	if o.Table == "" {
		o.Table = "llm_calls"
	}
	db, err := sql.Open("pgx", o.DSN)
	if err != nil {
		return nil, fmt.Errorf("sink(postgres): open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("sink(postgres): ping: %w", err)
	}
	if _, err := db.ExecContext(ctx, createTableSQL(o.Table)); err != nil {
		return nil, fmt.Errorf("sink(postgres): create table: %w", err)
	}
	// Idempotent upgrade for tables created before cost_usd existed. numeric
	// (exact decimal) rather than double precision, so a fractional-cent cost is
	// stored/shown as a plain decimal (0.000054) not float scientific notation.
	if _, err := db.ExecContext(ctx, "ALTER TABLE "+o.Table+" ADD COLUMN IF NOT EXISTS cost_usd numeric(20,10)"); err != nil {
		return nil, fmt.Errorf("sink(postgres): add cost_usd column: %w", err)
	}
	// Idempotent upgrade for tables created before the model registry
	// columns existed. Defaults keep old rows meaningful: an unregistered
	// call has fallback_index 0 and was not translated.
	for _, col := range []string{
		"requested_model text",
		"resolved_vendor text",
		"resolved_model text",
		"translated boolean NOT NULL DEFAULT false",
		"fallback_index integer NOT NULL DEFAULT 0",
	} {
		if _, err := db.ExecContext(ctx, "ALTER TABLE "+o.Table+" ADD COLUMN IF NOT EXISTS "+col); err != nil {
			return nil, fmt.Errorf("sink(postgres): add %s column: %w", strings.Fields(col)[0], err)
		}
	}
	// Idempotent upgrade for tables created before ClientName/UserAgent
	// existed (sink.LLMCall.ClientName/UserAgent). Old rows carry "".
	for _, col := range []string{"client_name text", "user_agent text", "skills_used text[]", "mcp_tools_used text[]"} {
		if _, err := db.ExecContext(ctx, "ALTER TABLE "+o.Table+" ADD COLUMN IF NOT EXISTS "+col); err != nil {
			return nil, fmt.Errorf("sink(postgres): add %s column: %w", strings.Fields(col)[0], err)
		}
	}
	// The budget lookup (KeySpend) filters by (tenant_id, key_id, timestamp);
	// without this index it would scan the whole capture table every 10 s
	// per budgeted key. Built once, on first boot after the upgrade.
	if _, err := db.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS "+spendIndexName(o.Table)+
		" ON "+o.Table+" (tenant_id, key_id, timestamp)"); err != nil {
		return nil, fmt.Errorf("sink(postgres): create spend index: %w", err)
	}
	// Same for the per-model budget lookup (ModelSpend).
	if _, err := db.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS "+modelSpendIndexName(o.Table)+
		" ON "+o.Table+" (tenant_id, requested_model, timestamp)"); err != nil {
		return nil, fmt.Errorf("sink(postgres): create model spend index: %w", err)
	}
	return &Sink{db: db, table: o.Table}, nil
}

// spendIndexName derives the KeySpend index name from the table name
// (which may be schema-qualified): identifier-safe characters only.
func spendIndexName(table string) string { return indexPrefix(table) + "_tenant_key_ts_idx" }

// modelSpendIndexName is the ModelSpend index name, derived the same way.
func modelSpendIndexName(table string) string { return indexPrefix(table) + "_tenant_model_ts_idx" }

func indexPrefix(table string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, table)
}

// KeySpend implements sink.SpendReader: one indexed range scan over the
// month, with the day as a FILTERed sub-sum. NULL cost_usd (unpriced model,
// unreadable usage) is skipped by SUM, i.e. counts as 0.
func (s *Sink) KeySpend(ctx context.Context, tenantID, keyID string, dayStart, monthStart time.Time) (day, month float64, err error) {
	q := `SELECT COALESCE(SUM(cost_usd) FILTER (WHERE timestamp >= $4), 0)::float8,
       COALESCE(SUM(cost_usd), 0)::float8
FROM ` + s.table + ` WHERE tenant_id = $1 AND key_id = $2 AND timestamp >= $3`
	if err := s.db.QueryRowContext(ctx, q, tenantID, keyID, monthStart, dayStart).Scan(&day, &month); err != nil {
		return 0, 0, fmt.Errorf("sink(postgres): key spend: %w", err)
	}
	return day, month, nil
}

// ModelSpend implements sink.SpendReader for one tenant's calls to one
// requested model name: the same query as KeySpend, keyed on
// (tenant_id, requested_model).
func (s *Sink) ModelSpend(ctx context.Context, tenantID, requestedModel string, dayStart, monthStart time.Time) (day, month float64, err error) {
	q := `SELECT COALESCE(SUM(cost_usd) FILTER (WHERE timestamp >= $4), 0)::float8,
       COALESCE(SUM(cost_usd), 0)::float8
FROM ` + s.table + ` WHERE tenant_id = $1 AND requested_model = $2 AND timestamp >= $3`
	if err := s.db.QueryRowContext(ctx, q, tenantID, requestedModel, monthStart, dayStart).Scan(&day, &month); err != nil {
		return 0, 0, fmt.Errorf("sink(postgres): model spend: %w", err)
	}
	return day, month, nil
}

const insertCols = `(timestamp, request_id, tenant_id, principal, key_id, session_id,
  provider, model, path, status_code, duration_ms, stream,
  input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
  stop_reason, upstream_host, provider_request_id,
  headers, request_body, response_body, messages, system, tools,
  truncated, body_ref, error, cost_usd,
  requested_model, resolved_vendor, resolved_model, translated, fallback_index,
  client_name, user_agent, skills_used, mcp_tools_used)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38)
ON CONFLICT (request_id) DO NOTHING`

// WriteBatch inserts calls synchronously, returning any error to the caller.
// Duplicate request_ids are no-ops (idempotent retry). The common single-call
// write is one autocommit statement (one round trip, on the response path); a
// multi-call batch runs in one transaction.
func (s *Sink) WriteBatch(ctx context.Context, calls []*sink.LLMCall) error {
	switch len(calls) {
	case 0:
		return nil
	case 1:
		if _, err := s.db.ExecContext(ctx, s.insertSQL(), insertArgs(calls[0])...); err != nil {
			return fmt.Errorf("sink(postgres): insert: %w", err)
		}
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sink(postgres): begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx, s.insertSQL())
	if err != nil {
		return fmt.Errorf("sink(postgres): prepare: %w", err)
	}
	defer stmt.Close()

	for _, c := range calls {
		if _, err := stmt.ExecContext(ctx, insertArgs(c)...); err != nil {
			return fmt.Errorf("sink(postgres): insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sink(postgres): commit: %w", err)
	}
	return nil
}

func (s *Sink) insertSQL() string { return "INSERT INTO " + s.table + " " + insertCols }

// insertArgs maps c onto insertCols. Every text column goes through pgText:
// several are caller-controlled (session header, URL path, request JSON), and
// Postgres rejects the whole row on one invalid UTF-8 byte or NUL, which would
// let a caller keep their call out of the log.
func insertArgs(c *sink.LLMCall) []any {
	headers, _ := json.Marshal(c.Headers)
	return []any{
		c.Timestamp, pgText(c.RequestID), pgText(c.TenantID), pgText(c.Principal), pgText(c.KeyID), pgText(c.SessionID),
		pgText(c.Provider), pgText(c.Model), pgText(c.Path), c.StatusCode, c.DurationMS, c.Stream,
		c.InputTokens, c.OutputTokens, c.CacheReadTokens, c.CacheCreationTokens,
		pgText(c.StopReason), pgText(c.UpstreamHost), pgText(c.ProviderRequestID),
		pgText(string(headers)), c.RequestBody, c.ResponseBody,
		pgText(string(c.Messages)), pgText(string(c.System)), pgText(string(c.Tools)),
		c.Truncated, pgText(c.BodyRef), pgText(c.Error), c.CostUSD,
		pgText(c.RequestedModel), pgText(c.ResolvedVendor), pgText(c.ResolvedModel), c.Translated, c.FallbackIndex,
		pgText(c.ClientName), pgText(c.UserAgent),
		pgTextArray(c.SkillsUsed), pgTextArray(c.MCPToolsUsed),
	}
}

// pgTextArray is pgText for each element of a text[] value (never nil, so
// the column holds {} rather than NULL).
func pgTextArray(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = pgText(s)
	}
	return out
}

// pgText makes s storable in a Postgres text column: invalid UTF-8 becomes
// U+FFFD and NUL bytes are dropped. Bodies are bytea and stored verbatim.
func pgText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "")
}

func (s *Sink) Close() error { return s.db.Close() }
