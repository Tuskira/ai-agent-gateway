//go:build integration

// Run against a real ClickHouse:
//
//	docker run -d --rm --name ossgw-ch -p 19000:9000 -p 18123:8123 clickhouse/clickhouse-server:24
//	GATEWAY_TEST_CLICKHOUSE_ADDR=localhost:19000 go test -tags integration ./pkg/sink/clickhouse/... -run Integration -v
//	docker rm -f ossgw-ch
package clickhouse

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// testConfig builds a Config from GATEWAY_TEST_CLICKHOUSE_ADDR
// ("host:port"), skipping the test entirely when it's unset -- the
// package's unit tests (sink_test.go) cover the queue/batch engine
// without a live server; this file is the one place that needs the real
// thing.
func testConfig(t *testing.T) Config {
	t.Helper()
	addr := os.Getenv("GATEWAY_TEST_CLICKHOUSE_ADDR")
	if addr == "" {
		t.Skip("GATEWAY_TEST_CLICKHOUSE_ADDR not set; skipping ClickHouse integration test")
	}
	host, portStr, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("GATEWAY_TEST_CLICKHOUSE_ADDR = %q, want host:port", addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("GATEWAY_TEST_CLICKHOUSE_ADDR port %q: %v", portStr, err)
	}
	return Config{
		Host: host, Port: port, Database: "default",
		BatchSize: 5, FlushInterval: 200 * time.Millisecond, BufferSize: 100,
	}
}

// uniqueTenantID returns a tenant id unique to this test run. The
// integration test's ClickHouse instance is often a long-lived container
// shared across repeated runs (rather than a fresh one per invocation),
// so a fixed tenant id would let one run's rows silently accumulate onto
// the next and throw off every count assertion below -- exactly what
// happened here once already.
func uniqueTenantID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("integration-tenant-%s-%d", t.Name(), time.Now().UnixNano())
}

func TestIntegration_WriteAndQueryOverview(t *testing.T) {
	cfg := testConfig(t)
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()

	reader, ok := s.(analytics.Reader)
	if !ok {
		t.Fatal("clickhouse sink does not implement analytics.Reader")
	}

	tenantID := uniqueTenantID(t)
	now := time.Now().UTC()

	// The access-log mix below is deliberately split across all three
	// outcome buckets (analytics.Outcome{Success,Notification,Error}) so
	// the assertions exercise the bucket rule itself, not just raw HTTP
	// status: an MCP call almost always answers HTTP 200 regardless of
	// whether the JSON-RPC call itself failed, so "success" has to come
	// from error_code, and a notification (status 204) must be excluded
	// from the success/failure verdict entirely rather than counted as
	// either.
	//
	//   15 tools/call, status 200, no error       -> "200"
	//    3 tools/call, status 200, JSON-RPC error -> "error"
	//    2 notifications/initialized, status 204  -> "204"
	//   ---------------------------------------------------
	//   20 access-log rows; 18 of them are tools/call.
	const (
		successRows      = 15
		jsonRPCErrorRows = 3
		notificationRows = 2
	)
	i := 0
	writeAccess := func(method, tool string, status int, errCode string) {
		s.WriteAccess(&sink.AccessLog{
			Timestamp: now.Add(-time.Duration(i) * time.Second), RequestID: fmt.Sprintf("req-%d", i),
			TenantID: tenantID, Principal: "user-1", Method: method, ConnectorID: "conn-everything",
			ToolName: tool, Profile: "profile-a", StatusCode: status, ErrorCode: errCode,
			DurationMS: int64(50 + i), BytesIn: 100, Bytes: 200, ClientIP: "127.0.0.1", UserAgent: "claude-code/1.0",
		})
		i++
	}
	for n := 0; n < successRows; n++ {
		writeAccess("tools/call", "everything__echo", 200, "")
	}
	for n := 0; n < jsonRPCErrorRows; n++ {
		writeAccess("tools/call", "everything__echo", 200, "-32603")
	}
	for n := 0; n < notificationRows; n++ {
		writeAccess("notifications/initialized", "", 204, "")
	}
	const totalAccessRows = successRows + jsonRPCErrorRows + notificationRows // 20
	const mcpToolCallRows = successRows + jsonRPCErrorRows                    // 18

	// 4 successful LLM calls, 1 upstream failure (status 500) -> "error".
	const (
		llmSuccessRows = 4
		llmErrorRows   = 1
	)
	for n := 0; n < llmSuccessRows; n++ {
		s.WriteLLMCall(&sink.LLMCall{
			Timestamp: now.Add(-time.Duration(n) * time.Second), RequestID: fmt.Sprintf("llm-ok-%d", n),
			TenantID: tenantID, Principal: "user-1", Provider: "anthropic", Model: "claude-opus",
			Path: "/v1/messages", StatusCode: 200, DurationMS: int64(100 + n),
			InputTokens: 10, OutputTokens: 20,
		})
	}
	for n := 0; n < llmErrorRows; n++ {
		s.WriteLLMCall(&sink.LLMCall{
			Timestamp: now.Add(-time.Duration(n) * time.Second), RequestID: fmt.Sprintf("llm-err-%d", n),
			TenantID: tenantID, Principal: "user-1", Provider: "anthropic", Model: "claude-opus",
			Path: "/v1/messages", StatusCode: 500, DurationMS: int64(100 + n), Error: "upstream_error",
		})
	}
	const totalLLMRows = llmSuccessRows + llmErrorRows // 5

	// Outcome totals across both tables (20 access + 5 llm = 25 rows):
	//   "200":   15 + 4 = 19
	//   "204":   2 + 0  = 2
	//   "error": 3 + 1  = 4
	const (
		outcome200   = successRows + llmSuccessRows    // 19
		outcome204   = notificationRows                // 2
		outcomeError = jsonRPCErrorRows + llmErrorRows // 4
		totalRows    = totalAccessRows + totalLLMRows  // 25
		totalNon204  = totalRows - outcome204          // 23
	)

	// Flushing is async; wait for it rather than sleeping a fixed amount.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	waitForRowCounts(t, ctx, reader, tenantID, totalAccessRows, totalLLMRows)

	ov, err := reader.Overview(context.Background(), tenantID, analytics.Range24h.Period(time.Now()))
	if err != nil {
		t.Fatalf("Overview() error = %v", err)
	}
	if ov.Kpis.McpToolCalls.Value != mcpToolCallRows {
		t.Errorf("McpToolCalls = %v, want %d", ov.Kpis.McpToolCalls.Value, mcpToolCallRows)
	}
	if ov.Kpis.LlmAgentCalls.Value != totalLLMRows {
		t.Errorf("LlmAgentCalls = %v, want %d", ov.Kpis.LlmAgentCalls.Value, totalLLMRows)
	}
	if ov.Traffic.McpCalls != totalAccessRows || ov.Traffic.LlmCalls != totalLLMRows {
		t.Errorf("Traffic = %+v, want {McpCalls:%d LlmCalls:%d}", ov.Traffic, totalAccessRows, totalLLMRows)
	}
	if len(ov.McpTools) == 0 || ov.McpTools[0].Name != "everything__echo" || ov.McpTools[0].Count != int64(mcpToolCallRows) {
		t.Errorf("McpTools = %+v, want everything__echo=%d", ov.McpTools, mcpToolCallRows)
	}
	if len(ov.TopAgents) == 0 || ov.TopAgents[0].Name != "profile-a" || ov.TopAgents[0].Count != int64(totalAccessRows) {
		t.Errorf("TopAgents = %+v, want profile-a=%d", ov.TopAgents, totalAccessRows)
	}
	if len(ov.TopConnectors) == 0 || ov.TopConnectors[0].Name != "conn-everything" {
		t.Errorf("TopConnectors = %+v", ov.TopConnectors)
	}

	// --- outcome-bucket assertions (the point of this test after the fix) ---

	wantSuccessPct := float64(outcome200) / float64(totalNon204) * 100 // ~82.6%
	if diff := ov.SuccessPct - wantSuccessPct; diff < -0.01 || diff > 0.01 {
		t.Errorf("SuccessPct = %v, want %v (= %d/%d, notifications excluded)", ov.SuccessPct, wantSuccessPct, outcome200, totalNon204)
	}
	if ov.Kpis.SuccessRate.Sub != fmt.Sprintf("%d / %d responses", outcome200, totalNon204) {
		t.Errorf("SuccessRate.Sub = %q, want %q", ov.Kpis.SuccessRate.Sub, fmt.Sprintf("%d / %d responses", outcome200, totalNon204))
	}

	gotBuckets := map[string]float64{}
	for _, b := range ov.StatusCodes {
		gotBuckets[b.Label] = b.Pct
	}
	wantBuckets := map[string]float64{
		analytics.OutcomeSuccess:      float64(outcome200) / float64(totalRows) * 100,
		analytics.OutcomeNotification: float64(outcome204) / float64(totalRows) * 100,
		analytics.OutcomeError:        float64(outcomeError) / float64(totalRows) * 100,
	}
	for label, want := range wantBuckets {
		got, ok := gotBuckets[label]
		if !ok {
			t.Errorf("statusCodes missing bucket %q; got %+v", label, ov.StatusCodes)
			continue
		}
		if diff := got - want; diff < -0.01 || diff > 0.01 {
			t.Errorf("statusCodes[%q].Pct = %v, want %v", label, got, want)
		}
	}
	if len(ov.StatusCodes) != len(wantBuckets) {
		t.Errorf("statusCodes = %+v, want exactly the 3 outcome buckets %v", ov.StatusCodes, wantBuckets)
	}

	items, total, err := reader.ListAccessLogs(context.Background(), tenantID, analytics.AccessLogFilter{ToolName: "everything__echo"})
	if err != nil {
		t.Fatalf("ListAccessLogs() error = %v", err)
	}
	if total != mcpToolCallRows || len(items) == 0 {
		t.Fatalf("ListAccessLogs total = %d, items = %d, want %d, >0", total, len(items), mcpToolCallRows)
	}
	if items[0].RequestBody != nil {
		t.Error("ListAccessLogs must not populate bodies")
	}

	one, err := reader.GetAccessLog(context.Background(), tenantID, items[0].RequestID)
	if err != nil {
		t.Fatalf("GetAccessLog() error = %v", err)
	}
	if one.RequestID != items[0].RequestID {
		t.Errorf("GetAccessLog request id = %q, want %q", one.RequestID, items[0].RequestID)
	}

	if _, err := reader.GetAccessLog(context.Background(), tenantID, "does-not-exist"); err != analytics.ErrNotFound {
		t.Errorf("GetAccessLog(missing) error = %v, want analytics.ErrNotFound", err)
	}

	// Cross-tenant isolation: a different tenant sees none of this.
	_, otherTotal, err := reader.ListAccessLogs(context.Background(), "some-other-tenant-"+tenantID, analytics.AccessLogFilter{})
	if err != nil {
		t.Fatalf("ListAccessLogs(other tenant) error = %v", err)
	}
	if otherTotal != 0 {
		t.Errorf("other tenant total = %d, want 0 (tenant isolation)", otherTotal)
	}
}

