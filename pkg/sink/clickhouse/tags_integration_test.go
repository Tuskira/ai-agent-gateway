//go:build integration

package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// user and tags must reach an llm_calls table created BEFORE the tags column
// existed (ADD COLUMN IF NOT EXISTS), leave old rows with an empty map, and
// round-trip through the writer, both readers and the llm_usage_canonical
// view (which gains `user` and tags). Runs in a database of its own.
func TestIntegration_TagsColumnUpgradeAndRoundTrip(t *testing.T) {
	cfg := freshDatabase(t, testConfig(t))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := openRawConn(t, cfg)

	// The schema as the previous release applied it: every statement except
	// the tags ALTER, with the view in its previous shape (no user, tags).
	oldView := strings.NewReplacer(",\n\t`user`,\n\ttags\nFROM (", "\nFROM (", ",\n\t\t`user`, tags\n\tFROM llm_calls", "\n\tFROM llm_calls").Replace(migrationUsageCanonical)
	if oldView == migrationUsageCanonical {
		t.Fatal("test setup: could not derive the previous view definition")
	}
	for _, stmt := range schemaStatements() {
		if stmt == migrationUsageCanonical {
			stmt = oldView
		}
		if stmt == alterLLMCallsTags[0] {
			continue
		}
		if err := c.Exec(ctx, stmt); err != nil {
			t.Fatalf("apply old schema: %v\n%s", err, stmt)
		}
	}
	tenant := uniqueTenantID(t)
	if err := c.Exec(ctx, "INSERT INTO llm_calls (timestamp, request_id, tenant_id, model, status_code, requested_model) VALUES (now64(3), 'old-row', '"+tenant+"', 'm', 200, 'm')"); err != nil {
		t.Fatalf("insert old row: %v", err)
	}

	// Starting the gateway applies the missing ALTER and replaces the view.
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New on an old-shape schema: %v", err)
	}
	defer s.Close()
	reader := s.(analytics.Reader)

	now := time.Now().UTC()
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, TenantID: tenant, RequestID: "new-row", Provider: "anthropic", Model: "m", RequestedModel: "m", StatusCode: 200,
		InputTokens: 10, OutputTokens: 5, User: "alice@example.com", Tags: map[string]string{"team": "search", "feature": "café"}})
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, TenantID: tenant, RequestID: "plain-row", Provider: "anthropic", Model: "m", RequestedModel: "m", StatusCode: 200})
	waitForRowCounts(t, ctx, reader, tenant, 0, 3)

	got, err := reader.GetLLMCall(ctx, tenant, "new-row")
	if err != nil || got.User != "alice@example.com" || got.Tags["team"] != "search" || got.Tags["feature"] != "café" || len(got.Tags) != 2 {
		t.Fatalf("GetLLMCall = user %q tags %v err %v", got.User, got.Tags, err)
	}
	list, _, err := reader.ListLLMCalls(ctx, tenant, analytics.LLMCallFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]sink.LLMCall{}
	for _, l := range list {
		byID[l.RequestID] = l
	}
	if l := byID["new-row"]; l.User != "alice@example.com" || len(l.Tags) != 2 {
		t.Errorf("list new-row = user %q tags %v", l.User, l.Tags)
	}
	for _, id := range []string{"old-row", "plain-row"} {
		if l := byID[id]; l.User != "" || len(l.Tags) != 0 {
			t.Errorf("list %s = user %q tags %v, want none", id, l.User, l.Tags)
		}
	}

	// The view carries `user` and tags, so cost attribution is one GROUP BY.
	var user, team string
	var tokens uint64
	if err := c.QueryRow(ctx, "SELECT `user`, tags['team'], total_tokens FROM llm_usage_canonical WHERE tenant_id = ? AND request_id = 'new-row'", tenant).Scan(&user, &team, &tokens); err != nil {
		t.Fatalf("query view: %v", err)
	}
	if user != "alice@example.com" || team != "search" || tokens != 15 {
		t.Errorf("view row = %q %q %d", user, team, tokens)
	}
	var n uint64
	if err := c.QueryRow(ctx, "SELECT count() FROM llm_usage_canonical WHERE tenant_id = ? AND mapContains(tags, 'team')", tenant).Scan(&n); err != nil || n != 1 {
		t.Errorf("tagged rows in view = %d, err %v", n, err)
	}
}
