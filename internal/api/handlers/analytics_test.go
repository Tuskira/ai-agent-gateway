package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/bodystore/fs"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// fakeAnalyticsReader is a hand-written analytics.Reader test double: it
// returns whatever's configured and records the arguments each method
// was called with, so tests can assert query params were parsed into the
// right filter without a real ClickHouse.
type fakeAnalyticsReader struct {
	overview    *analytics.Overview
	overviewErr error
	gotPeriod   analytics.Period

	accessItems  []sink.AccessLog
	accessTotal  int
	accessErr    error
	gotAccessF   analytics.AccessLogFilter
	accessOne    *sink.AccessLog
	accessOneErr error
	gotAccessID  string

	llmItems  []sink.LLMCall
	llmTotal  int
	llmErr    error
	gotLLMF   analytics.LLMCallFilter
	llmOne    *sink.LLMCall
	llmOneErr error
	gotLLMID  string

	modelsSummary  *analytics.ModelsSummary
	modelsErr      error
	gotModelsRange analytics.Range

	skillsSummary  *analytics.SkillsSummary
	skillsErr      error
	gotSkillsRange analytics.Period

	timeline        *analytics.SessionTimeline
	timelineErr     error
	gotTimelineID   string
	gotTimelineOpts analytics.TimelineOptions

	sankey    *analytics.Sankey
	flow      *analytics.TrafficFlow
	sankeyErr error

	skillUsage    []analytics.SkillUsage
	mcpUsage      []analytics.MCPToolUsage
	mcpCalls      []analytics.MCPServerCalls
	gotAliases    map[string]string
	usageErr      error
	gotUsageRange analytics.Period
	gotSankeyQ    analytics.SankeyQuery
}

func (f *fakeAnalyticsReader) Overview(_ context.Context, _ string, p analytics.Period) (*analytics.Overview, error) {
	f.gotPeriod = p
	return f.overview, f.overviewErr
}

func (f *fakeAnalyticsReader) ListAccessLogs(_ context.Context, _ string, filter analytics.AccessLogFilter) ([]sink.AccessLog, int, error) {
	f.gotAccessF = filter
	return f.accessItems, f.accessTotal, f.accessErr
}

func (f *fakeAnalyticsReader) GetAccessLog(_ context.Context, _, requestID string) (*sink.AccessLog, error) {
	f.gotAccessID = requestID
	return f.accessOne, f.accessOneErr
}

func (f *fakeAnalyticsReader) ListLLMCalls(_ context.Context, _ string, filter analytics.LLMCallFilter) ([]sink.LLMCall, int, error) {
	f.gotLLMF = filter
	return f.llmItems, f.llmTotal, f.llmErr
}

func (f *fakeAnalyticsReader) GetLLMCall(_ context.Context, _, requestID string) (*sink.LLMCall, error) {
	f.gotLLMID = requestID
	return f.llmOne, f.llmOneErr
}

func (f *fakeAnalyticsReader) ModelsSummary(_ context.Context, _ string, r analytics.Range) (*analytics.ModelsSummary, error) {
	f.gotModelsRange = r
	return f.modelsSummary, f.modelsErr
}

func (f *fakeAnalyticsReader) SkillsSummary(_ context.Context, _ string, r analytics.Period) (*analytics.SkillsSummary, error) {
	f.gotSkillsRange = r
	return f.skillsSummary, f.skillsErr
}

func (f *fakeAnalyticsReader) SessionTimeline(_ context.Context, _, sessionID string, opts analytics.TimelineOptions) (*analytics.SessionTimeline, error) {
	f.gotTimelineID = sessionID
	f.gotTimelineOpts = opts
	if f.timeline == nil {
		return nil, f.timelineErr
	}
	// Mimic the real reader: sort a copy in the requested direction.
	tl := *f.timeline
	tl.Events = append([]analytics.TimelineEvent(nil), f.timeline.Events...)
	analytics.SortTimelineEvents(tl.Events, opts.Order)
	return &tl, f.timelineErr
}

func (f *fakeAnalyticsReader) ClientModelSankey(_ context.Context, _ string, q analytics.SankeyQuery) (*analytics.Sankey, error) {
	f.gotSankeyQ = q
	return f.sankey, f.sankeyErr
}

func (f *fakeAnalyticsReader) TrafficFlowSankey(_ context.Context, _ string, q analytics.SankeyQuery) (*analytics.TrafficFlow, error) {
	f.gotSankeyQ = q
	return f.flow, f.sankeyErr
}

func (f *fakeAnalyticsReader) SkillUsage(_ context.Context, _ string, r analytics.Period) ([]analytics.SkillUsage, error) {
	f.gotUsageRange = r
	return f.skillUsage, f.usageErr
}

func (f *fakeAnalyticsReader) MCPToolUsage(_ context.Context, _ string, r analytics.Period) ([]analytics.MCPToolUsage, error) {
	f.gotUsageRange = r
	return f.mcpUsage, f.usageErr
}

func (f *fakeAnalyticsReader) MCPServerCalls(_ context.Context, _ string, r analytics.Period, aliases map[string]string) ([]analytics.MCPServerCalls, error) {
	f.gotUsageRange, f.gotAliases = r, aliases
	return f.mcpCalls, f.usageErr
}

// presetOf names the preset a Period spans, or "custom" when it spans none
// (assertions read better as presets than as durations).
func presetOf(p analytics.Period) analytics.Range {
	for _, r := range []analytics.Range{analytics.Range24h, analytics.Range7d, analytics.Range30d} {
		if p.End.Sub(p.Start) == r.Window() {
			return r
		}
	}
	return analytics.RangeCustom
}

func newAnalyticsTestDeps(reader *fakeAnalyticsReader) Deps {
	d := newTestDeps()
	if reader != nil {
		d.Analytics = reader
	}
	return d
}

func TestAnalytics_Overview_NilReader404(t *testing.T) {
	h := Analytics{Deps: newAnalyticsTestDeps(nil)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/overview", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/overview", h.Overview, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "not_found")
}