// waitForRowCounts polls Overview until Traffic's raw row counts reach
// the expected totals (the sink batches asynchronously) or ctx is done.
// Raw traffic counts (every row, notifications included) are used rather
// than McpToolCalls/LlmAgentCalls so this also waits for the
// notification rows to land before the outcome-bucket assertions run.
func waitForRowCounts(t *testing.T, ctx context.Context, r analytics.Reader, tenantID string, wantMCP, wantLLM int) {
	t.Helper()
	for {
		ov, err := r.Overview(ctx, tenantID, analytics.Range24h.Period(time.Now()))
		if err == nil && ov.Traffic.McpCalls >= int64(wantMCP) && ov.Traffic.LlmCalls >= int64(wantLLM) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("records never reached ClickHouse in time (last err=%v)", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// An unknown cost stays NULL (skipped by the sum, and a window of only NULLs
// sums to 0, not an error), and a float cost is rounded to the column's 12
// places rather than truncated.
func TestIntegration_CostNullableAndRounded(t *testing.T) {
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

	onlyNull := uniqueTenantID(t) + "-null"
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: onlyNull, TenantID: onlyNull, Model: "m", StatusCode: 200})
	waitForRowCounts(t, ctx, reader, onlyNull, 0, 1)
	ov, err := reader.Overview(ctx, onlyNull, analytics.Range24h.Period(time.Now()))
	if err != nil || ov.Kpis.TotalCost.Value != 0 {
		t.Fatalf("all-NULL window: TotalCost = %v, err = %v; want 0, nil", ov.Kpis.TotalCost.Value, err)
	}

	mixed := uniqueTenantID(t)
	inexact := (10*0.06 + 3*0.24) / 1e6 // 1.3199999999999998e-06 in float64
	half := 0.5
	for n, c := range []*float64{nil, &inexact, &half} {
		s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: fmt.Sprintf("%s-%d", mixed, n), TenantID: mixed, Model: "m", StatusCode: 200, CostUSD: c})
	}
	waitForRowCounts(t, ctx, reader, mixed, 0, 3)
	if ov, err = reader.Overview(ctx, mixed, analytics.Range24h.Period(time.Now())); err != nil {
		t.Fatal(err)
	}
	if got := ov.Kpis.TotalCost.Value; math.Abs(got-0.50000132) > 1e-15 {
		t.Errorf("TotalCost = %.15f, want 0.500001320000000 (rounded, NULL skipped)", got)
	}
}

// TestIntegration_ModelsSummary exercises GET /api/v1/analytics/models's
// backing query end to end on rows that carry no registry columns (written
// before they existed): per-model aggregation, the nullable-cost rule (a model with zero priced calls reports CostUSD =
// nil, not 0), used_by as a distinct key_id count, last_seen as the max
// timestamp, and the highest-traffic pick.
func TestIntegration_ModelsSummary(t *testing.T) {
	cfg := testConfig(t)
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()
	reader, ok := s.(analytics.Reader)
	if !ok {
		t.Fatal("clickhouse sink does not implement analytics.Reader")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	now := time.Now().UTC()
	tenantID := uniqueTenantID(t)

	priced := 0.02
	// claude-sonnet: 3 calls, 2 distinct keys, all priced.
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenantID + "-s1", TenantID: tenantID, Provider: "anthropic", Model: "claude-sonnet-4-5", KeyID: "key-a", InputTokens: 100, OutputTokens: 50, StatusCode: 200, CostUSD: &priced})
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now.Add(time.Second), RequestID: tenantID + "-s2", TenantID: tenantID, Provider: "anthropic", Model: "claude-sonnet-4-5", KeyID: "key-a", InputTokens: 100, OutputTokens: 50, StatusCode: 200, CostUSD: &priced})
	lastSeen := now.Add(2 * time.Second)
	s.WriteLLMCall(&sink.LLMCall{Timestamp: lastSeen, RequestID: tenantID + "-s3", TenantID: tenantID, Provider: "anthropic", Model: "claude-sonnet-4-5", KeyID: "key-b", InputTokens: 200, OutputTokens: 100, StatusCode: 200, CostUSD: &priced})
	// unpriced-model: 1 call, no cost -- must report CostUSD = nil, not 0.
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenantID + "-u1", TenantID: tenantID, Provider: "bedrock", Model: "unpriced-model", KeyID: "key-a", InputTokens: 5, OutputTokens: 5, StatusCode: 200})

	const wantLLMRows = 4
	waitForRowCounts(t, ctx, reader, tenantID, 0, wantLLMRows)

	summary, err := reader.ModelsSummary(ctx, tenantID, analytics.Range7d)
	if err != nil {
		t.Fatalf("ModelsSummary() error = %v", err)
	}
	if summary.TotalModels != 2 {
		t.Fatalf("TotalModels = %d, want 2; Models = %+v", summary.TotalModels, summary.Models)
	}

	var sonnet, unpriced *analytics.ModelSummaryRow
	for i := range summary.Models {
		switch summary.Models[i].Name {
		case "claude-sonnet-4-5":
			sonnet = &summary.Models[i]
		case "unpriced-model":
			unpriced = &summary.Models[i]
		}
	}
	if sonnet == nil || unpriced == nil {
		t.Fatalf("Models = %+v, want both claude-sonnet-4-5 and unpriced-model", summary.Models)
	}

	if sonnet.Calls != 3 || sonnet.Tokens != 600 || sonnet.UsedBy != 2 || sonnet.Provider != "anthropic" {
		t.Errorf("sonnet row = %+v, want Calls=3 Tokens=600 UsedBy=2 Provider=anthropic", sonnet)
	}
	if sonnet.CostUSD == nil || math.Abs(*sonnet.CostUSD-0.06) > 1e-9 {
		t.Errorf("sonnet CostUSD = %v, want 0.06", sonnet.CostUSD)
	}
	if sonnet.Status != "active" {
		t.Errorf("sonnet Status = %q, want %q", sonnet.Status, "active")
	}
	if diff := sonnet.LastSeen.Sub(lastSeen); diff < -time.Millisecond || diff > time.Millisecond {
		t.Errorf("sonnet LastSeen = %v, want %v", sonnet.LastSeen, lastSeen)
	}

	if unpriced.Calls != 1 || unpriced.Tokens != 10 || unpriced.UsedBy != 1 || unpriced.Provider != "bedrock" {
		t.Errorf("unpriced row = %+v, want Calls=1 Tokens=10 UsedBy=1 Provider=bedrock", unpriced)
	}
	if unpriced.CostUSD != nil {
		t.Errorf("unpriced CostUSD = %v, want nil (no priced calls)", *unpriced.CostUSD)
	}

	if summary.HighestTraffic == nil || summary.HighestTraffic.Name != "claude-sonnet-4-5" || summary.HighestTraffic.Tokens != 600 {
		t.Errorf("HighestTraffic = %+v, want claude-sonnet-4-5/600", summary.HighestTraffic)
	}

	// Cross-tenant isolation.
	otherSummary, err := reader.ModelsSummary(ctx, "some-other-tenant-"+tenantID, analytics.Range7d)
	if err != nil {
		t.Fatalf("ModelsSummary(other tenant) error = %v", err)
	}
	if otherSummary.TotalModels != 0 {
		t.Errorf("other tenant TotalModels = %d, want 0 (tenant isolation)", otherSummary.TotalModels)
	}
}

