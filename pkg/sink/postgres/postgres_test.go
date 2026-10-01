package postgres

import (
	"context"
	"database/sql"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// Response bodies can be binary (Bedrock's application/vnd.amazon.eventstream
// frames contain 0x00), which a Postgres text column rejects as invalid UTF-8.
// The body columns must be bytea.
func TestBodyColumnsAreBytea(t *testing.T) {
	ddl := createTableSQL("llm_calls")
	for _, col := range []string{"request_body", "response_body"} {
		if !regexp.MustCompile(col + `\s+bytea`).MatchString(ddl) {
			t.Errorf("%s must be bytea, not text (binary bodies contain 0x00)", col)
		}
	}
}

// A caller-controlled byte that Postgres rejects in a text column (invalid
// UTF-8, NUL) must not fail the insert and drop the whole call.
func TestPGText(t *testing.T) {
	for in, want := range map[string]string{
		"review-\xff":      "review-\uFFFD",
		"a\x00b":           "ab",
		"plain":            "plain",
		"héllo \u2603":     "héllo \u2603",
		"\xc3\x28\x00tail": "\uFFFD(tail",
	} {
		if got := pgText(in); got != want {
			t.Errorf("pgText(%q) = %q, want %q", in, got, want)
		}
	}
}

// insertArgs must supply exactly one value per placeholder, and sanitize the
// caller-controlled text fields.
func TestInsertArgs(t *testing.T) {
	c := &sink.LLMCall{SessionID: "s\xff", Path: "/v1/messages\x00", Messages: []byte("[\"\xff\"]"), RequestBody: []byte{0, 0xff}}
	args := insertArgs(c)
	if n := strings.Count(insertCols, "$"); len(args) != n {
		t.Fatalf("insertArgs has %d values, insertCols has %d placeholders", len(args), n)
	}
	if args[5] != "s\uFFFD" || args[8] != "/v1/messages" || args[22] != "[\"\uFFFD\"]" {
		t.Errorf("text fields not sanitized: session=%q path=%q messages=%q", args[5], args[8], args[22])
	}
	if b, _ := args[20].([]byte); len(b) != 2 {
		t.Errorf("bytea request body must be stored verbatim, got %v", args[20])
	}

	// An offloaded call writes its ref to body_ref and NULL bodies.
	args = insertArgs(&sink.LLMCall{BodyRef: "s3://b/llm-bodies/r1"})
	if args[26] != "s3://b/llm-bodies/r1" || args[20].([]byte) != nil || args[21].([]byte) != nil {
		t.Errorf("offloaded row: body_ref=%v request_body=%v response_body=%v", args[26], args[20], args[21])
	}

	// client_name/user_agent are the last two columns and go through the
	// same pgText sanitization as every other caller-controlled string.
	args = insertArgs(&sink.LLMCall{ClientName: "claude-code", UserAgent: "claude-cli/2.1.0 (external, cli)\xff"})
	if args[34] != "claude-code" || args[35] != "claude-cli/2.1.0 (external, cli)�" {
		t.Errorf("client_name=%v user_agent=%v", args[34], args[35])
	}

	// skills_used / mcp_tools_used are the last two columns: text[] values,
	// sanitized per element and never nil (the column holds {} not NULL).
	args = insertArgs(&sink.LLMCall{SkillsUsed: []string{"review-pr\xff"}, MCPToolsUsed: []string{"gw__langfuse__get_trace"}})
	if got, _ := args[36].([]string); len(got) != 1 || got[0] != "review-pr�" {
		t.Errorf("skills_used = %#v", args[36])
	}
	if got, _ := args[37].([]string); len(got) != 1 || got[0] != "gw__langfuse__get_trace" {
		t.Errorf("mcp_tools_used = %#v", args[37])
	}
	args = insertArgs(&sink.LLMCall{})
	if got, ok := args[36].([]string); !ok || got == nil || len(got) != 0 {
		t.Errorf("empty skills_used = %#v, want non-nil empty []string", args[36])
	}
}

// The model registry columns must reach a table created BEFORE they
// existed (ADD COLUMN IF NOT EXISTS on every start), keep old rows
// readable with their defaults, and round-trip on new rows. Runs against
// a real Postgres (GATEWAY_TEST_DATABASE_URL), in a table of its own.
func TestIntegration_ModelRegistryColumnsUpgrade(t *testing.T) {
	dsn := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping the Postgres sink integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	table := "llm_calls_upgrade_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+table) })

	// The table as the previous release created it: no registry columns.
	ddl := createTableSQL(table)
	cut := strings.Index(ddl, ",\n  -- Model registry")
	if cut < 0 {
		t.Fatal("test setup: cannot find the model registry columns in the DDL")
	}
	old := ddl[:cut] + "\n)"
	if strings.Contains(old, "requested_model") {
		t.Fatalf("test setup: old DDL still has the new columns:\n%s", old)
	}
	if _, err := db.ExecContext(ctx, old); err != nil {
		t.Fatalf("create old-shape table: %v\n%s", err, old)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO "+table+" (timestamp, request_id, model) VALUES (now(), 'old-row', 'claude-haiku-4-5')"); err != nil {
		t.Fatalf("insert old row: %v", err)
	}

	s, err := New(ctx, Options{DSN: dsn, Table: table})
	if err != nil {
		t.Fatalf("New on an old-shape table: %v", err)
	}
	defer s.Close()
	// Idempotent: a second start changes nothing.
	s2, err := New(ctx, Options{DSN: dsn, Table: table})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	s2.Close()

	if err := s.WriteBatch(ctx, []*sink.LLMCall{{
		Timestamp: time.Now().UTC(), RequestID: "new-row", Provider: "anthropic", Model: "claude-sonnet-4-5",
		RequestedModel: "sonnet", ResolvedVendor: "bedrock", ResolvedModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		FallbackIndex: 1, StatusCode: 200,
	}}); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	type row struct {
		requested, vendor, resolved sql.NullString
		translated                  bool
		fallback                    int
	}
	read := func(id string) row {
		var r row
		if err := db.QueryRowContext(ctx, "SELECT requested_model, resolved_vendor, resolved_model, translated, fallback_index FROM "+table+" WHERE request_id = $1", id).
			Scan(&r.requested, &r.vendor, &r.resolved, &r.translated, &r.fallback); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return r
	}
	if r := read("old-row"); r.requested.Valid || r.translated || r.fallback != 0 {
		t.Errorf("old row after upgrade = %+v; want NULL requested_model, translated false, fallback_index 0", r)
	}
	if r := read("new-row"); r.requested.String != "sonnet" || r.vendor.String != "bedrock" || r.resolved.String != "us.anthropic.claude-sonnet-4-5-20250929-v1:0" || r.translated || r.fallback != 1 {
		t.Errorf("new row = %+v", r)
	}
}

// client_name/user_agent must reach a table created BEFORE they existed
// (ADD COLUMN IF NOT EXISTS on every start), keep old rows readable with
// their "" default, and round-trip on new rows. Runs against a real
// Postgres (GATEWAY_TEST_DATABASE_URL), in a table of its own.
func TestIntegration_ClientInfoColumnsUpgrade(t *testing.T) {
	dsn := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping the Postgres sink integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	table := "llm_calls_clientinfo_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+table) })

	// The table as a release before this feature created it: no
	// client_name/user_agent columns (and, as a side effect of cutting
	// there, no later columns either -- fine, this test doesn't touch them).
	ddl := createTableSQL(table)
	cut := strings.Index(ddl, ",\n  client_name")
	if cut < 0 {
		t.Fatal("test setup: cannot find the client info columns in the DDL")
	}
	old := ddl[:cut] + "\n)"
	if strings.Contains(old, "client_name") {
		t.Fatalf("test setup: old DDL still has the new columns:\n%s", old)
	}
	if _, err := db.ExecContext(ctx, old); err != nil {
		t.Fatalf("create old-shape table: %v\n%s", err, old)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO "+table+" (timestamp, request_id, model) VALUES (now(), 'old-row', 'claude-haiku-4-5')"); err != nil {
		t.Fatalf("insert old row: %v", err)
	}

	s, err := New(ctx, Options{DSN: dsn, Table: table})
	if err != nil {
		t.Fatalf("New on an old-shape table: %v", err)
	}
	defer s.Close()

	if err := s.WriteBatch(ctx, []*sink.LLMCall{{
		Timestamp: time.Now().UTC(), RequestID: "new-row", Provider: "anthropic", Model: "claude-sonnet-4-5",
		ClientName: "claude-code", UserAgent: "claude-cli/2.1.0 (external, cli)", StatusCode: 200,
	}}); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	read := func(id string) (clientName, userAgent sql.NullString) {
		if err := db.QueryRowContext(ctx, "SELECT client_name, user_agent FROM "+table+" WHERE request_id = $1", id).
			Scan(&clientName, &userAgent); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return
	}
	if cn, ua := read("old-row"); cn.Valid || ua.Valid {
		t.Errorf("old row after upgrade = client_name=%v user_agent=%v; want both NULL", cn, ua)
	}
	if cn, ua := read("new-row"); cn.String != "claude-code" || ua.String != "claude-cli/2.1.0 (external, cli)" {
		t.Errorf("new row = client_name=%v user_agent=%v", cn, ua)
	}
}
