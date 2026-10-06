//go:build integration

package clickhouse

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// canonicalRow is one llm_usage_canonical row as read back by the test.
type canonicalRow struct {
	RequestID        string
	Model            string
	Provider         string
	PromptTokens     uint64
	CompletionTokens uint64
	CacheReadTokens  uint64
	CacheWriteTokens uint64
	TotalTokens      uint64
	Priced           bool
}

// TestIntegration_UsageCanonicalView: the view is the one definition every
// token-monitoring query reads. Input is normalized to exclude cached
// tokens whatever the provider's convention, total = input + output, the
// model is the name the caller asked for, and refused calls, token-count
// endpoints and failed calls are not usage.
func TestIntegration_UsageCanonicalView(t *testing.T) {
	cfg := testConfig(t)
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()
	reader := s.(analytics.Reader)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	now := time.Now().UTC()
	tenantID := uniqueTenantID(t)
	cost := 0.01

	rows := []*sink.LLMCall{
		// Anthropic convention: input excludes cache reads.
		{RequestID: "a-anthropic", Provider: "anthropic", Model: "claude-sonnet-4-5", InputTokens: 100, OutputTokens: 50, CacheReadTokens: 1000, CacheCreationTokens: 200, StatusCode: 200, CostUSD: &cost},
		// OpenAI convention: input includes the 300 cached tokens.
		{RequestID: "b-openai", Provider: "openai", Model: "gpt-5", InputTokens: 500, OutputTokens: 40, CacheReadTokens: 300, StatusCode: 200, CostUSD: &cost},
		// Registry alias translated onto an openai_compat target: costed
		// (and stored) as openai, named by the alias the caller used.
		{RequestID: "c-alias", Provider: "anthropic", Model: "vendor-model", RequestedModel: "my-alias", ResolvedVendor: "openai_compat", InputTokens: 400, OutputTokens: 10, CacheReadTokens: 100, StatusCode: 200, CostUSD: &cost},
		// A labelled target is costed under its label: not folded.
		{RequestID: "d-label", Provider: "openai", Model: "llama", RequestedModel: "llama", ResolvedVendor: "groq", InputTokens: 50, OutputTokens: 5, CacheReadTokens: 20, StatusCode: 200, CostUSD: &cost},
		// Gemini with more cache reads than input never underflows.
		{RequestID: "e-gemini", Provider: "gemini", Model: "gemini-2.5-pro", InputTokens: 10, OutputTokens: 1, CacheReadTokens: 20, StatusCode: 200},
		// Not usage: refused before any target, token counting, failed call.
		{RequestID: "f-refused", Provider: "anthropic", Model: "claude-sonnet-4-5", FallbackIndex: -1, StatusCode: 429},
		{RequestID: "g-count", Provider: "anthropic", Model: "claude-sonnet-4-5", Path: "/v1/messages/count_tokens", InputTokens: 900, StatusCode: 200},
		{RequestID: "h-failed", Provider: "anthropic", Model: "claude-sonnet-4-5", InputTokens: 7, StatusCode: 500},
		// Billable although it starts like a batch path: still usage.
		{RequestID: "i-near-miss", Provider: "anthropic", Model: "claude-sonnet-4-5", Path: "/v1/messages/batches-export", InputTokens: 3, OutputTokens: 1, StatusCode: 200, CostUSD: &cost},
	}
	// Every free endpoint the LLM plane leaves unpriced is also not usage:
	// the view's filter is built from the same list (pricing.UnpricedPath).
	for i, suffix := range pricing.UnpricedPathSuffixes {
		rows = append(rows, &sink.LLMCall{RequestID: fmt.Sprintf("j-free-%d", i), Provider: "anthropic", Model: "claude-sonnet-4-5",
			Path: "/v1/x" + suffix, InputTokens: 900, StatusCode: 200})
	}
	rows = append(rows, &sink.LLMCall{RequestID: "j-free-under", Provider: "anthropic", Model: "claude-sonnet-4-5",
		Path: "/v1" + pricing.UnpricedPathSegment + "msgbatch_1/results", InputTokens: 900, StatusCode: 200})
	for i, r := range rows {
		r.Timestamp, r.TenantID = now.Add(time.Duration(i)*time.Millisecond), tenantID
		r.RequestID = tenantID + "-" + r.RequestID
		if r.Path == "" {
			r.Path = "/v1/messages"
		}
		s.WriteLLMCall(r)
	}
	waitForRowCounts(t, ctx, reader, tenantID, 0, len(rows))

	c := openRawConn(t, cfg)
	res, err := c.Query(ctx, `SELECT request_id, model, provider, prompt_tokens, completion_tokens,
		cache_read_tokens, cache_write_tokens, total_tokens, priced
		FROM llm_usage_canonical WHERE tenant_id = ? ORDER BY request_id`, tenantID)
	if err != nil {
		t.Fatalf("query llm_usage_canonical: %v", err)
	}
	defer res.Close()
	got := map[string]canonicalRow{}
	for res.Next() {
		var r canonicalRow
		if err := res.Scan(&r.RequestID, &r.Model, &r.Provider, &r.PromptTokens, &r.CompletionTokens,
			&r.CacheReadTokens, &r.CacheWriteTokens, &r.TotalTokens, &r.Priced); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[r.RequestID[len(tenantID)+1:]] = r
	}

	want := map[string]canonicalRow{
		"a-anthropic": {Model: "claude-sonnet-4-5", Provider: "anthropic", PromptTokens: 100, CompletionTokens: 50, CacheReadTokens: 1000, CacheWriteTokens: 200, TotalTokens: 150, Priced: true},
		"b-openai":    {Model: "gpt-5", Provider: "openai", PromptTokens: 200, CompletionTokens: 40, CacheReadTokens: 300, TotalTokens: 240, Priced: true},
		"c-alias":     {Model: "my-alias", Provider: "openai", PromptTokens: 300, CompletionTokens: 10, CacheReadTokens: 100, TotalTokens: 310, Priced: true},
		"d-label":     {Model: "llama", Provider: "groq", PromptTokens: 50, CompletionTokens: 5, CacheReadTokens: 20, TotalTokens: 55, Priced: true},
		"e-gemini":    {Model: "gemini-2.5-pro", Provider: "gemini", PromptTokens: 0, CompletionTokens: 1, CacheReadTokens: 20, TotalTokens: 1, Priced: false},
		"i-near-miss": {Model: "claude-sonnet-4-5", Provider: "anthropic", PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4, Priced: true},
	}
	if len(got) != len(want) {
		t.Errorf("view rows = %v, want exactly %v (refused calls, free endpoints and failed calls excluded)", keysOf(got), keysOf(want))
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("row %s missing from view", id)
			continue
		}
		g.RequestID = ""
		if g != w {
			t.Errorf("row %s = %+v, want %+v", id, g, w)
		}
	}
}