// TestIntegration_ModelsSummary_RegistryColumns: rows that went through
// the model registry are grouped by the name the client asked for, one row
// per name whatever target answered; the provider is the vendor that served
// most of its calls; a call refused before any target was tried (no
// resolved vendor) counts in the same row.
func TestIntegration_ModelsSummary_RegistryColumns(t *testing.T) {
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

	priced := 0.01
	call := func(id, requested, vendor, resolved string, status int, in int64, cost *float64) {
		model := resolved
		if model == "" {
			model = requested
		}
		s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenantID + id, TenantID: tenantID, Provider: "anthropic", KeyID: "key-a",
			Model: model, RequestedModel: requested, ResolvedVendor: vendor, ResolvedModel: resolved, Translated: vendor == "openai_compat",
			StatusCode: status, InputTokens: in, CostUSD: cost})
	}
	// "coder": two calls answered by the openai_compat target, one by the
	// anthropic fallback, one refused by a limit before any target.
	call("-c1", "coder", "openai_compat", "qwen2.5:1.5b", 200, 100, &priced)
	call("-c2", "coder", "openai_compat", "qwen2.5:1.5b", 200, 100, &priced)
	call("-c3", "coder", "anthropic", "claude-haiku-4-5", 200, 100, &priced)
	call("-c4", "coder", "", "", 429, 0, nil)
	// An unregistered name: no resolved vendor, the dialect is the provider.
	call("-p1", "claude-sonnet-4-5", "", "", 200, 50, &priced)

	waitForRowCounts(t, ctx, reader, tenantID, 0, 5)
	summary, err := reader.ModelsSummary(ctx, tenantID, analytics.Range7d)
	if err != nil {
		t.Fatalf("ModelsSummary() error = %v", err)
	}
	if summary.TotalModels != 2 || len(summary.Models) != 2 {
		t.Fatalf("Models = %+v, want one row per requested name (coder, claude-sonnet-4-5)", summary.Models)
	}
	coder, plain := summary.Models[0], summary.Models[1]
	if coder.Name != "coder" || coder.Provider != "openai_compat" || coder.Calls != 4 || coder.Tokens != 300 ||
		coder.CostUSD == nil || math.Abs(*coder.CostUSD-0.03) > 1e-9 {
		t.Errorf("coder row = %+v (cost %v), want provider openai_compat, 4 calls, 300 tokens, $0.03", coder, coder.CostUSD)
	}
	if plain.Name != "claude-sonnet-4-5" || plain.Provider != "anthropic" || plain.Calls != 1 {
		t.Errorf("unregistered row = %+v, want claude-sonnet-4-5 / anthropic / 1 call", plain)
	}
	if summary.HighestTraffic == nil || summary.HighestTraffic.Name != "coder" {
		t.Errorf("HighestTraffic = %+v, want coder", summary.HighestTraffic)
	}
}

// An offloaded call round-trips its body_ref through GetLLMCall with empty
// bodies; an inline call keeps its bodies and an empty ref. client_ip
// round-trips through both the list and detail reads.
func TestIntegration_BodyRef(t *testing.T) {
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

	tenant := uniqueTenantID(t)
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenant + "-off", TenantID: tenant, Model: "m", StatusCode: 200, BodyRef: "fs://" + tenant + "-off", ClientIP: "203.0.113.9"})
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenant + "-in", TenantID: tenant, Model: "m", StatusCode: 200, RequestBody: []byte("req")})
	waitForRowCounts(t, ctx, reader, tenant, 0, 2)

	off, err := reader.GetLLMCall(ctx, tenant, tenant+"-off")
	if err != nil || off.BodyRef != "fs://"+tenant+"-off" || off.RequestBody != nil || off.ClientIP != "203.0.113.9" {
		t.Errorf("offloaded row = %+v, err = %v", off, err)
	}
	list, _, err := reader.ListLLMCalls(ctx, tenant, analytics.LLMCallFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListLLMCalls() error = %v", err)
	}
	ips := map[string]string{}
	for _, l := range list {
		ips[l.RequestID] = l.ClientIP
	}
	if ips[tenant+"-off"] != "203.0.113.9" || ips[tenant+"-in"] != "" || len(ips) != 2 {
		t.Errorf("listed client_ip by request = %v", ips)
	}
	in, err := reader.GetLLMCall(ctx, tenant, tenant+"-in")
	if err != nil || in.BodyRef != "" || string(in.RequestBody) != "req" {
		t.Errorf("inline row = %+v, err = %v", in, err)
	}
}

// cost_usd round-trips through both ListLLMCalls and GetLLMCall -- a priced
// call comes back with its exact dollar cost, an unpriced one with nil, not
// 0. Regression test for a bug where both queries omitted the column
// entirely (it's Nullable(Decimal(38,12)), which the driver can't scan
// straight into *float64, so it needs an explicit toFloat64(...) cast):
// GET /analytics/llm-logs and /analytics/llm-logs/{request_id} always
// answered cost_usd: null, even for a call the row itself priced correctly
// (visible only via aggregates like /analytics/models, which select it
// differently). Caught e2e against a real Nebius call with a price set on
// its registry model -- see docs/models.md's price/capabilities section.
func TestIntegration_ListAndGetLLMCall_CostUSD(t *testing.T) {
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

	tenant := uniqueTenantID(t)
	priced := 0.0000676
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenant + "-priced", TenantID: tenant, Model: "zai-org/GLM-5.3", StatusCode: 200, InputTokens: 19, OutputTokens: 50, CostUSD: &priced})
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now.Add(time.Second), RequestID: tenant + "-unpriced", TenantID: tenant, Model: "some-unpriced-model", StatusCode: 200, InputTokens: 10, OutputTokens: 10})
	waitForRowCounts(t, ctx, reader, tenant, 0, 2)

	got, err := reader.GetLLMCall(ctx, tenant, tenant+"-priced")
	if err != nil || got.CostUSD == nil || math.Abs(*got.CostUSD-priced) > 1e-9 {
		t.Errorf("GetLLMCall(priced) cost = %v, err = %v, want %v", got.CostUSD, err, priced)
	}
	gotUnpriced, err := reader.GetLLMCall(ctx, tenant, tenant+"-unpriced")
	if err != nil || gotUnpriced.CostUSD != nil {
		t.Errorf("GetLLMCall(unpriced) cost = %v, err = %v, want nil", gotUnpriced.CostUSD, err)
	}

	list, _, err := reader.ListLLMCalls(ctx, tenant, analytics.LLMCallFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListLLMCalls() error = %v", err)
	}
	costs := map[string]*float64{}
	for _, l := range list {
		costs[l.RequestID] = l.CostUSD
	}
	if costs[tenant+"-priced"] == nil || math.Abs(*costs[tenant+"-priced"]-priced) > 1e-9 {
		t.Errorf("ListLLMCalls priced cost = %v, want %v", costs[tenant+"-priced"], priced)
	}
	if costs[tenant+"-unpriced"] != nil {
		t.Errorf("ListLLMCalls unpriced cost = %v, want nil", *costs[tenant+"-unpriced"])
	}
}

// client_name/user_agent round-trip through both the list and detail reads,
// and GET /analytics/llm-logs?client_name= (ListLLMCalls' ClientName filter)
// narrows to exactly the matching rows.
func TestIntegration_ClientNameAndUserAgent(t *testing.T) {
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

	tenant := uniqueTenantID(t)
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenant + "-cc", TenantID: tenant, Model: "m", StatusCode: 200,
		ClientName: "claude-code", UserAgent: "claude-cli/2.1.0 (external, cli)"})
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenant + "-cursor", TenantID: tenant, Model: "m", StatusCode: 200,
		ClientName: "cursor", UserAgent: "Cursor/1.5"})
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenant + "-none", TenantID: tenant, Model: "m", StatusCode: 200})
	waitForRowCounts(t, ctx, reader, tenant, 0, 3)

	cc, err := reader.GetLLMCall(ctx, tenant, tenant+"-cc")
	if err != nil || cc.ClientName != "claude-code" || cc.UserAgent != "claude-cli/2.1.0 (external, cli)" {
		t.Errorf("claude-code row = %+v, err = %v", cc, err)
	}
	none, err := reader.GetLLMCall(ctx, tenant, tenant+"-none")
	if err != nil || none.ClientName != "" || none.UserAgent != "" {
		t.Errorf("no-UA row = %+v, err = %v", none, err)
	}

	filtered, total, err := reader.ListLLMCalls(ctx, tenant, analytics.LLMCallFilter{Limit: 10, ClientName: "claude-code"})
	if err != nil {
		t.Fatalf("ListLLMCalls(client_name=claude-code) error = %v", err)
	}
	if total != 1 || len(filtered) != 1 || filtered[0].RequestID != tenant+"-cc" {
		t.Errorf("ListLLMCalls(client_name=claude-code) = %+v, total %d, want just %s-cc", filtered, total, tenant)
	}
}

// The model registry columns round-trip through the writer and both
// readers (list and get); a call that never touched the registry reads
// back with empty resolved fields and fallback_index 0, and -1 survives
// the signed column.
func TestIntegration_ModelRegistryColumns(t *testing.T) {
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

	tenant := uniqueTenantID(t)
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenant + "-alias", TenantID: tenant, Provider: "anthropic", Model: "claude-sonnet-4-5", StatusCode: 200,
		RequestedModel: "sonnet", ResolvedVendor: "bedrock", ResolvedModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0", FallbackIndex: 1})
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenant + "-plain", TenantID: tenant, Provider: "anthropic", Model: "claude-haiku-4-5", StatusCode: 200,
		RequestedModel: "claude-haiku-4-5"})
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenant + "-refused", TenantID: tenant, Provider: "anthropic", Model: "gpt", StatusCode: 400,
		RequestedModel: "gpt", FallbackIndex: -1, Error: "requires_translation"})
	waitForRowCounts(t, ctx, reader, tenant, 0, 3)

	alias, err := reader.GetLLMCall(ctx, tenant, tenant+"-alias")
	if err != nil || alias.RequestedModel != "sonnet" || alias.ResolvedVendor != "bedrock" ||
		alias.ResolvedModel != "us.anthropic.claude-sonnet-4-5-20250929-v1:0" || alias.FallbackIndex != 1 || alias.Translated {
		t.Errorf("alias row = %+v, err = %v", alias, err)
	}
	refused, err := reader.GetLLMCall(ctx, tenant, tenant+"-refused")
	if err != nil || refused.FallbackIndex != -1 || refused.ResolvedModel != "" {
		t.Errorf("refused row = %+v, err = %v", refused, err)
	}

	rows, total, err := reader.ListLLMCalls(ctx, tenant, analytics.LLMCallFilter{Limit: 10})
	if err != nil || total != 3 || len(rows) != 3 {
		t.Fatalf("ListLLMCalls = %d rows, total %d, err %v", len(rows), total, err)
	}
	byID := map[string]sink.LLMCall{}
	for _, r := range rows {
		byID[r.RequestID] = r
	}
	if r := byID[tenant+"-alias"]; r.RequestedModel != "sonnet" || r.ResolvedModel == "" || r.FallbackIndex != 1 {
		t.Errorf("listed alias row = %+v", r)
	}
	if r := byID[tenant+"-plain"]; r.RequestedModel != "claude-haiku-4-5" || r.ResolvedModel != "" || r.ResolvedVendor != "" || r.FallbackIndex != 0 {
		t.Errorf("listed plain row = %+v", r)
	}
}