func TestAnalytics_Overview_DefaultsRangeTo24h(t *testing.T) {
	fake := &fakeAnalyticsReader{overview: &analytics.Overview{Range: analytics.Range24h}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/overview", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/overview", h.Overview, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if span := fake.gotPeriod.End.Sub(fake.gotPeriod.Start); span != 24*time.Hour {
		t.Errorf("period passed to Reader spans %v, want 24h", span)
	}
	if !strings.Contains(w.Body.String(), `"range":"24h"`) {
		t.Errorf("body should label the range 24h: %s", w.Body.String())
	}
}

// from/to (YYYY-MM-DD, both inclusive) select whole UTC days; the response
// is labeled "custom" and the previous period is the same span before.
func TestAnalytics_Overview_CustomDates(t *testing.T) {
	fake := &fakeAnalyticsReader{overview: &analytics.Overview{}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/overview?from=2026-01-01&to=2026-01-03", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/overview", h.Overview, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	p := fake.gotPeriod
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !p.Start.Equal(start) || !p.End.Equal(start.AddDate(0, 0, 3)) || !p.PrevStart.Equal(start.AddDate(0, 0, -3)) {
		t.Errorf("period = %+v, want 2026-01-01..2026-01-04, prev from 2025-12-29", p)
	}
	if !strings.Contains(w.Body.String(), `"range":"custom"`) {
		t.Errorf("body should label the range custom: %s", w.Body.String())
	}

	for _, qs := range []string{"from=2026-01-03&to=2026-01-01", "from=2026-01-01", "from=jan&to=2026-01-02", "from=2999-01-01&to=2999-01-02", "from=2024-01-01&to=2026-01-01"} {
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/overview?"+qs, nil), "tenant-a", "agent")
		if w := serve(http.MethodGet, "/analytics/overview", h.Overview, req); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", qs, w.Code)
		}
	}
}

func TestAnalytics_Overview_InvalidRange400(t *testing.T) {
	fake := &fakeAnalyticsReader{overview: &analytics.Overview{}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/overview?range=3w", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/overview", h.Overview, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "validation_error")
}

func TestAnalytics_Overview_Happy(t *testing.T) {
	fake := &fakeAnalyticsReader{overview: &analytics.Overview{
		Range: analytics.Range7d,
		Kpis:  analytics.OverviewKpis{McpToolCalls: analytics.CountKpi{Value: 42}},
	}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/overview?range=7d", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/overview", h.Overview, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if span := fake.gotPeriod.End.Sub(fake.gotPeriod.Start); span != 7*24*time.Hour {
		t.Errorf("period spans %v, want 7d", span)
	}
	var got analytics.Overview
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kpis.McpToolCalls.Value != 42 {
		t.Errorf("mcpToolCalls = %v, want 42", got.Kpis.McpToolCalls.Value)
	}
	// Field names in the wire JSON must be camelCase (matches
	// web/src/lib/overview.ts's OverviewMetrics), not snake_case.
	if _, ok := rawField(t, w.Body.Bytes(), "kpis", "mcpToolCalls"); !ok {
		t.Error(`response JSON must have kpis.mcpToolCalls (camelCase)`)
	}
}

// TestAnalytics_Overview_StatusCodesBucketLabels guards the outcome-bucket
// contract: statusCodes is bucketed by outcome ("200"/"204"/"error"), not
// by raw HTTP status class, and the handler must pass those labels and
// SuccessPct through byte-for-byte -- it does no relabeling of its own.
// The actual bucketing rule lives in pkg/sink/clickhouse's Reader
// (exercised for real in integration_test.go); this only pins the
// handler's pass-through.
func TestAnalytics_Overview_StatusCodesBucketLabels(t *testing.T) {
	fake := &fakeAnalyticsReader{overview: &analytics.Overview{
		Range: analytics.Range24h,
		StatusCodes: []analytics.StatusCodeSlice{
			{Label: analytics.OutcomeSuccess, Pct: 76},
			{Label: analytics.OutcomeNotification, Pct: 8},
			{Label: analytics.OutcomeError, Pct: 16},
		},
		SuccessPct: 82.6086956521739,
		Kpis: analytics.OverviewKpis{
			SuccessRate: analytics.SuccessRateKpi{Value: 82.6086956521739, Sub: "19 / 23 responses"},
		},
	}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/overview", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/overview", h.Overview, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var got analytics.Overview
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantLabels := []string{analytics.OutcomeSuccess, analytics.OutcomeNotification, analytics.OutcomeError}
	if len(got.StatusCodes) != len(wantLabels) {
		t.Fatalf("statusCodes = %+v, want %d buckets", got.StatusCodes, len(wantLabels))
	}
	for i, want := range wantLabels {
		if got.StatusCodes[i].Label != want {
			t.Errorf("statusCodes[%d].Label = %q, want %q (no HTTP-status-class labels like \"2xx\")", i, got.StatusCodes[i].Label, want)
		}
	}
	if got.SuccessPct != 82.6086956521739 {
		t.Errorf("successPct = %v, not passed through unchanged", got.SuccessPct)
	}
	if got.Kpis.SuccessRate.Sub != "19 / 23 responses" {
		t.Errorf("kpis.successRate.sub = %q, not passed through unchanged", got.Kpis.SuccessRate.Sub)
	}
}

// TestAnalytics_Overview_CostEstimateFields pins that the handler stamps
// costEstimated (always true -- every dollar figure comes from
// pkg/pricing's rate card, not a provider invoice) and pricingSource
// (from h.AnalyticsMeta, defaulting to "embedded" when unset) onto the
// Reader's response, without the Reader itself knowing about either.
func TestAnalytics_Overview_CostEstimateFields(t *testing.T) {
	fake := &fakeAnalyticsReader{overview: &analytics.Overview{Range: analytics.Range24h}}

	t.Run("defaults to embedded when AnalyticsMeta is unset", func(t *testing.T) {
		h := Analytics{Deps: newAnalyticsTestDeps(fake)}
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/overview", nil), "tenant-a", "agent")
		w := serve(http.MethodGet, "/analytics/overview", h.Overview, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var got analytics.Overview
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !got.CostEstimated {
			t.Error("costEstimated = false, want true")
		}
		if got.PricingSource != analytics.PricingSourceEmbedded {
			t.Errorf("pricingSource = %q, want %q", got.PricingSource, analytics.PricingSourceEmbedded)
		}
		// Field names in the wire JSON must be camelCase (matches
		// web/src/lib/overview.ts's OverviewMetrics), not snake_case.
		if _, ok := rawField(t, w.Body.Bytes(), "costEstimated"); !ok {
			t.Error(`response JSON must have top-level costEstimated (camelCase)`)
		}
		if _, ok := rawField(t, w.Body.Bytes(), "pricingSource"); !ok {
			t.Error(`response JSON must have top-level pricingSource (camelCase)`)
		}
	})

	t.Run("reflects AnalyticsMeta.PricingSource=file", func(t *testing.T) {
		deps := newAnalyticsTestDeps(fake)
		deps.AnalyticsMeta = analytics.Meta{PricingSource: analytics.PricingSourceFile}
		h := Analytics{Deps: deps}
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/overview", nil), "tenant-a", "agent")
		w := serve(http.MethodGet, "/analytics/overview", h.Overview, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var got analytics.Overview
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.PricingSource != analytics.PricingSourceFile {
			t.Errorf("pricingSource = %q, want %q", got.PricingSource, analytics.PricingSourceFile)
		}
	})
}

// TestAnalytics_Overview_ResolvesConnectorNames covers resolveConnectorNames:
// the ClickHouse Reader only knows connector ids (a foreign key into
// Postgres, which it has no access to), so the handler must resolve them
// to display names via h.Store before the response goes out. An id whose
// connector no longer exists (deleted since the log row was written)
// keeps the id rather than dropping the row.
func TestAnalytics_Overview_ResolvesConnectorNames(t *testing.T) {
	deps := newAnalyticsTestDeps(nil)
	connID := createTestConnector(t, deps, "tenant-a")

	fake := &fakeAnalyticsReader{overview: &analytics.Overview{
		Range: analytics.Range24h,
		TopConnectors: []analytics.NamedCount{
			{Name: connID, Count: 10},
			{Name: "deleted-connector-id", Count: 3},
		},
	}}
	deps.Analytics = fake
	h := Analytics{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/overview", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/overview", h.Overview, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var got analytics.Overview
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.TopConnectors) != 2 {
		t.Fatalf("topConnectors = %+v, want 2 rows", got.TopConnectors)
	}
	if got.TopConnectors[0].Name != "Okta MCP!" {
		t.Errorf("topConnectors[0].Name = %q, want the connector's display name %q", got.TopConnectors[0].Name, "Okta MCP!")
	}
	if got.TopConnectors[1].Name != "deleted-connector-id" {
		t.Errorf("topConnectors[1].Name = %q, want the raw id kept as a fallback", got.TopConnectors[1].Name)
	}
}

func TestAnalytics_Models_NilReader404(t *testing.T) {
	h := Analytics{Deps: newAnalyticsTestDeps(nil)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/models", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/models", h.Models, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "not_found")
}

func TestAnalytics_Models_DefaultsRangeTo7d(t *testing.T) {
	fake := &fakeAnalyticsReader{modelsSummary: &analytics.ModelsSummary{Range: analytics.Range7d}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/models", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/models", h.Models, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	// Models defaults to 7d, unlike Overview's 24h.
	if fake.gotModelsRange != analytics.Range7d {
		t.Errorf("range passed to Reader = %q, want %q", fake.gotModelsRange, analytics.Range7d)
	}
}

func TestAnalytics_Models_InvalidRange400(t *testing.T) {
	fake := &fakeAnalyticsReader{modelsSummary: &analytics.ModelsSummary{}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/models?range=3w", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/models", h.Models, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "validation_error")
}

func TestAnalytics_Models_Happy(t *testing.T) {
	cost := 1.5
	fake := &fakeAnalyticsReader{modelsSummary: &analytics.ModelsSummary{
		Range: analytics.Range7d,
		Models: []analytics.ModelSummaryRow{
			{Name: "claude-sonnet-4-5", Provider: "anthropic", Calls: 10, Tokens: 1000, CostUSD: &cost, UsedBy: 2, Status: "active"},
			{Name: "unpriced-model", Provider: "anthropic", Calls: 1, Tokens: 5, UsedBy: 1, Status: "active"},
		},
		TotalModels:    2,
		HighestTraffic: &analytics.ModelHighestTraffic{Name: "claude-sonnet-4-5", Tokens: 1000},
	}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/models?range=7d", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/models", h.Models, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fake.gotModelsRange != analytics.Range7d {
		t.Errorf("range = %q, want 7d", fake.gotModelsRange)
	}

	var got analytics.ModelsSummary
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TotalModels != 2 || len(got.Models) != 2 {
		t.Fatalf("summary = %+v", got)
	}
	if got.Models[0].CostUSD == nil || *got.Models[0].CostUSD != 1.5 {
		t.Errorf("Models[0].CostUSD = %v, want 1.5", got.Models[0].CostUSD)
	}
	if got.Models[1].CostUSD != nil {
		t.Errorf("Models[1].CostUSD = %v, want nil (no priced calls)", *got.Models[1].CostUSD)
	}
	if got.HighestTraffic == nil || got.HighestTraffic.Name != "claude-sonnet-4-5" || got.HighestTraffic.Tokens != 1000 {
		t.Errorf("HighestTraffic = %+v", got.HighestTraffic)
	}
	// Field names in the wire JSON must be snake_case (matches
	// web/src/lib/models.ts), not camelCase like Overview's.
	if _, ok := rawField(t, w.Body.Bytes(), "total_models"); !ok {
		t.Error(`response JSON must have top-level total_models (snake_case)`)
	}
	if _, ok := rawField(t, w.Body.Bytes(), "highest_traffic", "tokens"); !ok {
		t.Error(`response JSON must have highest_traffic.tokens (snake_case)`)
	}
}

func TestAnalytics_Models_ErrorFromReader(t *testing.T) {
	fake := &fakeAnalyticsReader{modelsErr: errors.New("boom")}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/models", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/models", h.Models, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", w.Code, w.Body.String())
	}
}

func TestAnalytics_Skills_NilReader404(t *testing.T) {
	h := Analytics{Deps: newAnalyticsTestDeps(nil)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/skills", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/skills", h.Skills, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "not_found")
}

func TestAnalytics_Skills_DefaultsRangeTo7d(t *testing.T) {
	fake := &fakeAnalyticsReader{skillsSummary: &analytics.SkillsSummary{Range: analytics.Range7d}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/skills", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/skills", h.Skills, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	// Skills defaults to 7d, like Models (not Overview's 24h).
	if presetOf(fake.gotSkillsRange) != analytics.Range7d {
		t.Errorf("range passed to Reader = %q, want %q", presetOf(fake.gotSkillsRange), analytics.Range7d)
	}
}

func TestAnalytics_Skills_InvalidRange400(t *testing.T) {
	fake := &fakeAnalyticsReader{skillsSummary: &analytics.SkillsSummary{}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/skills?range=3w", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/skills", h.Skills, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "validation_error")
}

// TestAnalytics_Skills_Happy exercises the registry join (resolveSkillKinds):
// a tenant "skill" row and a tenant "command" row both get their Kind
// filled in by name, and a name with traffic but no matching registry row
// (deleted since, or never existed) keeps Kind "" rather than being
// dropped or failing the request.
func TestAnalytics_Skills_Happy(t *testing.T) {
	fake := &fakeAnalyticsReader{skillsSummary: &analytics.SkillsSummary{
		Range: analytics.Range7d,
		Skills: []analytics.SkillSummaryRow{
			{Name: "review-pr", Calls: 10, UsedBy: 2},
			{Name: "summarize", Calls: 5, UsedBy: 1},
			{Name: "deleted-skill", Calls: 1, UsedBy: 1},
		},
		TotalSkills: 3,
		MostUsed:    &analytics.SkillMostUsed{Name: "review-pr", Calls: 10},
	}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}

	// Seed the registry directly through the fake store -- no need to go
	// through the Skills handler's SKILL.md validation for this test.
	ctx := context.Background()
	if err := h.Store.Skills().Create(ctx, &store.Skill{TenantID: "tenant-a", Name: "review-pr", Kind: "skill", Description: "Reviews a PR"},
		[]store.SkillFile{{Path: "SKILL.md", Content: "---\nname: review-pr\ndescription: Reviews a PR\n---\nBody"}}, "test"); err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	if err := h.Store.Skills().Create(ctx, &store.Skill{TenantID: "tenant-a", Name: "summarize", Kind: "command", Description: "Summarizes"},
		[]store.SkillFile{{Path: "SKILL.md", Content: "---\nname: summarize\ndescription: Summarizes\n---\nBody"}}, "test"); err != nil {
		t.Fatalf("seed command: %v", err)
	}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/skills?range=7d", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/skills", h.Skills, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if presetOf(fake.gotSkillsRange) != analytics.Range7d {
		t.Errorf("range = %q, want 7d", presetOf(fake.gotSkillsRange))
	}

	var got analytics.SkillsSummary
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TotalSkills != 3 || len(got.Skills) != 3 {
		t.Fatalf("summary = %+v", got)
	}

	byName := map[string]analytics.SkillSummaryRow{}
	for _, row := range got.Skills {
		byName[row.Name] = row
	}
	if byName["review-pr"].Kind != "skill" {
		t.Errorf("review-pr Kind = %q, want %q", byName["review-pr"].Kind, "skill")
	}
	if byName["summarize"].Kind != "command" {
		t.Errorf("summarize Kind = %q, want %q", byName["summarize"].Kind, "command")
	}
	if byName["deleted-skill"].Kind != "" {
		t.Errorf("deleted-skill Kind = %q, want \"\" (no matching registry row)", byName["deleted-skill"].Kind)
	}
	if got.MostUsed == nil || got.MostUsed.Name != "review-pr" || got.MostUsed.Kind != "skill" {
		t.Errorf("MostUsed = %+v, want review-pr/skill", got.MostUsed)
	}
	// Field names in the wire JSON must be snake_case, matching Models'
	// own convention (see web/src/lib/analytics.ts).
	if _, ok := rawField(t, w.Body.Bytes(), "total_skills"); !ok {
		t.Error(`response JSON must have top-level total_skills (snake_case)`)
	}
	if _, ok := rawField(t, w.Body.Bytes(), "most_used", "kind"); !ok {
		t.Error(`response JSON must have most_used.kind (snake_case)`)
	}
}

func TestAnalytics_Skills_ErrorFromReader(t *testing.T) {
	fake := &fakeAnalyticsReader{skillsErr: errors.New("boom")}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/skills", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/skills", h.Skills, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", w.Code, w.Body.String())
	}
}

func TestAnalytics_ClientModelSankey_NilReader404(t *testing.T) {
	h := Analytics{Deps: newAnalyticsTestDeps(nil)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/client-models", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/client-models", h.ClientModelSankey, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "not_found")
}

func TestAnalytics_ClientModelSankey_Defaults(t *testing.T) {
	fake := &fakeAnalyticsReader{sankey: &analytics.Sankey{Range: analytics.Range7d, Metric: analytics.SankeyMetricCalls}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/client-models", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/client-models", h.ClientModelSankey, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	// Defaults to 7d (like Models/Skills) and metric=calls, limit=10.
	if fake.gotSankeyQ.Range != analytics.Range7d {
		t.Errorf("range = %q, want 7d", fake.gotSankeyQ.Range)
	}
	if fake.gotSankeyQ.Metric != analytics.SankeyMetricCalls {
		t.Errorf("metric = %q, want calls", fake.gotSankeyQ.Metric)
	}
	if fake.gotSankeyQ.Limit != analytics.SankeyDefaultLimit {
		t.Errorf("limit = %d, want %d", fake.gotSankeyQ.Limit, analytics.SankeyDefaultLimit)
	}
	if fake.gotSankeyQ.ClientName != "" {
		t.Errorf("client_name = %q, want empty", fake.gotSankeyQ.ClientName)
	}
}

func TestAnalytics_ClientModelSankey_InvalidRange400(t *testing.T) {
	fake := &fakeAnalyticsReader{sankey: &analytics.Sankey{}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/client-models?range=3w", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/client-models", h.ClientModelSankey, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "validation_error")
}

func TestAnalytics_ClientModelSankey_InvalidMetric400(t *testing.T) {
	fake := &fakeAnalyticsReader{sankey: &analytics.Sankey{}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/client-models?metric=bytes", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/client-models", h.ClientModelSankey, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "validation_error")
}

func TestAnalytics_ClientModelSankey_InvalidLimit400(t *testing.T) {
	fake := &fakeAnalyticsReader{sankey: &analytics.Sankey{}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	for _, raw := range []string{"0", "-1", "abc"} {
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/client-models?limit="+raw, nil), "tenant-a", "agent")
		w := serve(http.MethodGet, "/analytics/client-models", h.ClientModelSankey, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("limit=%q: status = %d, want 400, body = %s", raw, w.Code, w.Body.String())
		}
	}
}

func TestAnalytics_ClientModelSankey_ParsesQueryParams(t *testing.T) {
	fake := &fakeAnalyticsReader{sankey: &analytics.Sankey{}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/client-models?range=24h&metric=tokens&limit=3&client_name=cursor", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/client-models", h.ClientModelSankey, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	want := analytics.SankeyQuery{
		Range: analytics.Range24h, Metric: analytics.SankeyMetricTokens, Limit: 3, ClientName: "cursor",
	}
	got := fake.gotSankeyQ
	if span := got.Period.End.Sub(got.Period.Start); span != 24*time.Hour {
		t.Errorf("period spans %v, want 24h", span)
	}
	got.Period = analytics.Period{}
	if got != want {
		t.Errorf("query = %+v, want %+v", got, want)
	}
}

func TestAnalytics_ClientModelSankey_Happy(t *testing.T) {
	fake := &fakeAnalyticsReader{sankey: &analytics.Sankey{
		Range:  analytics.Range7d,
		Metric: analytics.SankeyMetricCalls,
		Total:  400,
		Nodes: []analytics.SankeyNode{
			{ID: "CLIENT:cursor", Key: "cursor", Label: "Cursor", Type: "CLIENT", Value: 400},
			{ID: "MODEL:claude-sonnet-5", Key: "claude-sonnet-5", Label: "claude-sonnet-5", Type: "MODEL", Value: 400},
			{ID: "PROVIDER:anthropic", Key: "anthropic", Label: "anthropic", Type: "PROVIDER", Value: 400},
		},
		Links: []analytics.SankeyLink{
			{Source: "cursor", SourceType: "CLIENT", Target: "claude-sonnet-5", TargetType: "MODEL", Value: 400},
			{Source: "claude-sonnet-5", SourceType: "MODEL", Target: "anthropic", TargetType: "PROVIDER", Value: 400},
		},
	}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/client-models?range=7d", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/client-models", h.ClientModelSankey, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var got analytics.Sankey
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 400 || len(got.Nodes) != 3 || len(got.Links) != 2 {
		t.Fatalf("sankey = %+v", got)
	}
	// Field names in the wire JSON: camelCase for sourceType/targetType
	// (the common nivo Sankey convention), snake_case-style top
	// level fields following /models' convention otherwise.
	if _, ok := rawField(t, w.Body.Bytes(), "links"); !ok {
		t.Error(`response JSON must have top-level links`)
	}
	linksRaw, _ := rawField(t, w.Body.Bytes(), "links")
	if !strings.Contains(string(linksRaw), `"sourceType"`) {
		t.Error(`response JSON links must use camelCase "sourceType"`)
	}
}

func TestAnalytics_ClientModelSankey_ErrorFromReader(t *testing.T) {
	fake := &fakeAnalyticsReader{sankeyErr: errors.New("boom")}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/client-models", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/client-models", h.ClientModelSankey, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", w.Code, w.Body.String())
	}
}

func TestAnalytics_Logs_NilReader404(t *testing.T) {
	h := Analytics{Deps: newAnalyticsTestDeps(nil)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/logs", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/logs", h.Logs, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestAnalytics_Logs_ParsesFiltersFromQuery(t *testing.T) {
	fake := &fakeAnalyticsReader{accessItems: []sink.AccessLog{{RequestID: "r1"}}, accessTotal: 1}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}

	url := "/analytics/logs?limit=10&offset=5&method=tools%2Fcall&connector_id=conn-1&tool=echo&session_id=sess-1&principal=user-1&status=200&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z"
	req := withPrincipal(httptest.NewRequest(http.MethodGet, url, nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/logs", h.Logs, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	f := fake.gotAccessF
	if f.Limit != 10 || f.Offset != 5 || f.Method != "tools/call" || f.ConnectorID != "conn-1" ||
		f.ToolName != "echo" || f.SessionID != "sess-1" || f.Principal != "user-1" || f.Status != 200 {
		t.Errorf("filter = %+v", f)
	}
	wantFrom, _ := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
	wantTo, _ := time.Parse(time.RFC3339, "2026-01-02T00:00:00Z")
	if !f.From.Equal(wantFrom) || !f.To.Equal(wantTo) {
		t.Errorf("from/to = %v/%v, want %v/%v", f.From, f.To, wantFrom, wantTo)
	}

	var page struct {
		Items []sink.AccessLog `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].RequestID != "r1" {
		t.Errorf("page = %+v", page)
	}
}

func TestAnalytics_Logs_InvalidStatus400(t *testing.T) {
	fake := &fakeAnalyticsReader{}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/logs?status=not-a-number", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/logs", h.Logs, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	assertErrorType(t, w, "validation_error")
}

func TestAnalytics_Logs_InvalidFrom400(t *testing.T) {
	fake := &fakeAnalyticsReader{}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/logs?from=not-a-date", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/logs", h.Logs, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	assertErrorType(t, w, "validation_error")
}

func TestAnalytics_LogsGet_Happy(t *testing.T) {
	fake := &fakeAnalyticsReader{accessOne: &sink.AccessLog{RequestID: "r1", RequestBody: []byte(`{"a":1}`)}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/logs/r1", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/analytics/logs/{request_id}", h.LogsGet, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fake.gotAccessID != "r1" {
		t.Errorf("request id passed = %q, want r1", fake.gotAccessID)
	}
	var got sink.AccessLog
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got.RequestBody) != `{"a":1}` {
		t.Errorf("request body = %q, want the fake's body (get-one includes bodies)", got.RequestBody)
	}
}

func TestAnalytics_LogsGet_NotFound(t *testing.T) {
	fake := &fakeAnalyticsReader{accessOneErr: analytics.ErrNotFound}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/logs/missing", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/analytics/logs/{request_id}", h.LogsGet, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
}

func TestAnalytics_LLMLogs_ParsesFiltersFromQuery(t *testing.T) {
	fake := &fakeAnalyticsReader{llmItems: []sink.LLMCall{{RequestID: "l1"}}, llmTotal: 1}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}

	url := "/analytics/llm-logs?limit=5&model=claude-opus&session_id=sess-1&principal=user-1&client_name=claude-code&status=500"
	req := withPrincipal(httptest.NewRequest(http.MethodGet, url, nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/llm-logs", h.LLMLogs, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	f := fake.gotLLMF
	if f.Limit != 5 || f.Model != "claude-opus" || f.SessionID != "sess-1" || f.Principal != "user-1" ||
		f.ClientName != "claude-code" || f.Status != 500 {
		t.Errorf("filter = %+v", f)
	}
}

func TestAnalytics_LLMLogsGet_Happy(t *testing.T) {
	fake := &fakeAnalyticsReader{llmOne: &sink.LLMCall{RequestID: "l1", Messages: []byte(`[]`)}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/llm-logs/l1", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/analytics/llm-logs/{request_id}", h.LLMLogsGet, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fake.gotLLMID != "l1" {
		t.Errorf("request id passed = %q, want l1", fake.gotLLMID)
	}
}

// An offloaded row's bodies are resolved from the BodyStore into the same
// request_body/response_body fields an inline row carries, so the console
// reads one shape. Without a store, or with the bodies gone, the row still
// answers 200 with bodies_unavailable set.
func TestAnalytics_LLMLogsGet_ResolvesBodyRef(t *testing.T) {
	store, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(context.Background(), "l1", []byte(`{"q":1}`), []byte(`{"a":2}`))
	if err != nil {
		t.Fatal(err)
	}

	get := func(row *sink.LLMCall, bs sink.BodyStore) map[string]any {
		t.Helper()
		d := newAnalyticsTestDeps(&fakeAnalyticsReader{llmOne: row})
		d.BodyStore = bs
		h := Analytics{Deps: d}
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/llm-logs/l1", nil), "tenant-a", "admin")
		w := serve(http.MethodGet, "/analytics/llm-logs/{request_id}", h.LLMLogsGet, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	b64 := func(s string) string { b, _ := json.Marshal([]byte(s)); return string(b[1 : len(b)-1]) }

	m := get(&sink.LLMCall{RequestID: "l1", BodyRef: ref}, store)
	if m["request_body"] != b64(`{"q":1}`) || m["response_body"] != b64(`{"a":2}`) || m["body_ref"] != ref || m["bodies_unavailable"] != nil {
		t.Errorf("resolved detail = %v", m)
	}

	m = get(&sink.LLMCall{RequestID: "l1", BodyRef: ref}, nil)
	if m["request_body"] != nil || m["bodies_unavailable"] != "body store not configured on this instance" {
		t.Errorf("no-store detail = %v", m)
	}

	m = get(&sink.LLMCall{RequestID: "l1", BodyRef: "fs://gone-1"}, store)
	if m["request_body"] != nil || m["bodies_unavailable"] != "bodies not found in the body store (expired or deleted)" {
		t.Errorf("missing-bodies detail = %v", m)
	}

	// An inline row is untouched: bodies as stored, no ref, no reason.
	m = get(&sink.LLMCall{RequestID: "l1", RequestBody: []byte("inline")}, store)
	if m["request_body"] != b64("inline") || m["body_ref"] != nil || m["bodies_unavailable"] != nil {
		t.Errorf("inline detail = %v", m)
	}
}

func TestAnalytics_SessionTimeline_NilReader404(t *testing.T) {
	h := Analytics{Deps: newAnalyticsTestDeps(nil)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/sessions/sess-1/timeline", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/sessions/{session_id}/timeline", h.SessionTimeline, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	assertErrorType(t, w, "not_found")
}

func TestAnalytics_SessionTimeline_Happy(t *testing.T) {
	fake := &fakeAnalyticsReader{timeline: &analytics.SessionTimeline{
		SessionID:  "sess-1",
		OwnerKeyID: "key-a",
		Events: []analytics.TimelineEvent{
			{Plane: "mcp", Kind: "tools/call", Name: "echo", Status: "success", KeyID: "key-a", ID: "req-1"},
			{Plane: "llm", Kind: "llm_call", Name: "claude-sonnet-4-5", Status: "success", KeyID: "key-a", ID: "req-2"},
		},
		TotalEvents:           2,
		ExcludedForeignEvents: 3,
	}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/sessions/sess-1/timeline", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/sessions/{session_id}/timeline", h.SessionTimeline, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fake.gotTimelineID != "sess-1" {
		t.Errorf("session id passed to reader = %q, want sess-1", fake.gotTimelineID)
	}

	var got analytics.SessionTimeline
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OwnerKeyID != "key-a" || got.TotalEvents != 2 || got.ExcludedForeignEvents != 3 || len(got.Events) != 2 {
		t.Errorf("timeline = %+v", got)
	}
}

func timelineOrderFixture() *fakeAnalyticsReader {
	t0 := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	return &fakeAnalyticsReader{timeline: &analytics.SessionTimeline{
		SessionID: "sess-1", OwnerKeyID: "key-a", TotalEvents: 4, ExcludedForeignEvents: 2,
		Events: []analytics.TimelineEvent{
			{Plane: "llm", ID: "b", Timestamp: t0},
			{Plane: "mcp", ID: "a", Timestamp: t0},
			{Plane: "mcp", ID: "c", Timestamp: t0.Add(time.Second)},
			{Plane: "llm", ID: "d", Timestamp: t0.Add(-time.Second)},
		},
	}}
}

func getTimeline(t *testing.T, fake *fakeAnalyticsReader, query string) (*httptest.ResponseRecorder, analytics.SessionTimeline) {
	t.Helper()
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/sessions/sess-1/timeline"+query, nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/sessions/{session_id}/timeline", h.SessionTimeline, req)
	var got analytics.SessionTimeline
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w, got
}

func timelineIDs(tl analytics.SessionTimeline) string {
	s := ""
	for _, e := range tl.Events {
		s += e.ID
	}
	return s
}

func TestAnalytics_SessionTimeline_Order(t *testing.T) {
	cases := []struct {
		query, wantOrder, wantIDs string
	}{
		{"", "asc", "dabc"},
		{"?order=asc", "asc", "dabc"},
		{"?order=desc", "desc", "cbad"}, // equal timestamps tie-break on id, mirrored
	}
	var stats []string
	for _, c := range cases {
		fake := timelineOrderFixture()
		w, got := getTimeline(t, fake, c.query)
		if w.Code != http.StatusOK {
			t.Fatalf("%q: status = %d, body = %s", c.query, w.Code, w.Body.String())
		}
		if string(got.Order) != c.wantOrder || string(fake.gotTimelineOpts.Order) != c.wantOrder {
			t.Errorf("%q: echoed order = %q, reader got %q, want %q", c.query, got.Order, fake.gotTimelineOpts.Order, c.wantOrder)
		}
		if ids := timelineIDs(got); ids != c.wantIDs {
			t.Errorf("%q: ids = %s, want %s", c.query, ids, c.wantIDs)
		}
		stats = append(stats, fmt.Sprint(got.OwnerKeyID, got.TotalEvents, got.ExcludedForeignEvents))
	}
	if stats[0] != stats[1] || stats[1] != stats[2] {
		t.Errorf("stats differ across orders: %v", stats)
	}
}

func TestAnalytics_SessionTimeline_InvalidOrder400(t *testing.T) {
	for _, q := range []string{"?order=bogus", "?order=ASC", "?order=up"} {
		w, _ := getTimeline(t, timelineOrderFixture(), q)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%q: status = %d, want 400", q, w.Code)
		}
		assertErrorType(t, w, "validation_error")
	}
}

func TestAnalytics_SessionTimeline_ErrorFromReader(t *testing.T) {
	fake := &fakeAnalyticsReader{timelineErr: errors.New("boom")}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/sessions/sess-1/timeline", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/sessions/{session_id}/timeline", h.SessionTimeline, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", w.Code, w.Body.String())
	}
}

// rawField digs field path[0][path[1]]... out of a raw JSON body, for
// asserting exact wire-format key casing without depending on a Go
// struct's own json tags to decode it back.
func rawField(t *testing.T, body []byte, path ...string) (json.RawMessage, bool) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode top-level object: %v", err)
	}
	var raw json.RawMessage
	for i, key := range path {
		v, ok := m[key]
		if !ok {
			return nil, false
		}
		if i == len(path)-1 {
			raw = v
			break
		}
		if err := json.Unmarshal(v, &m); err != nil {
			t.Fatalf("decode nested object at %q: %v", key, err)
		}
	}
	return raw, true
}

func TestAnalytics_TrafficFlow_NilReader404(t *testing.T) {
	h := Analytics{Deps: newAnalyticsTestDeps(nil)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/traffic-flow", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/traffic-flow", h.TrafficFlow, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "not_found")
}

func TestAnalytics_TrafficFlow_ParamsAndBody(t *testing.T) {
	fake := &fakeAnalyticsReader{flow: &analytics.TrafficFlow{
		Sankey: analytics.Sankey{
			Range: analytics.Range24h, Metric: analytics.SankeyMetricCalls, Total: 3,
			Nodes: []analytics.SankeyNode{
				{ID: "CLIENT:cursor", Key: "cursor", Label: "Cursor", Type: "CLIENT", Value: 3},
				{ID: "PATH:llm", Key: "llm", Label: "LLM calls", Type: "PATH", Value: 3},
				{ID: "MODEL:m", Key: "m", Label: "m", Type: "MODEL", Value: 3, Sublabel: "anthropic"},
			},
			Links: []analytics.SankeyLink{},
		},
		Agents: []analytics.SankeyAgent{{Key: "cursor", Label: "Cursor", Value: 3}},
	}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/traffic-flow?range=24h&limit=3&client_name=cursor", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/traffic-flow", h.TrafficFlow, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fake.gotSankeyQ.Range != analytics.Range24h || fake.gotSankeyQ.Metric != analytics.SankeyMetricCalls ||
		fake.gotSankeyQ.Limit != 3 || fake.gotSankeyQ.ClientName != "cursor" {
		t.Errorf("query = %+v, want range=24h metric=calls limit=3 client_name=cursor", fake.gotSankeyQ)
	}
	if span := fake.gotSankeyQ.Period.End.Sub(fake.gotSankeyQ.Period.Start); span != 24*time.Hour {
		t.Errorf("period spans %v, want 24h", span)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"sublabel":"anthropic"`) {
		t.Errorf("body should carry the MODEL node's provider sublabel: %s", body)
	}
	if strings.Contains(body, `"sublabel":""`) {
		t.Errorf("nodes without a sublabel must omit the field entirely: %s", body)
	}
	if !strings.Contains(body, `"agents":[{"key":"cursor","label":"Cursor","value":3}]`) {
		t.Errorf("body should carry the agents list at the top level: %s", body)
	}
}

func TestAnalytics_TrafficFlow_CustomDates(t *testing.T) {
	fake := &fakeAnalyticsReader{flow: &analytics.TrafficFlow{}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/traffic-flow?from=2026-01-01&to=2026-01-01", nil), "tenant-a", "agent")
	if w := serve(http.MethodGet, "/analytics/traffic-flow", h.TrafficFlow, req); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	q := fake.gotSankeyQ
	if q.Range != analytics.RangeCustom || !q.Period.Start.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) || q.Period.End.Sub(q.Period.Start) != 24*time.Hour {
		t.Errorf("query = %+v, want custom 2026-01-01 (one day)", q)
	}
}

func TestAnalytics_TrafficFlow_InvalidParams400(t *testing.T) {
	fake := &fakeAnalyticsReader{flow: &analytics.TrafficFlow{}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	for _, qs := range []string{"range=3w", "metric=bytes", "limit=0", "limit=x", "from=2026-01-02&to=2026-01-01"} {
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/traffic-flow?"+qs, nil), "tenant-a", "agent")
		w := serve(http.MethodGet, "/analytics/traffic-flow", h.TrafficFlow, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400, body = %s", qs, w.Code, w.Body.String())
		}
		assertErrorType(t, w, "validation_error")
	}
}

func TestAnalytics_TrafficFlow_ResolvesConnectorLabels(t *testing.T) {
	deps := newAnalyticsTestDeps(nil)
	connID := createTestConnector(t, deps, "tenant-a")

	fake := &fakeAnalyticsReader{flow: &analytics.TrafficFlow{Sankey: analytics.Sankey{
		Range: analytics.Range7d, Metric: analytics.SankeyMetricCalls, Total: 13,
		Nodes: []analytics.SankeyNode{
			{ID: "CLIENT:cursor", Key: "cursor", Label: "Cursor", Type: "CLIENT", Value: 13},
			{ID: "PATH:mcp", Key: "mcp", Label: "MCP calls", Type: "PATH", Value: 13},
			{ID: "CONNECTOR:" + connID, Key: connID, Label: connID, Type: "CONNECTOR", Value: 10},
			{ID: "CONNECTOR:deleted-connector-id", Key: "deleted-connector-id", Label: "deleted-connector-id", Type: "CONNECTOR", Value: 2},
			{ID: "CONNECTOR:gateway", Key: "gateway", Label: "Gateway tools", Type: "CONNECTOR", Value: 1},
		},
		Links: []analytics.SankeyLink{},
	}}}
	deps.Analytics = fake
	h := Analytics{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/traffic-flow", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/traffic-flow", h.TrafficFlow, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got analytics.TrafficFlow
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]analytics.SankeyNode{}
	for _, n := range got.Nodes {
		byID[n.ID] = n
	}
	if n := byID["CONNECTOR:"+connID]; n.Label != "Okta MCP!" || n.Key != connID {
		t.Errorf("connector node = %+v, want label resolved to the display name with key kept as the id", n)
	}
	if n := byID["CONNECTOR:deleted-connector-id"]; n.Label != "deleted-connector-id" {
		t.Errorf("deleted connector node = %+v, want the raw id kept as the label", n)
	}
	if n := byID["CONNECTOR:gateway"]; n.Label != "Gateway tools" {
		t.Errorf("gateway node = %+v, want its label untouched", n)
	}
}

// tenantLLMCalls is an analytics.LLMCallReader test double standing in for
// the Postgres capture store: rows are keyed by tenant, so a handler that
// passed the wrong tenant would read another tenant's calls.
type tenantLLMCalls struct {
	rows  map[string][]sink.LLMCall
	calls int
	gotF  analytics.LLMCallFilter
}

func (f *tenantLLMCalls) ListLLMCalls(_ context.Context, tenantID string, filter analytics.LLMCallFilter) ([]sink.LLMCall, int, error) {
	f.calls++
	f.gotF = filter
	return f.rows[tenantID], len(f.rows[tenantID]), nil
}

func (f *tenantLLMCalls) GetLLMCall(_ context.Context, tenantID, requestID string) (*sink.LLMCall, error) {
	f.calls++
	for _, c := range f.rows[tenantID] {
		if c.RequestID == requestID {
			return &c, nil
		}
	}
	return nil, analytics.ErrNotFound
}

func newPGLLMCalls() *tenantLLMCalls {
	return &tenantLLMCalls{rows: map[string][]sink.LLMCall{
		"tenant-a": {{RequestID: "a1", TenantID: "tenant-a", Model: "m", RequestBody: []byte(`{"q":1}`)}},
		"tenant-b": {{RequestID: "b1", TenantID: "tenant-b", Model: "m"}},
	}}
}

// Without ClickHouse, LLM Logs list and detail come from Deps.LLMCalls (the
// Postgres capture store), scoped to the caller's tenant.
func TestAnalytics_LLMLogs_FallsBackToLLMCalls(t *testing.T) {
	pg := newPGLLMCalls()
	d := newAnalyticsTestDeps(nil)
	d.LLMCalls = pg
	h := Analytics{Deps: d}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/llm-logs?limit=5&client_name=claude-code", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/llm-logs", h.LLMLogs, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var page struct {
		Items []sink.LLMCall `json:"items"`
		Total int            `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].RequestID != "a1" {
		t.Errorf("page = %+v, want tenant-a's a1 only", page)
	}
	if pg.gotF.Limit != 5 || pg.gotF.ClientName != "claude-code" {
		t.Errorf("filter = %+v", pg.gotF)
	}

	get := func(tenant, id string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/llm-logs/"+id, nil), tenant, "admin")
		return serve(http.MethodGet, "/analytics/llm-logs/{request_id}", h.LLMLogsGet, req)
	}
	w = get("tenant-a", "a1")
	if w.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body = %s", w.Code, w.Body.String())
	}
	var rec sink.LLMCall
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.RequestID != "a1" || string(rec.RequestBody) != `{"q":1}` {
		t.Errorf("detail = %+v", rec)
	}
	// Another tenant's call is invisible: 404, not its row.
	if w := get("tenant-a", "b1"); w.Code != http.StatusNotFound {
		t.Errorf("cross-tenant detail status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
}

// With a ClickHouse reader configured it stays the LLM Logs source; the
// Postgres reader is not consulted.
func TestAnalytics_LLMLogs_ClickHouseWinsOverLLMCalls(t *testing.T) {
	pg := newPGLLMCalls()
	ch := &fakeAnalyticsReader{llmItems: []sink.LLMCall{{RequestID: "ch1"}}, llmTotal: 1, llmOne: &sink.LLMCall{RequestID: "ch1"}}
	d := newAnalyticsTestDeps(ch)
	d.LLMCalls = pg
	h := Analytics{Deps: d}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/llm-logs", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/llm-logs", h.LLMLogs, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ch1"`) {
		t.Fatalf("list status = %d, body = %s", w.Code, w.Body.String())
	}
	req = withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/llm-logs/ch1", nil), "tenant-a", "admin")
	w = serve(http.MethodGet, "/analytics/llm-logs/{request_id}", h.LLMLogsGet, req)
	if w.Code != http.StatusOK || ch.gotLLMID != "ch1" {
		t.Fatalf("detail status = %d, id = %q, body = %s", w.Code, ch.gotLLMID, w.Body.String())
	}
	if pg.calls != 0 {
		t.Errorf("postgres reader called %d times with ClickHouse configured", pg.calls)
	}
}

// With neither source LLM Logs answers 404 as before, and the Postgres
// reader alone never lights up the other analytics routes.
func TestAnalytics_LLMLogs_NoSource404(t *testing.T) {
	h := Analytics{Deps: newAnalyticsTestDeps(nil)}
	for _, tc := range []struct {
		pattern, url string
		handler      http.HandlerFunc
	}{
		{"/analytics/llm-logs", "/analytics/llm-logs", h.LLMLogs},
		{"/analytics/llm-logs/{request_id}", "/analytics/llm-logs/a1", h.LLMLogsGet},
	} {
		req := withPrincipal(httptest.NewRequest(http.MethodGet, tc.url, nil), "tenant-a", "admin")
		w := serve(http.MethodGet, tc.pattern, tc.handler, req)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), analyticsUnavailableMessage) {
			t.Errorf("%s: status = %d, body = %s", tc.url, w.Code, w.Body.String())
		}
	}

	d := newAnalyticsTestDeps(nil)
	d.LLMCalls = newPGLLMCalls()
	h = Analytics{Deps: d}
	for _, tc := range []struct {
		pattern, url string
		handler      http.HandlerFunc
	}{
		{"/analytics/overview", "/analytics/overview", h.Overview},
		{"/analytics/logs", "/analytics/logs", h.Logs},
		{"/analytics/logs/{request_id}", "/analytics/logs/x", h.LogsGet},
		{"/analytics/models", "/analytics/models", h.Models},
		{"/analytics/skills", "/analytics/skills", h.Skills},
		{"/analytics/client-models", "/analytics/client-models", h.ClientModelSankey},
		{"/analytics/sessions/{session_id}/timeline", "/analytics/sessions/s1/timeline", h.SessionTimeline},
	} {
		req := withPrincipal(httptest.NewRequest(http.MethodGet, tc.url, nil), "tenant-a", "admin")
		w := serve(http.MethodGet, tc.pattern, tc.handler, req)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), analyticsUnavailableMessage) {
			t.Errorf("%s with only the Postgres reader: status = %d, body = %s", tc.url, w.Code, w.Body.String())
		}
	}
}
