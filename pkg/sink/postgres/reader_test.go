package postgres

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// The capture table serves LLM Logs when there is no ClickHouse: list
// (no bodies, filtered, newest first, tenant-scoped) and get (with bodies),
// including a row written before most columns existed. Runs against a real
// Postgres (GATEWAY_TEST_DATABASE_URL), in a table of its own.
func TestIntegration_LLMCallReader(t *testing.T) {
	dsn := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping the Postgres sink integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	table := "llm_calls_reader_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	s, err := New(ctx, Options{DSN: dsn, Table: table})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t.Cleanup(func() {
		db, _ := sql.Open("pgx", dsn)
		_, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+table)
		db.Close()
	})

	t0 := time.Now().UTC().Truncate(time.Millisecond)
	cost := 0.0123
	calls := []*sink.LLMCall{
		{Timestamp: t0, RequestID: "r1", TenantID: "t", SessionID: "s1", Model: "m", StatusCode: 200, InputTokens: 5,
			CostUSD: &cost, RequestBody: []byte(`{"messages":[]}`), ResponseBody: []byte("ok"),
			Messages: []byte(`[{"role":"user","content":"hi"}]`), Headers: map[string]string{"x-a": "1"},
			ClientName: "claude-code", UserAgent: "claude-cli/2.1.0", RequestedModel: "sonnet", FallbackIndex: 1},
		{Timestamp: t0.Add(time.Second), RequestID: "r2", TenantID: "t", SessionID: "s1", Model: "m", StatusCode: 400, Error: "upstream: bad request"},
		{Timestamp: t0.Add(2 * time.Second), RequestID: "r3", TenantID: "t", SessionID: "s2", Model: "m", StatusCode: 200},
		{Timestamp: t0, RequestID: "other", TenantID: "u", SessionID: "s1", Model: "m", StatusCode: 200},
	}
	if err := s.WriteBatch(ctx, calls); err != nil {
		t.Fatal(err)
	}
	// A row from before most columns existed: NULLs must read as zero values.
	if _, err := s.db.ExecContext(ctx, "INSERT INTO "+table+" (timestamp, request_id, tenant_id) VALUES ($1, 'bare', 't')", t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	list, total, err := s.ListLLMCalls(ctx, "t", analytics.LLMCallFilter{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(list) != 2 || list[0].RequestID != "r2" || list[1].RequestID != "r1" {
		t.Fatalf("session list = %d %+v", total, list)
	}
	r1 := list[1]
	if r1.RequestBody != nil || r1.CostUSD == nil || *r1.CostUSD != cost || r1.ClientName != "claude-code" ||
		r1.UserAgent != "claude-cli/2.1.0" || r1.RequestedModel != "sonnet" || r1.FallbackIndex != 1 || list[0].Error != "upstream: bad request" {
		t.Errorf("list row = %+v", r1)
	}
	all, total, err := s.ListLLMCalls(ctx, "t", analytics.LLMCallFilter{})
	if err != nil || total != 4 || len(all) != 4 || all[3].RequestID != "bare" {
		t.Errorf("tenant list: total %d, rows %d, err %v", total, len(all), err)
	}
	for _, c := range all {
		if c.TenantID != "t" {
			t.Errorf("tenant t list leaked %s of tenant %q", c.RequestID, c.TenantID)
		}
	}
	// Pagination: total counts every match, the page holds limit rows.
	if page, total, err := s.ListLLMCalls(ctx, "t", analytics.LLMCallFilter{Limit: 1, Offset: 1}); err != nil || total != 4 || len(page) != 1 || page[0].RequestID != "r2" {
		t.Errorf("page 2 of 1 = %d %+v, %v", total, page, err)
	}
	if got, total, err := s.ListLLMCalls(ctx, "t", analytics.LLMCallFilter{ClientName: "claude-code", Status: 200, Model: "m"}); err != nil || total != 1 || got[0].RequestID != "r1" {
		t.Errorf("client filter = %d %+v, %v", total, got, err)
	}
	if got, total, err := s.ListLLMCalls(ctx, "t", analytics.LLMCallFilter{From: t0.Add(500 * time.Millisecond), To: t0.Add(1500 * time.Millisecond)}); err != nil || total != 1 || got[0].RequestID != "r2" {
		t.Errorf("time window = %d %+v, %v", total, got, err)
	}
	if got, total, err := s.ListLLMCalls(ctx, "u", analytics.LLMCallFilter{}); err != nil || total != 1 || got[0].RequestID != "other" {
		t.Errorf("tenant u list = %d %+v, %v", total, got, err)
	}

	got, err := s.GetLLMCall(ctx, "t", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.RequestBody) != `{"messages":[]}` || string(got.ResponseBody) != "ok" || got.Headers["x-a"] != "1" ||
		!strings.Contains(string(got.Messages), `"hi"`) || !got.Timestamp.Equal(t0) || got.ClientName != "claude-code" {
		t.Errorf("get = %+v", got)
	}
	if bare, err := s.GetLLMCall(ctx, "t", "bare"); err != nil || bare.Model != "" || bare.CostUSD != nil || bare.Messages != nil {
		t.Errorf("bare row = %+v, %v", bare, err)
	}
	if _, err := s.GetLLMCall(ctx, "t", "other"); !errors.Is(err, analytics.ErrNotFound) {
		t.Errorf("cross-tenant get: %v, want ErrNotFound", err)
	}
}