// TestIntegration_SessionTimeline_OwnershipRule is the end-to-end
// regression test for the session-timeline-poisoning fix
// (docs/observability.md#session-timeline-ownership): key-b, running its
// own legitimate MCP session SB, claims session SA's id as its
// X-Session-Id on both planes -- exactly the attack the fix closes. Its
// MCP events land correctly under SB (proven in
// internal/dataplane/transport's TestAccessLogSessionIDIsAlwaysNegotiated,
// so out of scope here) and so never enter SA's timeline; its direct LLM
// calls tagged session_id=SA do reach the query but must be excluded by
// the key_id check, not merely relabeled.
func TestIntegration_SessionTimeline_OwnershipRule(t *testing.T) {
	cfg := testConfig(t)
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()
	reader, ok := s.(analytics.Reader)
	if !ok {
		t.Fatal("clickhouse sink does not implement analytics.Reader")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	now := time.Now().UTC()

	tenantID := uniqueTenantID(t)
	const sessionA = "session-a"
	const sessionB = "session-b"

	// key-a's own session A: 3 MCP tool calls, 2 LLM calls, no client tag.
	for n := 0; n < 3; n++ {
		s.WriteAccess(&sink.AccessLog{
			Timestamp: now.Add(time.Duration(n) * time.Millisecond), RequestID: fmt.Sprintf("%s-a-mcp-%d", tenantID, n),
			TenantID: tenantID, KeyID: "key-a", SessionID: sessionA, Method: "tools/call", ToolName: "echo", StatusCode: 200,
		})
	}
	for n := 0; n < 2; n++ {
		s.WriteLLMCall(&sink.LLMCall{
			Timestamp: now.Add(time.Duration(n) * time.Millisecond), RequestID: fmt.Sprintf("%s-a-llm-%d", tenantID, n),
			TenantID: tenantID, KeyID: "key-a", SessionID: sessionA, Model: "claude-sonnet-4-5", StatusCode: 200,
		})
	}

	// The attack: key-b's own real MCP session B, with 2 tool calls that
	// claim X-Session-Id: session-a -- recorded (per the fix) with
	// SessionID=session-b, ClientSessionID=session-a. These never enter
	// session A's timeline: they're queried by session_id, and their
	// session_id is B, not A.
	for n := 0; n < 2; n++ {
		s.WriteAccess(&sink.AccessLog{
			Timestamp: now.Add(time.Duration(n) * time.Millisecond), RequestID: fmt.Sprintf("%s-b-mcp-%d", tenantID, n),
			TenantID: tenantID, KeyID: "key-b", SessionID: sessionB, ClientSessionID: sessionA, Method: "tools/call", ToolName: "list_dir", StatusCode: 200,
		})
	}
	// key-b also calls the LLM plane directly with X-Session-Id: session-a
	// (the LLM plane has no negotiated session of its own to protect this
	// with) -- these DO match session A's query by session_id, so the
	// key_id check is the only thing standing between them and the
	// timeline.
	for n := 0; n < 2; n++ {
		s.WriteLLMCall(&sink.LLMCall{
			Timestamp: now.Add(time.Duration(n) * time.Millisecond), RequestID: fmt.Sprintf("%s-b-llm-%d", tenantID, n),
			TenantID: tenantID, KeyID: "key-b", SessionID: sessionA, Model: "claude-sonnet-4-5", StatusCode: 200,
		})
	}

	const wantMCPRows = 3 + 2 // session A's 3 + session B's 2
	const wantLLMRows = 2 + 2 // key-a's 2 + key-b's 2 (both tagged session-a)
	waitForRowCounts(t, ctx, reader, tenantID, wantMCPRows, wantLLMRows)

	tl, err := reader.SessionTimeline(ctx, tenantID, sessionA, analytics.TimelineOptions{})
	if err != nil {
		t.Fatalf("SessionTimeline() error = %v", err)
	}
	if tl.OwnerKeyID != "key-a" {
		t.Fatalf("OwnerKeyID = %q, want key-a", tl.OwnerKeyID)
	}
	if tl.ExcludedForeignEvents != 2 {
		t.Errorf("ExcludedForeignEvents = %d, want 2 (key-b's two LLM calls tagged session-a)", tl.ExcludedForeignEvents)
	}
	if tl.TotalEvents != 5 || len(tl.Events) != 5 {
		t.Fatalf("TotalEvents = %d, len(Events) = %d, want 5 (3 MCP + 2 LLM, all key-a)", tl.TotalEvents, len(tl.Events))
	}
	mcpCount, llmCount := 0, 0
	for _, e := range tl.Events {
		if e.KeyID != "key-a" {
			t.Errorf("event %+v belongs to a foreign key, must not appear in Events", e)
		}
		switch e.Plane {
		case "mcp":
			mcpCount++
		case "llm":
			llmCount++
		default:
			t.Errorf("event %+v has unexpected plane", e)
		}
	}
	if mcpCount != 3 || llmCount != 2 {
		t.Errorf("mcpCount=%d llmCount=%d, want 3/2", mcpCount, llmCount)
	}
	for i := 1; i < len(tl.Events); i++ {
		if tl.Events[i].Timestamp.Before(tl.Events[i-1].Timestamp) {
			t.Errorf("Events not time-ordered: %+v then %+v", tl.Events[i-1], tl.Events[i])
		}
	}

	// Descending is exactly the ascending list reversed -- equal-timestamp
	// MCP/LLM pairs included (the ids break the tie) -- with identical stats.
	tlDesc, err := reader.SessionTimeline(ctx, tenantID, sessionA, analytics.TimelineOptions{Order: analytics.TimelineOrderDesc})
	if err != nil {
		t.Fatalf("SessionTimeline(desc) error = %v", err)
	}
	if tlDesc.Order != analytics.TimelineOrderDesc || tl.Order != analytics.TimelineOrderAsc {
		t.Errorf("orders = %q / %q, want desc / asc", tlDesc.Order, tl.Order)
	}
	if tlDesc.OwnerKeyID != tl.OwnerKeyID || tlDesc.TotalEvents != tl.TotalEvents || tlDesc.ExcludedForeignEvents != tl.ExcludedForeignEvents {
		t.Errorf("stats differ across orders: asc %+v desc %+v", tl, tlDesc)
	}
	for i := range tl.Events {
		if tlDesc.Events[len(tl.Events)-1-i].ID != tl.Events[i].ID {
			t.Errorf("desc is not the reverse of asc at %d: %v vs %v", i, tlDesc.Events, tl.Events)
			break
		}
	}

	// Session B's own timeline: key-b's 2 MCP events, PLUS key-b's own 2
	// LLM calls tagged session_id=session-a -- because key-b's own MCP
	// events carry ClientSessionID=session-a, that tag belongs to B's tag
	// set too, and the LLM events under it are key-b's own. This is not a
	// leak: key-a's 2 LLM events also carry session_id=session-a (matching
	// B's tag set by coincidence of the shared tag string) but are
	// correctly excluded, since they belong to a different key.
	tlB, err := reader.SessionTimeline(ctx, tenantID, sessionB, analytics.TimelineOptions{})
	if err != nil {
		t.Fatalf("SessionTimeline(sessionB) error = %v", err)
	}
	if tlB.OwnerKeyID != "key-b" || tlB.TotalEvents != 4 || tlB.ExcludedForeignEvents != 2 {
		t.Errorf("session B timeline = %+v, want owner key-b, 4 events, 2 excluded (key-a's LLM events under the shared tag)", tlB)
	}
	for _, e := range tlB.Events {
		if e.KeyID != "key-b" {
			t.Errorf("session B event %+v belongs to a foreign key", e)
		}
	}

	// An unknown session id is a valid, empty timeline -- not an error.
	empty, err := reader.SessionTimeline(ctx, tenantID, "never-seen-session", analytics.TimelineOptions{})
	if err != nil {
		t.Fatalf("SessionTimeline(unknown) error = %v", err)
	}
	if empty.OwnerKeyID != "" || empty.TotalEvents != 0 || len(empty.Events) != 0 {
		t.Errorf("unknown session timeline = %+v, want empty", empty)
	}
}

// TestIntegration_SessionTimeline_LLMOnlySession covers a session that
// never made an MCP call at all: ownership falls back to the earliest LLM
// event's key, and a later call under the same tag from another key is
// excluded the same way an MCP-anchored session excludes one.
func TestIntegration_SessionTimeline_LLMOnlySession(t *testing.T) {
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
	const sessionID = "llm-only-session"

	// key-a is first to use this tag -- it owns it.
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenantID + "-1", TenantID: tenantID, KeyID: "key-a", SessionID: sessionID, Model: "m", StatusCode: 200})
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now.Add(time.Second), RequestID: tenantID + "-2", TenantID: tenantID, KeyID: "key-a", SessionID: sessionID, Model: "m", StatusCode: 200})
	// key-b uses the same tag later -- excluded.
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now.Add(2 * time.Second), RequestID: tenantID + "-3", TenantID: tenantID, KeyID: "key-b", SessionID: sessionID, Model: "m", StatusCode: 200})

	waitForRowCounts(t, ctx, reader, tenantID, 0, 3)

	tl, err := reader.SessionTimeline(ctx, tenantID, sessionID, analytics.TimelineOptions{})
	if err != nil {
		t.Fatalf("SessionTimeline() error = %v", err)
	}
	if tl.OwnerKeyID != "key-a" {
		t.Errorf("OwnerKeyID = %q, want key-a (earliest LLM event)", tl.OwnerKeyID)
	}
	if tl.TotalEvents != 2 || len(tl.Events) != 2 {
		t.Fatalf("TotalEvents = %d, want 2", tl.TotalEvents)
	}
	if tl.ExcludedForeignEvents != 1 {
		t.Errorf("ExcludedForeignEvents = %d, want 1", tl.ExcludedForeignEvents)
	}
	for _, e := range tl.Events {
		if e.Plane != "llm" || e.KeyID != "key-a" {
			t.Errorf("event %+v, want plane=llm key_id=key-a", e)
		}
	}
}