func keysOf(m map[string]canonicalRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// monFixture writes one tenant's monitoring test rows: key-a (gateway)
// uses claude-sonnet-4-5 (session s1, priced) and gpt-5 (session s2,
// OpenAI convention, unpriced); key-b is an interceptor; key-a also has
// sonnet usage 8 days ago (the previous 7d period), a refused call that is
// not usage, and another tenant has usage that must never leak in. It
// returns how many rows fall in the last 24h (what waitForRowCounts sees).
func monFixture(t *testing.T, s interface{ WriteLLMCall(*sink.LLMCall) }, tenantID string, now time.Time) int {
	t.Helper()
	cost := 0.01
	rows := []*sink.LLMCall{
		{KeyID: "key-a", SessionID: "s1", Provider: "anthropic", Model: "claude-sonnet-4-5", InputTokens: 100, OutputTokens: 50, CostUSD: &cost},
		{KeyID: "key-a", SessionID: "s1", Provider: "anthropic", Model: "claude-sonnet-4-5", InputTokens: 100, OutputTokens: 50, CostUSD: &cost},
		{KeyID: "key-a", SessionID: "s2", Provider: "openai", Model: "gpt-5", InputTokens: 500, OutputTokens: 40, CacheReadTokens: 300},
		{KeyID: "key-b", Source: "interceptor", Provider: "anthropic", Model: "claude-sonnet-4-5", InputTokens: 10, OutputTokens: 5, CostUSD: &cost},
		{KeyID: "key-a", SessionID: "s1", Provider: "anthropic", Model: "claude-sonnet-4-5", InputTokens: 100, OutputTokens: 100, CostUSD: &cost, Timestamp: now.Add(-8 * 24 * time.Hour)},
		{KeyID: "key-a", Provider: "anthropic", Model: "claude-sonnet-4-5", FallbackIndex: -1, StatusCode: 429},
	}
	for i, r := range rows {
		if r.Timestamp.IsZero() {
			r.Timestamp = now.Add(-time.Duration(i+1) * time.Minute)
		}
		if r.StatusCode == 0 {
			r.StatusCode = 200
		}
		r.TenantID, r.RequestID, r.Path = tenantID, fmt.Sprintf("%s-%d", tenantID, i), "/v1/messages"
		s.WriteLLMCall(r)
	}
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, TenantID: "other-" + tenantID, RequestID: "other-" + tenantID, KeyID: "key-x", Provider: "anthropic", Model: "claude-sonnet-4-5", InputTokens: 999, OutputTokens: 1, StatusCode: 200, Path: "/v1/messages"})
	return len(rows) - 1 // the 8-day-old row is outside waitForRowCounts' 24h
}