// TestIntegration_SessionTimeline_ByClientTag covers searching by a
// client's own session tag (the X-Session-Id it sends on both planes)
// rather than an MCP session id: the MCP calls that carried the tag and
// the LLM calls under it both appear, owned by the earliest event's key,
// and another key claiming the same tag is excluded on both planes.
func TestIntegration_SessionTimeline_ByClientTag(t *testing.T) {
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
	const tag = "agent-conversation-1"

	// key-a: an LLM call, then two MCP calls on its own MCP session, both
	// tagged with the conversation id, then another LLM call.
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, RequestID: tenantID + "-a-llm-0", TenantID: tenantID,
		KeyID: "key-a", SessionID: tag, Model: "m", StatusCode: 200})
	for n := 1; n <= 2; n++ {
		s.WriteAccess(&sink.AccessLog{Timestamp: now.Add(time.Duration(n) * time.Millisecond), RequestID: fmt.Sprintf("%s-a-mcp-%d", tenantID, n),
			TenantID: tenantID, KeyID: "key-a", SessionID: "mcp-session-a", ClientSessionID: tag, Method: "tools/call", ToolName: "echo", StatusCode: 200})
	}
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now.Add(3 * time.Millisecond), RequestID: tenantID + "-a-llm-3", TenantID: tenantID,
		KeyID: "key-a", SessionID: tag, Model: "m", StatusCode: 200})
	// key-b claims the same tag on both planes, later.
	s.WriteAccess(&sink.AccessLog{Timestamp: now.Add(4 * time.Millisecond), RequestID: tenantID + "-b-mcp", TenantID: tenantID,
		KeyID: "key-b", SessionID: "mcp-session-b", ClientSessionID: tag, Method: "tools/call", ToolName: "list_dir", StatusCode: 200})
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now.Add(5 * time.Millisecond), RequestID: tenantID + "-b-llm", TenantID: tenantID,
		KeyID: "key-b", SessionID: tag, Model: "m", StatusCode: 200})
	waitForRowCounts(t, ctx, reader, tenantID, 3, 3)

	tl, err := reader.SessionTimeline(ctx, tenantID, tag, analytics.TimelineOptions{})
	if err != nil {
		t.Fatalf("SessionTimeline() error = %v", err)
	}
	if tl.OwnerKeyID != "key-a" || tl.TotalEvents != 4 || tl.ExcludedForeignEvents != 2 {
		t.Fatalf("timeline = owner %q, %d events, %d excluded; want key-a, 4 (2 MCP + 2 LLM), 2", tl.OwnerKeyID, tl.TotalEvents, tl.ExcludedForeignEvents)
	}
	planes := map[string]int{}
	for i, e := range tl.Events {
		if e.KeyID != "key-a" {
			t.Errorf("event %+v belongs to a foreign key", e)
		}
		if i > 0 && e.Timestamp.Before(tl.Events[i-1].Timestamp) {
			t.Errorf("events not time-ordered at %d", i)
		}
		planes[e.Plane]++
	}
	if planes["mcp"] != 2 || planes["llm"] != 2 {
		t.Errorf("planes = %v, want 2 mcp + 2 llm", planes)
	}

	// Searching by the MCP session id still works as before.
	if tlm, err := reader.SessionTimeline(ctx, tenantID, "mcp-session-a", analytics.TimelineOptions{}); err != nil || tlm.TotalEvents != 4 || tlm.OwnerKeyID != "key-a" {
		t.Errorf("by MCP session id = %+v, %v; want 4 events owned by key-a", tlm, err)
	}
}

// TestIntegration_SessionTimeline_LinksMCPByToolCall covers agents that tag
// only their model calls (Claude Code): an MCP call carrying the reply's
// tool-use id links to exactly that conversation, even with a parallel one on
// the same key calling the same tool; a call without an id links by tool name
// in the window. Another key, another tool, or much later never links.
func TestIntegration_SessionTimeline_LinksMCPByToolCall(t *testing.T) {
	cfg := testConfig(t)
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()
	reader := s.(analytics.Reader)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	now := time.Now().UTC().Add(-2 * time.Hour)
	tenantID := uniqueTenantID(t)
	reply := func(useID string) []byte {
		return []byte(`{"type":"message","content":[{"type":"tool_use","id":"` + useID + `","name":"mcp__gw__everything__echo","input":{"message":"hi"}}]}`)
	}
	llm := func(id, conv string, at time.Time, body []byte) {
		s.WriteLLMCall(&sink.LLMCall{Timestamp: at, RequestID: tenantID + "-" + id, TenantID: tenantID, KeyID: "key-a",
			SessionID: conv, Model: "m", StatusCode: 200, ResponseBody: body})
	}
	llm("llm-0", "conv-1", now, reply("toolu_A"))
	llm("llm2-0", "conv-2", now.Add(500*time.Millisecond), reply("toolu_B")) // parallel conversation, same key and tool
	llm("llm-1", "conv-1", now.Add(3*time.Second), []byte(`{"type":"message","content":[{"type":"text","text":"done"}]}`))
	access := func(id, key, session, method, tool, useID string, at time.Time) {
		var body []byte
		if useID != "" {
			body = []byte(`{"method":"tools/call","params":{"name":"` + tool + `","_meta":{"claudecode/toolUseId":"` + useID + `"}}}`)
		}
		s.WriteAccess(&sink.AccessLog{Timestamp: at, RequestID: tenantID + "-" + id, TenantID: tenantID, KeyID: key,
			SessionID: session, Method: method, ToolName: tool, StatusCode: 200, RequestBody: body})
	}
	access("a-init", "key-a", "mcp-a", "initialize", "", "", now.Add(-time.Second))
	access("a-call", "key-a", "mcp-a", "tools/call", "everything__echo", "toolu_A", now.Add(time.Second))
	access("e-call", "key-a", "mcp-e", "tools/call", "everything__echo", "toolu_B", now.Add(time.Second)) // conv-2's call
	access("f-call", "key-a", "mcp-f", "tools/call", "everything__echo", "", now.Add(2*time.Second))      // no id: by name
	access("b-call", "key-b", "mcp-b", "tools/call", "everything__echo", "", now.Add(time.Second))        // another key
	access("c-call", "key-a", "mcp-c", "tools/call", "everything__get-sum", "", now.Add(time.Second))     // not asked for
	access("d-call", "key-a", "mcp-d", "tools/call", "everything__echo", "", now.Add(time.Hour))          // much later
	waitForRowCounts(t, ctx, reader, tenantID, 7, 3)

	events := func(id string) string {
		tl, err := reader.SessionTimeline(ctx, tenantID, id, analytics.TimelineOptions{})
		if err != nil {
			t.Fatalf("SessionTimeline(%s) error = %v", id, err)
		}
		var ids []string
		for _, e := range tl.Events {
			ids = append(ids, strings.TrimPrefix(e.ID, tenantID+"-"))
		}
		return strings.Join(ids, ",")
	}
	for id, want := range map[string]string{
		"conv-1": "a-init,llm-0,a-call,f-call,llm-1", // its exact call, plus the id-less call by name; never conv-2's
		"conv-2": "llm2-0,e-call,f-call",
		"mcp-a":  "a-init,llm-0,a-call,llm-1", // exact: only the conversation whose reply issued toolu_A
		"mcp-e":  "llm2-0,e-call",
		"mcp-b":  "b-call", // another key's session is never joined to key-a's conversations
	} {
		if got := events(id); got != want {
			t.Errorf("timeline(%s) = %s, want %s", id, got, want)
		}
	}
}