func TestIntegration_TokenMonitoring(t *testing.T) {
	cfg := testConfig(t)
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()
	mon, ok := s.(analytics.TokenMonitoringReader)
	if !ok {
		t.Fatal("clickhouse sink does not implement analytics.TokenMonitoringReader")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tenantID := uniqueTenantID(t)
	n := monFixture(t, s, tenantID, time.Now().UTC())
	waitForRowCounts(t, ctx, s.(analytics.Reader), tenantID, 0, n)
	p7d := analytics.Range7d.Period(time.Now())

	g, err := mon.TokenMonitoring(ctx, tenantID, analytics.TokenMonitoringQuery{Range: analytics.Range7d, Period: p7d})
	if err != nil {
		t.Fatalf("TokenMonitoring() error = %v", err)
	}
	tot := g.Totals
	if tot.Tokens != 555 || tot.PromptTokens != 410 || tot.CompletionTokens != 145 || tot.CacheReadTokens != 300 ||
		tot.Calls != 4 || tot.UnpricedCalls != 1 || tot.PrevTokens != 200 {
		t.Errorf("totals = %+v, want tokens 555 prompt 410 completion 145 cache_read 300 calls 4 unpriced 1 prev 200", tot)
	}
	if tot.PrevTokensWithCache != 200 {
		t.Errorf("prev tokens with cache = %d, want 200 (the old sonnet row has no cache)", tot.PrevTokensWithCache)
	}
	if tot.CostUSD == nil || math.Abs(*tot.CostUSD-0.03) > 1e-9 {
		t.Errorf("totals cost = %v, want 0.03", tot.CostUSD)
	}
	if g.Range != analytics.Range7d || g.Granularity != analytics.GranularityDay {
		t.Errorf("range/granularity = %s/%s, want 7d/day", g.Range, g.Granularity)
	}

	// Breakdowns: ordered by tokens, and each adds up to the totals.
	if len(g.ByModel) != 2 || g.ByModel[0].Model != "claude-sonnet-4-5" || g.ByModel[0].Tokens != 315 ||
		g.ByModel[0].Calls != 3 || g.ByModel[0].PrevTokens != 200 {
		t.Fatalf("by_model = %+v, want sonnet first with 315 tokens, 3 calls, prev 200", g.ByModel)
	}
	if gpt := g.ByModel[1]; gpt.Model != "gpt-5" || gpt.Tokens != 240 || gpt.CostUSD != nil || gpt.UnpricedCalls != 1 {
		t.Errorf("gpt row = %+v, want 240 tokens, nil cost, 1 unpriced call", gpt)
	}
	if len(g.ByKey) != 2 || g.ByKey[0].KeyID != "key-a" || g.ByKey[0].Tokens != 540 || g.ByKey[0].Models != 2 ||
		g.ByKey[0].Source != "gateway" || g.ByKey[1].KeyID != "key-b" || g.ByKey[1].Source != "interceptor" || g.ByKey[1].Tokens != 15 {
		t.Errorf("by_key = %+v, want key-a 540 (2 models, gateway) then key-b 15 (interceptor)", g.ByKey)
	}
	var sumModel, sumKey, sumBurn uint64
	for _, m := range g.ByModel {
		sumModel += m.Tokens
	}
	for _, k := range g.ByKey {
		sumKey += k.Tokens
	}
	for _, b := range g.Burn {
		sumBurn += b.Tokens
	}
	if sumModel != tot.Tokens || sumKey != tot.Tokens || sumBurn != tot.Tokens {
		t.Errorf("sums by_model %d by_key %d burn %d, all want totals %d", sumModel, sumKey, sumBurn, tot.Tokens)
	}
	if g.Sessions != nil || g.SessionsTotal != 0 {
		t.Errorf("overview sessions = %+v (total %d), want none: sessions are a drill-down", g.Sessions, g.SessionsTotal)
	}
	if len(g.Burn) != 8 {
		t.Errorf("burn has %d day buckets, want 8 (zero-filled across the 7d window)", len(g.Burn))
	}

	// Last 24h: only the recent rows, hourly buckets, compared with the 24h before.
	day, err := mon.TokenMonitoring(ctx, tenantID, analytics.TokenMonitoringQuery{Range: analytics.Range24h, Period: analytics.Range24h.Period(time.Now())})
	if err != nil {
		t.Fatalf("TokenMonitoring(24h) error = %v", err)
	}
	if day.Granularity != analytics.GranularityHour || day.Totals.Tokens != 555 || day.Totals.PrevTokens != 0 {
		t.Errorf("24h = granularity %s tokens %d prev %d, want hour/555/0", day.Granularity, day.Totals.Tokens, day.Totals.PrevTokens)
	}

	// Custom dates: the single day 8 days ago holds only the old sonnet row.
	old := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.DateOnly)
	custom, err := analytics.ParseDateRange(old, old, time.Now())
	if err != nil {
		t.Fatalf("ParseDateRange() error = %v", err)
	}
	c, err := mon.TokenMonitoring(ctx, tenantID, analytics.TokenMonitoringQuery{Range: analytics.RangeCustom, Period: custom})
	if err != nil {
		t.Fatalf("TokenMonitoring(custom) error = %v", err)
	}
	if c.Range != analytics.RangeCustom || c.Totals.Tokens != 200 || c.Totals.Calls != 1 || len(c.Burn) != 24 {
		t.Errorf("custom = range %s tokens %d calls %d buckets %d, want custom/200/1/24", c.Range, c.Totals.Tokens, c.Totals.Calls, len(c.Burn))
	}

	// Tenant isolation.
	other, err := mon.TokenMonitoring(ctx, "nobody-"+tenantID, analytics.TokenMonitoringQuery{Range: analytics.Range7d, Period: p7d})
	if err != nil {
		t.Fatalf("TokenMonitoring(other tenant) error = %v", err)
	}
	if other.Totals.Tokens != 0 || len(other.ByModel) != 0 || len(other.ByKey) != 0 {
		t.Errorf("other tenant = %+v, want nothing (tenant isolation)", other.Totals)
	}
}