// TestIntegration_MigrationAddsClientSessionIDToExistingTable simulates a
// deployment upgrading from before this column existed: mcp_access_logs is
// recreated with the pre-fix schema, then New (which runs applyMigrations)
// must add client_session_id via ADD COLUMN IF NOT EXISTS without losing
// or failing on the rest of the table, and the column must round-trip
// afterwards through the writer and both readers (list + detail).
func TestIntegration_MigrationAddsClientSessionIDToExistingTable(t *testing.T) {
	cfg := testConfig(t)

	raw, err := clickhouse.Open(cfg.options())
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}
	defer raw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// The pre-fix schema: every column migrationMCPAccessLogs declares
	// today, EXCEPT client_session_id.
	const preFixSchema = `
CREATE TABLE mcp_access_logs (
	timestamp DateTime64(3),
	request_id String,
	correlation_id String,
	trace_id String,
	span_id String,
	tenant_id String,
	principal String,
	key_id String,
	session_id String,
	method String,
	json_rpc_id String,
	connector_id String,
	tool_name String,
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
	truncated UInt8
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (tenant_id, timestamp, request_id)
TTL toDateTime(timestamp) + INTERVAL 90 DAY
`
	if err := raw.Exec(ctx, "DROP TABLE IF EXISTS mcp_access_logs"); err != nil {
		t.Fatalf("drop mcp_access_logs: %v", err)
	}
	if err := raw.Exec(ctx, preFixSchema); err != nil {
		t.Fatalf("create pre-fix mcp_access_logs: %v", err)
	}

	// New() applies the baseline migration, including
	// alterMCPAccessLogsClientSessionID -- this is the upgrade path under
	// test.
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v (migration should have added client_session_id, not failed)", err)
	}
	defer s.Close()
	reader, ok := s.(analytics.Reader)
	if !ok {
		t.Fatal("clickhouse sink does not implement analytics.Reader")
	}

	tenantID := uniqueTenantID(t)
	now := time.Now().UTC()
	s.WriteAccess(&sink.AccessLog{
		Timestamp: now, RequestID: tenantID + "-req", TenantID: tenantID, KeyID: "key-a",
		SessionID: "sess-x", ClientSessionID: "claimed-tag", Method: "tools/call", ToolName: "echo", StatusCode: 200,
	})
	waitForRowCounts(t, ctx, reader, tenantID, 1, 0)

	items, total, err := reader.ListAccessLogs(ctx, tenantID, analytics.AccessLogFilter{})
	if err != nil {
		t.Fatalf("ListAccessLogs() error = %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ClientSessionID != "claimed-tag" {
		t.Fatalf("ListAccessLogs = %+v, want one row with ClientSessionID=claimed-tag", items)
	}

	one, err := reader.GetAccessLog(ctx, tenantID, tenantID+"-req")
	if err != nil {
		t.Fatalf("GetAccessLog() error = %v", err)
	}
	if one.ClientSessionID != "claimed-tag" {
		t.Errorf("GetAccessLog ClientSessionID = %q, want claimed-tag", one.ClientSessionID)
	}
}

// TestIntegration_SkillsSummary exercises GET /api/v1/analytics/skills's
// backing query end to end: per skill/command aggregation from
// mcp_access_logs.skill_name (a gateway__skill tools/call or a native
// command's prompts/get), used_by as a distinct key_id count, last_seen
// as the max timestamp, the most-used pick, and that rows with no
// skill_name (a normal connector-routed call) are excluded entirely.
func TestIntegration_SkillsSummary(t *testing.T) {
	cfg := testConfig(t)
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()
	reader, ok := s.(analytics.Reader)
	if !ok {
		t.Fatal("clickhouse sink does not implement analytics.Reader")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	now := time.Now().UTC()
	tenantID := uniqueTenantID(t)

	// "review-pr" (a skill): 3 gateway__skill calls, 2 distinct keys.
	s.WriteAccess(&sink.AccessLog{Timestamp: now, TenantID: tenantID, RequestID: tenantID + "-r1", Method: "tools/call", ToolName: "gateway__skill", SkillName: "review-pr", KeyID: "key-a", StatusCode: 200})
	s.WriteAccess(&sink.AccessLog{Timestamp: now.Add(time.Second), TenantID: tenantID, RequestID: tenantID + "-r2", Method: "tools/call", ToolName: "gateway__skill", SkillName: "review-pr", KeyID: "key-a", StatusCode: 200})
	lastSeen := now.Add(2 * time.Second)
	s.WriteAccess(&sink.AccessLog{Timestamp: lastSeen, TenantID: tenantID, RequestID: tenantID + "-r3", Method: "tools/call", ToolName: "gateway__skill", SkillName: "review-pr", KeyID: "key-b", StatusCode: 200})
	// "summarize" (a command): 1 prompts/get.
	s.WriteAccess(&sink.AccessLog{Timestamp: now, TenantID: tenantID, RequestID: tenantID + "-c1", Method: "prompts/get", SkillName: "summarize", KeyID: "key-a", StatusCode: 200})
	// A normal connector-routed call, no skill_name -- must not appear.
	s.WriteAccess(&sink.AccessLog{Timestamp: now, TenantID: tenantID, RequestID: tenantID + "-n1", Method: "tools/call", ConnectorID: "conn-1", ToolName: "search", KeyID: "key-a", StatusCode: 200})

	const wantAccessRows = 5
	waitForRowCounts(t, ctx, reader, tenantID, wantAccessRows, 0)

	summary, err := reader.SkillsSummary(ctx, tenantID, analytics.Range7d.Period(time.Now()))
	if err != nil {
		t.Fatalf("SkillsSummary() error = %v", err)
	}
	if summary.TotalSkills != 2 {
		t.Fatalf("TotalSkills = %d, want 2; Skills = %+v", summary.TotalSkills, summary.Skills)
	}

	var reviewPR, summarize *analytics.SkillSummaryRow
	for i := range summary.Skills {
		switch summary.Skills[i].Name {
		case "review-pr":
			reviewPR = &summary.Skills[i]
		case "summarize":
			summarize = &summary.Skills[i]
		}
	}
	if reviewPR == nil || summarize == nil {
		t.Fatalf("Skills = %+v, want both review-pr and summarize", summary.Skills)
	}

	if reviewPR.Calls != 3 || reviewPR.UsedBy != 2 {
		t.Errorf("review-pr row = %+v, want Calls=3 UsedBy=2", reviewPR)
	}
	if diff := reviewPR.LastSeen.Sub(lastSeen); diff < -time.Millisecond || diff > time.Millisecond {
		t.Errorf("review-pr LastSeen = %v, want %v", reviewPR.LastSeen, lastSeen)
	}
	// Kind is always "" from this Reader -- the registry join happens in
	// internal/api/handlers.Analytics.Skills, not here.
	if reviewPR.Kind != "" {
		t.Errorf("review-pr Kind = %q, want \"\" (ClickHouse Reader never sets it)", reviewPR.Kind)
	}

	if summarize.Calls != 1 || summarize.UsedBy != 1 {
		t.Errorf("summarize row = %+v, want Calls=1 UsedBy=1", summarize)
	}

	if summary.MostUsed == nil || summary.MostUsed.Name != "review-pr" || summary.MostUsed.Calls != 3 {
		t.Errorf("MostUsed = %+v, want review-pr/3", summary.MostUsed)
	}

	// Cross-tenant isolation.
	otherSummary, err := reader.SkillsSummary(ctx, "some-other-tenant-"+tenantID, analytics.Range7d.Period(time.Now()))
	if err != nil {
		t.Fatalf("SkillsSummary(other tenant) error = %v", err)
	}
	if otherSummary.TotalSkills != 0 {
		t.Errorf("other tenant TotalSkills = %d, want 0 (tenant isolation)", otherSummary.TotalSkills)
	}
}

// TestIntegration_DiscoveryUsage round-trips llm_calls.skills_used /
// mcp_tools_used through the writer and the two discovery readers: per-name
// call counts, distinct keys, last_seen, empty arrays for calls that used
// nothing, and tenant isolation.
func TestIntegration_DiscoveryUsage(t *testing.T) {
	cfg := testConfig(t)
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()
	reader := s.(analytics.Reader)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	now := time.Now().UTC()
	tenantID := uniqueTenantID(t)

	mk := func(id, key string, at time.Time, skills, mcps []string) *sink.LLMCall {
		return &sink.LLMCall{Timestamp: at, TenantID: tenantID, RequestID: tenantID + id, KeyID: key, Model: "m", StatusCode: 200, SkillsUsed: skills, MCPToolsUsed: mcps}
	}
	lastSeen := now.Add(2 * time.Second)
	s.WriteLLMCall(mk("-1", "key-a", now, []string{"review-pr"}, []string{"gw__langfuse__get_trace"}))
	s.WriteLLMCall(mk("-2", "key-b", lastSeen, []string{"review-pr", "deploy"}, []string{"gw__langfuse__get_trace", "gw__langfuse__list_traces", "figma__get_file"}))
	s.WriteLLMCall(mk("-3", "key-a", now, nil, nil)) // used nothing: no array rows
	waitForRowCounts(t, ctx, reader, tenantID, 0, 3)

	skills, err := reader.SkillUsage(ctx, tenantID, analytics.Range7d.Period(time.Now()))
	if err != nil {
		t.Fatalf("SkillUsage: %v", err)
	}
	if len(skills) != 2 || skills[0].Name != "review-pr" || skills[0].Calls != 2 || skills[0].UsedBy != 2 {
		t.Fatalf("skills = %+v", skills)
	}
	if d := skills[0].LastSeen.Sub(lastSeen); d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("last_seen = %v want %v", skills[0].LastSeen, lastSeen)
	}
	if skills[1].Name != "deploy" || skills[1].Calls != 1 {
		t.Errorf("deploy = %+v", skills[1])
	}

	tools, err := reader.MCPToolUsage(ctx, tenantID, analytics.Range7d.Period(time.Now()))
	if err != nil {
		t.Fatalf("MCPToolUsage: %v", err)
	}
	if len(tools) != 3 || tools[0].Server != "gw" || tools[0].Tool != "langfuse__get_trace" || tools[0].Calls != 2 || len(tools[0].Keys) != 2 {
		t.Fatalf("tools = %+v", tools)
	}
	if tools[1].Server != "figma" || tools[1].Tool != "get_file" || tools[1].Calls != 1 {
		t.Errorf("figma = %+v", tools[1])
	}

	// Server calls are distinct LLM calls: call -2 used two gw tools of the
	// same connector and call -1 one, so "langfuse" (via gw) is 2, not 3.
	calls, err := reader.MCPServerCalls(ctx, tenantID, analytics.Range7d.Period(time.Now()), map[string]string{"langfuse": "langfuse"})
	if err != nil {
		t.Fatalf("MCPServerCalls: %v", err)
	}
	got := map[string]int64{}
	for _, c := range calls {
		got[fmt.Sprintf("%s/%v", c.Server, c.Via)] = c.Calls
	}
	if got["langfuse/true"] != 2 || got["figma/false"] != 1 || len(got) != 2 {
		t.Errorf("server calls = %+v", calls)
	}

	other, err := reader.SkillUsage(ctx, "other-"+tenantID, analytics.Range7d.Period(time.Now()))
	if err != nil || len(other) != 0 {
		t.Errorf("other tenant skills = %+v, err %v (want none)", other, err)
	}

	// Custom dates bound the window: a day a month ago holds none of these rows.
	day := time.Now().UTC().AddDate(0, 0, -30).Format(time.DateOnly)
	past, err := analytics.ParseDateRange(day, day, time.Now())
	if err != nil {
		t.Fatalf("ParseDateRange() error = %v", err)
	}
	if old, err := reader.MCPToolUsage(ctx, tenantID, past); err != nil || len(old) != 0 {
		t.Errorf("tools a month ago = %+v, err %v (want none)", old, err)
	}
}