func TestIntegration_TokenMonitoringDrillDown(t *testing.T) {
	cfg := testConfig(t)
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()
	mon := s.(analytics.TokenMonitoringReader)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tenantID := uniqueTenantID(t)
	n := monFixture(t, s, tenantID, time.Now().UTC())
	waitForRowCounts(t, ctx, s.(analytics.Reader), tenantID, 0, n)
	p7d := analytics.Range7d.Period(time.Now())

	// One model: broken down by caller; sessions paged.
	m, err := mon.TokenMonitoring(ctx, tenantID, analytics.TokenMonitoringQuery{Range: analytics.Range7d, Period: p7d, Model: "claude-sonnet-4-5", Limit: 1})
	if err != nil {
		t.Fatalf("TokenMonitoring(model) error = %v", err)
	}
	if m.Totals.Tokens != 315 || m.Totals.PrevTokens != 200 {
		t.Errorf("model totals = %+v, want 315 tokens, prev 200", m.Totals)
	}
	if len(m.ByKey) != 2 || m.ByKey[0].KeyID != "key-a" || m.ByKey[0].Tokens != 300 || m.ByKey[1].Tokens != 15 {
		t.Errorf("model by_key = %+v, want key-a 300, key-b 15", m.ByKey)
	}
	if m.SessionsTotal != 2 || len(m.Sessions) != 1 || m.Sessions[0].SessionID != "s1" || m.Sessions[0].Tokens != 300 || m.Sessions[0].Calls != 2 {
		t.Errorf("model sessions = %+v (total %d), want first page [s1 300 tokens 2 calls] of 2", m.Sessions, m.SessionsTotal)
	}
	page2, err := mon.TokenMonitoring(ctx, tenantID, analytics.TokenMonitoringQuery{Range: analytics.Range7d, Period: p7d, Model: "claude-sonnet-4-5", Limit: 1, Offset: 1})
	if err != nil || len(page2.Sessions) != 1 || page2.Sessions[0].KeyID != "key-b" {
		t.Errorf("model sessions page 2 = %+v, %v; want key-b's session", page2, err)
	}

	// One key: broken down by model; each session lists its models.
	k, err := mon.TokenMonitoring(ctx, tenantID, analytics.TokenMonitoringQuery{Range: analytics.Range7d, Period: p7d, KeyID: "key-a", Limit: 10})
	if err != nil {
		t.Fatalf("TokenMonitoring(key) error = %v", err)
	}
	if k.Totals.Tokens != 540 || len(k.ByModel) != 2 || k.ByModel[0].Model != "claude-sonnet-4-5" || k.ByModel[0].Tokens != 300 || k.ByModel[1].Tokens != 240 {
		t.Errorf("key detail = totals %d by_model %+v, want 540 = sonnet 300 + gpt 240", k.Totals.Tokens, k.ByModel)
	}
	if k.SessionsTotal != 2 || len(k.Sessions) != 2 || k.Sessions[0].SessionID != "s1" || len(k.Sessions[0].Models) != 1 || k.Sessions[0].Models[0] != "claude-sonnet-4-5" {
		t.Errorf("key sessions = %+v, want s1 [claude-sonnet-4-5] then s2", k.Sessions)
	}
	var burn uint64
	for _, b := range k.Burn {
		burn += b.Tokens
	}
	if burn != k.Totals.Tokens {
		t.Errorf("key burn sum = %d, want totals %d", burn, k.Totals.Tokens)
	}

	// A key from another tenant is not visible.
	x, err := mon.TokenMonitoring(ctx, tenantID, analytics.TokenMonitoringQuery{Range: analytics.Range7d, Period: p7d, KeyID: "key-x", Limit: 10})
	if err != nil || x.Totals.Tokens != 0 || len(x.Sessions) != 0 {
		t.Errorf("other tenant's key = %+v, %v; want empty", x, err)
	}
}