/* ---------------------------------------------------------------------- */
/* Interceptor ingest: migration idempotency, WriteIngestBatch, source/`user` */
/* reader filters.                                                       */
/* ---------------------------------------------------------------------- */

// oldMigrationMCPAccessLogs and oldMigrationLLMCalls are byte-for-byte the
// CREATE TABLE statements from before the interceptor-ingest feature (git
// HEAD~N's migrate.go, i.e. "main"): no source/`user` columns.
// TestIntegration_MigrationIdempotency uses these to build a database as
// it would have looked right before this change, so the test proves the
// NEW applyMigrations upgrades an old table in place, twice, without error.
const oldMigrationMCPAccessLogs = `
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
	truncated UInt8
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (tenant_id, timestamp, request_id)
TTL toDateTime(timestamp) + INTERVAL 90 DAY
`

const oldMigrationLLMCalls = `
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
	fallback_index Int32
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (tenant_id, timestamp, request_id)
TTL toDateTime(timestamp) + INTERVAL 90 DAY
`

// openRawConn opens a bare clickhouse-go connection (bypassing New, which
// would immediately run the CURRENT applyMigrations) so a test can control
// migration ordering itself.
func openRawConn(t *testing.T, cfg Config) conn {
	t.Helper()
	c, err := clickhouse.Open(cfg.withDefaults().options())
	if err != nil {
		t.Fatalf("clickhouse.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx); err != nil {
		t.Fatalf("ping ClickHouse: %v", err)
	}
	return c
}

// TestIntegration_MigrationIdempotency builds mcp_access_logs/llm_calls
// with the pre-interceptor-ingest ("main") schema, inserts one row into
// each with the OLD column set, then runs the CURRENT applyMigrations
// TWICE against that database. It asserts: no error either time (an ALTER
// TABLE ... ADD COLUMN IF NOT EXISTS is idempotent), the old row reads
// back with source='gateway' and `user`=” (the columns' DEFAULTs, applied
// retroactively since the row predates the columns), and a freshly
// inserted row's explicit source/`user` values round-trip correctly. It
// also directly proves ClickHouse 24.8 accepts the backtick-quoted `user`
// column (see migrate.go's doc comment: unquoted, ClickHouse parses it as
// the user() function).
func TestIntegration_MigrationIdempotency(t *testing.T) {
	cfg := testConfig(t)
	// A private, unique database per run so this test's raw DDL can never
	// collide with another test (or another run) sharing the container.
	cfg.Database = "ingest_migrate_" + strings.ReplaceAll(uniqueTenantID(t), "-", "_")

	bootstrap, err := clickhouse.Open(Config{Host: cfg.Host, Port: cfg.Port, Database: "default"}.withDefaults().options())
	if err != nil {
		t.Fatalf("open bootstrap conn: %v", err)
	}
	if err := bootstrap.Exec(context.Background(), "CREATE DATABASE IF NOT EXISTS "+cfg.Database); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_ = bootstrap.Exec(context.Background(), "DROP DATABASE IF EXISTS "+cfg.Database)
		_ = bootstrap.Close()
	})

	c := openRawConn(t, cfg)
	ctx := context.Background()

	// 1. Build the OLD schema.
	if err := c.Exec(ctx, oldMigrationMCPAccessLogs); err != nil {
		t.Fatalf("create old mcp_access_logs: %v", err)
	}
	if err := c.Exec(ctx, oldMigrationLLMCalls); err != nil {
		t.Fatalf("create old llm_calls: %v", err)
	}

	// 2. Insert one row into each with the OLD column set (no source/`user`).
	tenantID := uniqueTenantID(t)
	now := time.Now().UTC()
	if err := c.Exec(ctx, `INSERT INTO mcp_access_logs (timestamp, request_id, tenant_id, method, status_code) VALUES (?, ?, ?, ?, ?)`,
		now, "old-access-1", tenantID, "tools/call", uint16(200)); err != nil {
		t.Fatalf("insert old access row: %v", err)
	}
	if err := c.Exec(ctx, `INSERT INTO llm_calls (timestamp, request_id, tenant_id, model, status_code) VALUES (?, ?, ?, ?, ?)`,
		now, "old-llm-1", tenantID, "claude-opus", uint16(200)); err != nil {
		t.Fatalf("insert old llm row: %v", err)
	}

	// 3. Run the CURRENT applyMigrations TWICE.
	if err := applyMigrations(ctx, c); err != nil {
		t.Fatalf("applyMigrations() (1st run) error = %v", err)
	}
	if err := applyMigrations(ctx, c); err != nil {
		t.Fatalf("applyMigrations() (2nd run) error = %v", err)
	}

	// 4. The old row reads back with the new columns' defaults.
	var source, user string
	if err := c.QueryRow(ctx, "SELECT source, `user` FROM mcp_access_logs WHERE request_id = ?", "old-access-1").Scan(&source, &user); err != nil {
		t.Fatalf("select old access row's source/user: %v", err)
	}
	if source != "gateway" || user != "" {
		t.Errorf("old access row source/user = %q/%q, want gateway/\"\" (column DEFAULTs)", source, user)
	}
	if err := c.QueryRow(ctx, "SELECT source, `user` FROM llm_calls WHERE request_id = ?", "old-llm-1").Scan(&source, &user); err != nil {
		t.Fatalf("select old llm row's source/user: %v", err)
	}
	if source != "gateway" || user != "" {
		t.Errorf("old llm row source/user = %q/%q, want gateway/\"\" (column DEFAULTs)", source, user)
	}

	// 5. A freshly inserted row with explicit source/`user` round-trips --
	// this is the direct proof that ClickHouse 24.8 accepts the
	// backtick-quoted `user` column as a column, not the user() function.
	if err := c.Exec(ctx, "INSERT INTO llm_calls (timestamp, request_id, tenant_id, model, status_code, source, `user`) VALUES (?, ?, ?, ?, ?, ?, ?)",
		now, "new-llm-1", tenantID, "claude-opus", uint16(200), "interceptor", "person@example.com"); err != nil {
		t.Fatalf("insert new llm row with source/user: %v", err)
	}
	if err := c.QueryRow(ctx, "SELECT source, `user` FROM llm_calls WHERE request_id = ?", "new-llm-1").Scan(&source, &user); err != nil {
		t.Fatalf("select new llm row's source/user: %v", err)
	}
	if source != "interceptor" || user != "person@example.com" {
		t.Errorf("new llm row source/user = %q/%q, want interceptor/person@example.com", source, user)
	}
}

// TestIntegration_WriteIngestBatch_AcceptAndDuplicates exercises
// sink.IngestSink end to end: a fresh batch is fully accepted; re-sending
// the exact same batch is fully skipped as duplicates (the ClickHouse-
// backed existing-row check); a batch mixing brand-new records with
// repeats of an already-written one is partially accepted; and a
// request_id repeated WITHIN one batch is deduplicated in-batch, keeping
// only the first occurrence.
func TestIntegration_WriteIngestBatch_AcceptAndDuplicates(t *testing.T) {
	cfg := testConfig(t)
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()

	is, ok := s.(sink.IngestSink)
	if !ok {
		t.Fatal("clickhouse sink does not implement sink.IngestSink")
	}
	reader := s.(analytics.Reader)

	tenantID := uniqueTenantID(t)
	now := time.Now().UTC()
	ctx := context.Background()

	access := []*sink.AccessLog{
		{Timestamp: now, TenantID: tenantID, RequestID: tenantID + "-a1", Method: "tools/call", StatusCode: 200, Source: "interceptor", User: "a@example.com"},
		{Timestamp: now, TenantID: tenantID, RequestID: tenantID + "-a2", Method: "tools/call", StatusCode: 200, Source: "interceptor", User: "a@example.com"},
	}
	llm := []*sink.LLMCall{
		{Timestamp: now, TenantID: tenantID, RequestID: tenantID + "-l1", Model: "claude-opus", StatusCode: 200, Source: "interceptor", User: "a@example.com"},
	}

	// First send: everything is new.
	result, err := is.WriteIngestBatch(ctx, tenantID, access, llm)
	if err != nil {
		t.Fatalf("WriteIngestBatch() (1st) error = %v", err)
	}
	if result.AcceptedAccess != 2 || result.AcceptedLLM != 1 || result.DuplicateAccess != 0 || result.DuplicateLLM != 0 {
		t.Fatalf("1st WriteIngestBatch result = %+v, want {Accepted: 2/1, Duplicate: 0/0}", result)
	}

	// Re-send the EXACT same batch: every record already exists.
	result, err = is.WriteIngestBatch(ctx, tenantID, access, llm)
	if err != nil {
		t.Fatalf("WriteIngestBatch() (2nd, exact repeat) error = %v", err)
	}
	if result.AcceptedAccess != 0 || result.AcceptedLLM != 0 || result.DuplicateAccess != 2 || result.DuplicateLLM != 1 {
		t.Fatalf("2nd WriteIngestBatch result = %+v, want {Accepted: 0/0, Duplicate: 2/1}", result)
	}

	// Mixed batch: 1 already-written record + 1 brand-new one, per table.
	mixedAccess := []*sink.AccessLog{
		access[0], // already written
		{Timestamp: now, TenantID: tenantID, RequestID: tenantID + "-a3", Method: "tools/call", StatusCode: 200, Source: "interceptor"},
	}
	mixedLLM := []*sink.LLMCall{
		llm[0], // already written
		{Timestamp: now, TenantID: tenantID, RequestID: tenantID + "-l2", Model: "claude-opus", StatusCode: 200, Source: "interceptor"},
	}
	result, err = is.WriteIngestBatch(ctx, tenantID, mixedAccess, mixedLLM)
	if err != nil {
		t.Fatalf("WriteIngestBatch() (mixed) error = %v", err)
	}
	if result.AcceptedAccess != 1 || result.AcceptedLLM != 1 || result.DuplicateAccess != 1 || result.DuplicateLLM != 1 {
		t.Fatalf("mixed WriteIngestBatch result = %+v, want {Accepted: 1/1, Duplicate: 1/1}", result)
	}

	// In-batch duplicate: the SAME request_id twice in one call, never
	// before seen by the store -- only the first occurrence is written,
	// the second is counted as a duplicate without ever reaching the DB.
	dupID := tenantID + "-a4"
	inBatchDup := []*sink.AccessLog{
		{Timestamp: now, TenantID: tenantID, RequestID: dupID, Method: "tools/call", StatusCode: 200, Source: "interceptor"},
		{Timestamp: now, TenantID: tenantID, RequestID: dupID, Method: "tools/call", StatusCode: 200, Source: "interceptor"},
	}
	result, err = is.WriteIngestBatch(ctx, tenantID, inBatchDup, nil)
	if err != nil {
		t.Fatalf("WriteIngestBatch() (in-batch dup) error = %v", err)
	}
	if result.AcceptedAccess != 1 || result.DuplicateAccess != 1 {
		t.Fatalf("in-batch dup WriteIngestBatch result = %+v, want {Accepted: 1, Duplicate: 1}", result)
	}

	// Sanity: exactly 4 distinct access rows and 2 distinct llm rows ended
	// up durably written for this tenant (a1, a2, a3, a4-first-occurrence;
	// l1, l2) -- never more, despite every duplicate attempt above.
	items, total, err := reader.ListAccessLogs(ctx, tenantID, analytics.AccessLogFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListAccessLogs() error = %v", err)
	}
	if total != 4 || len(items) != 4 {
		t.Errorf("access rows for tenant = %d (items=%d), want 4", total, len(items))
	}
	_, llmTotal, err := reader.ListLLMCalls(ctx, tenantID, analytics.LLMCallFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListLLMCalls() error = %v", err)
	}
	if llmTotal != 2 {
		t.Errorf("llm rows for tenant = %d, want 2", llmTotal)
	}
}

// TestIntegration_ReaderFiltersBySourceAndUser writes one gateway-sourced
// row and one interceptor-sourced row to each table via the normal async
// WriteAccess/WriteLLMCall path (source left "" -- i.e. the implicit
// "gateway" default -- for the first, stamped "interceptor" + a user email
// for the second, exactly as Ingest.buildAccessLog/buildLLMCall do), then
// asserts List/GetAccessLog and List/GetLLMCall's source/user filters and
// returned fields behave correctly.
func TestIntegration_ReaderFiltersBySourceAndUser(t *testing.T) {
	cfg := testConfig(t)
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Close()
	reader := s.(analytics.Reader)

	tenantID := uniqueTenantID(t)
	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s.WriteAccess(&sink.AccessLog{Timestamp: now, TenantID: tenantID, RequestID: tenantID + "-gw", Method: "tools/call", StatusCode: 200})
	s.WriteAccess(&sink.AccessLog{Timestamp: now, TenantID: tenantID, RequestID: tenantID + "-icp", Method: "tools/call", StatusCode: 200, Source: "interceptor", User: "person@example.com"})
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, TenantID: tenantID, RequestID: tenantID + "-gw-llm", Model: "claude-opus", StatusCode: 200})
	s.WriteLLMCall(&sink.LLMCall{Timestamp: now, TenantID: tenantID, RequestID: tenantID + "-icp-llm", Model: "claude-opus", StatusCode: 200, Source: "interceptor", User: "person@example.com"})

	waitForRowCounts(t, ctx, reader, tenantID, 2, 2)

	// source filter: access logs.
	gwOnly, gwTotal, err := reader.ListAccessLogs(ctx, tenantID, analytics.AccessLogFilter{Source: "gateway"})
	if err != nil {
		t.Fatalf("ListAccessLogs(source=gateway) error = %v", err)
	}
	if gwTotal != 1 || len(gwOnly) != 1 || gwOnly[0].RequestID != tenantID+"-gw" {
		t.Fatalf("ListAccessLogs(source=gateway) = %+v (total=%d), want exactly the gateway row", gwOnly, gwTotal)
	}
	if gwOnly[0].Source != "gateway" || gwOnly[0].User != "" {
		t.Errorf("gateway row source/user = %q/%q, want gateway/\"\"", gwOnly[0].Source, gwOnly[0].User)
	}

	icpOnly, icpTotal, err := reader.ListAccessLogs(ctx, tenantID, analytics.AccessLogFilter{Source: "interceptor"})
	if err != nil {
		t.Fatalf("ListAccessLogs(source=interceptor) error = %v", err)
	}
	if icpTotal != 1 || len(icpOnly) != 1 || icpOnly[0].RequestID != tenantID+"-icp" {
		t.Fatalf("ListAccessLogs(source=interceptor) = %+v (total=%d), want exactly the interceptor row", icpOnly, icpTotal)
	}
	if icpOnly[0].Source != "interceptor" || icpOnly[0].User != "person@example.com" {
		t.Errorf("interceptor row source/user = %q/%q, want interceptor/person@example.com", icpOnly[0].Source, icpOnly[0].User)
	}

	// user filter: access logs.
	byUser, byUserTotal, err := reader.ListAccessLogs(ctx, tenantID, analytics.AccessLogFilter{User: "person@example.com"})
	if err != nil {
		t.Fatalf("ListAccessLogs(user=person@example.com) error = %v", err)
	}
	if byUserTotal != 1 || len(byUser) != 1 || byUser[0].RequestID != tenantID+"-icp" {
		t.Fatalf("ListAccessLogs(user=person@example.com) = %+v (total=%d), want exactly the interceptor row", byUser, byUserTotal)
	}

	// GetAccessLog carries source/user too.
	oneAccess, err := reader.GetAccessLog(ctx, tenantID, tenantID+"-icp")
	if err != nil {
		t.Fatalf("GetAccessLog() error = %v", err)
	}
	if oneAccess.Source != "interceptor" || oneAccess.User != "person@example.com" {
		t.Errorf("GetAccessLog source/user = %q/%q, want interceptor/person@example.com", oneAccess.Source, oneAccess.User)
	}

	// source + user filters: llm_calls.
	llmGW, llmGWTotal, err := reader.ListLLMCalls(ctx, tenantID, analytics.LLMCallFilter{Source: "gateway"})
	if err != nil {
		t.Fatalf("ListLLMCalls(source=gateway) error = %v", err)
	}
	if llmGWTotal != 1 || len(llmGW) != 1 || llmGW[0].RequestID != tenantID+"-gw-llm" {
		t.Fatalf("ListLLMCalls(source=gateway) = %+v (total=%d), want exactly the gateway row", llmGW, llmGWTotal)
	}

	llmICP, llmICPTotal, err := reader.ListLLMCalls(ctx, tenantID, analytics.LLMCallFilter{User: "person@example.com"})
	if err != nil {
		t.Fatalf("ListLLMCalls(user=person@example.com) error = %v", err)
	}
	if llmICPTotal != 1 || len(llmICP) != 1 || llmICP[0].RequestID != tenantID+"-icp-llm" {
		t.Fatalf("ListLLMCalls(user=person@example.com) = %+v (total=%d), want exactly the interceptor row", llmICP, llmICPTotal)
	}

	oneLLM, err := reader.GetLLMCall(ctx, tenantID, tenantID+"-icp-llm")
	if err != nil {
		t.Fatalf("GetLLMCall() error = %v", err)
	}
	if oneLLM.Source != "interceptor" || oneLLM.User != "person@example.com" {
		t.Errorf("GetLLMCall source/user = %q/%q, want interceptor/person@example.com", oneLLM.Source, oneLLM.User)
	}

	// Unfiltered lists still return every source (source="" means
	// unfiltered, not "gateway only").
	all, allTotal, err := reader.ListAccessLogs(ctx, tenantID, analytics.AccessLogFilter{})
	if err != nil {
		t.Fatalf("ListAccessLogs(unfiltered) error = %v", err)
	}
	if allTotal != 2 || len(all) != 2 {
		t.Errorf("ListAccessLogs(unfiltered) = %d rows (total=%d), want 2", len(all), allTotal)
	}
}
