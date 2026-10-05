package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// fakeMonReader is an analytics.Reader that also serves token monitoring.
type fakeMonReader struct {
	*fakeAnalyticsReader
	mon      *analytics.TokenMonitoring
	gotQuery analytics.TokenMonitoringQuery
}

func (f *fakeMonReader) TokenMonitoring(_ context.Context, _ string, q analytics.TokenMonitoringQuery) (*analytics.TokenMonitoring, error) {
	f.gotQuery = q
	return f.mon, nil
}

func usage(tokens, prev uint64) analytics.TokenUsage {
	return analytics.TokenUsage{Tokens: tokens, Calls: 1, PrevTokens: prev}
}

// monDeps seeds tenant-a's keys: one per role, plus another tenant's key.
func monDeps(t *testing.T, r *fakeMonReader) (Deps, map[string]string) {
	t.Helper()
	d := newTestDeps()
	d.Analytics = r
	ids := map[string]string{}
	for _, k := range []struct{ tenant, name, role string }{
		{"tenant-a", "ci-bot", "agent"},
		{"tenant-a", "ops", "admin"},
		{"tenant-a", "root", "platform-admin"},
		{"tenant-a", "collector", "interceptor"},
		{"tenant-a", "reader", "viewer"},
		{"tenant-b", "elsewhere", "admin"},
	} {
		key := &store.APIKey{TenantID: k.tenant, Name: k.name, Role: k.role, KeyHash: k.name}
		if err := d.Store.APIKeys().Create(context.Background(), key); err != nil {
			t.Fatal(err)
		}
		ids[k.name] = key.ID
	}
	return d, ids
}

func TestTokenMonitoring_Unavailable404(t *testing.T) {
	for name, d := range map[string]Deps{
		"no reader":        newAnalyticsTestDeps(nil),
		"reader lacks mon": newAnalyticsTestDeps(&fakeAnalyticsReader{}),
	} {
		h := Analytics{Deps: d}
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/token-monitoring", nil), "tenant-a", "viewer")
		if w := serve(http.MethodGet, "/analytics/token-monitoring", h.TokenMonitoring, req); w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, w.Code)
		}
	}
}

func TestTokenMonitoring_WindowValidation(t *testing.T) {
	r := &fakeMonReader{fakeAnalyticsReader: &fakeAnalyticsReader{}, mon: &analytics.TokenMonitoring{}}
	d, _ := monDeps(t, r)
	h := Analytics{Deps: d}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/token-monitoring?window=24h", nil), "tenant-a", "viewer")
	if w := serve(http.MethodGet, "/analytics/token-monitoring", h.TokenMonitoring, req); w.Code != http.StatusBadRequest {
		t.Errorf("window=24h: status %d, want 400", w.Code)
	}
	req = withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/token-monitoring", nil), "tenant-a", "viewer")
	if w := serve(http.MethodGet, "/analytics/token-monitoring", h.TokenMonitoring, req); w.Code != http.StatusOK || r.gotQuery.Window != analytics.WindowToday {
		t.Errorf("no window: status %d window %q, want 200 today", w.Code, r.gotQuery.Window)
	}
}

func TestTokenMonitoring_EnrichesCallersAndRoles(t *testing.T) {
	r := &fakeMonReader{fakeAnalyticsReader: &fakeAnalyticsReader{}}
	d, ids := monDeps(t, r)
	r.mon = &analytics.TokenMonitoring{
		Window: analytics.Window7d,
		Totals: analytics.TokenUsage{Tokens: 1100, PrevTokens: 1000},
		ByModel: []analytics.ModelTokenUsage{
			{Model: "claude-sonnet-4-5", TokenUsage: usage(1000, 500)},
			{Model: "gpt-5", TokenUsage: usage(100, 0)},
		},
		ByKey: []analytics.KeyTokenUsage{
			{KeyID: ids["ci-bot"], Source: "gateway", TokenUsage: usage(500, 250)},
			{KeyID: ids["ops"], Source: "gateway", TokenUsage: usage(200, 0)},
			{KeyID: ids["root"], Source: "gateway", TokenUsage: usage(100, 0)},
			{KeyID: ids["collector"], Source: "interceptor", TokenUsage: usage(150, 0)},
			// An agent key whose rows were ingested is interceptor traffic.
			{KeyID: ids["ci-bot"] + "-ingest", Source: "interceptor", TokenUsage: usage(50, 0)},
			{KeyID: ids["reader"], Source: "gateway", TokenUsage: usage(40, 0)},
			{KeyID: ids["elsewhere"], Source: "gateway", TokenUsage: usage(30, 0)}, // other tenant's key: unknown here
			{KeyID: "", Source: "gateway", TokenUsage: usage(30, 0)},
		},
	}
	h := Analytics{Deps: d}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/token-monitoring?window=7d", nil), "tenant-a", "viewer")
	w := serve(http.MethodGet, "/analytics/token-monitoring", h.TokenMonitoring, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if r.gotQuery.Window != analytics.Window7d {
		t.Errorf("window = %q, want 7d", r.gotQuery.Window)
	}
	var got tokenMonitoringView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.Body.String(), `"sessions`) {
		t.Errorf("overview body lists sessions; they belong to the drill-downs: %s", w.Body.String())
	}
	if got.Totals.DeltaPct == nil || *got.Totals.DeltaPct != 10 {
		t.Errorf("totals delta = %v, want 10", got.Totals.DeltaPct)
	}
	if got.ByModel[0].DeltaPct == nil || *got.ByModel[0].DeltaPct != 100 || got.ByModel[1].DeltaPct != nil {
		t.Errorf("model deltas = %v / %v, want 100 / nil (new)", got.ByModel[0].DeltaPct, got.ByModel[1].DeltaPct)
	}
	want := []struct{ name, role string }{
		{"ci-bot", "agent"}, {"ops", "admin"}, {"root", "admin"}, {"collector", "interceptor"},
		{"", "interceptor"}, {"reader", "other"}, {"", "other"}, {"", "other"},
	}
	if len(got.ByKey) != len(want) {
		t.Fatalf("by_key = %+v", got.ByKey)
	}
	for i, w := range want {
		if got.ByKey[i].Name != w.name || got.ByKey[i].Role != w.role {
			t.Errorf("by_key[%d] = %s/%s, want %s/%s", i, got.ByKey[i].Name, got.ByKey[i].Role, w.name, w.role)
		}
	}
	if d := got.ByKey[0].DeltaPct; d == nil || *d != 100 {
		t.Errorf("by_key[0] delta = %v, want 100", d)
	}
	wantRoles := []roleUsage{{"agent", 500, 1}, {"admin", 300, 2}, {"interceptor", 200, 2}, {"other", 100, 3}}
	if len(got.ByRole) != len(wantRoles) {
		t.Fatalf("by_role = %+v, want %+v", got.ByRole, wantRoles)
	}
	for i, w := range wantRoles {
		if got.ByRole[i] != w {
			t.Errorf("by_role[%d] = %+v, want %+v", i, got.ByRole[i], w)
		}
	}
}

func TestTokenMonitoringModel(t *testing.T) {
	r := &fakeMonReader{fakeAnalyticsReader: &fakeAnalyticsReader{}}
	d, ids := monDeps(t, r)
	r.mon = &analytics.TokenMonitoring{
		Totals:   usage(600, 0),
		ByKey:    []analytics.KeyTokenUsage{{KeyID: ids["ci-bot"], Source: "gateway", TokenUsage: usage(600, 0)}},
		Sessions: []analytics.SessionTokenUsage{{SessionID: "s1", KeyID: ids["ci-bot"], TokenUsage: usage(600, 0)}},
	}
	h := Analytics{Deps: d}
	path := "/analytics/token-monitoring/model"

	req := withPrincipal(httptest.NewRequest(http.MethodGet, path+"?window=today", nil), "tenant-a", "viewer")
	if w := serve(http.MethodGet, path, h.TokenMonitoringModel, req); w.Code != http.StatusBadRequest {
		t.Errorf("missing model: status %d, want 400", w.Code)
	}

	req = withPrincipal(httptest.NewRequest(http.MethodGet, path+"?model=bedrock%2Fus.anthropic.claude%3A0&window=30d&limit=5&offset=10", nil), "tenant-a", "viewer")
	w := serve(http.MethodGet, path, h.TokenMonitoringModel, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if q := r.gotQuery; q.Model != "bedrock/us.anthropic.claude:0" || q.KeyID != "" || q.Window != analytics.Window30d || q.Limit != 5 || q.Offset != 10 {
		t.Errorf("query = %+v", q)
	}
	var got tokenMonitoringView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.Body.String(), `"by_role"`) {
		t.Errorf("model drill-down has by_role; the role split is the overview's: %s", w.Body.String())
	}
	if got.Model != "bedrock/us.anthropic.claude:0" || got.ByKey[0].Name != "ci-bot" || got.ByKey[0].Role != "agent" ||
		got.Sessions[0].KeyName != "ci-bot" || got.Sessions[0].Role != "agent" {
		t.Errorf("detail = %+v", got)
	}
}

func TestTokenMonitoringKey(t *testing.T) {
	r := &fakeMonReader{fakeAnalyticsReader: &fakeAnalyticsReader{}}
	d, ids := monDeps(t, r)
	r.mon = &analytics.TokenMonitoring{
		Totals:  usage(100, 0),
		ByModel: []analytics.ModelTokenUsage{{Model: "gpt-5", TokenUsage: usage(100, 0)}},
		ByKey:   []analytics.KeyTokenUsage{},
		Sessions: []analytics.SessionTokenUsage{{SessionID: "s9", KeyID: ids["collector"], TokenUsage: usage(100, 0),
			LastSeen: time.Unix(0, 0).UTC()}},
	}
	h := Analytics{Deps: d}
	pattern := "/analytics/token-monitoring/keys/{id}"
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/token-monitoring/keys/"+ids["collector"]+"?window=7d", nil), "tenant-a", "viewer")
	w := serve(http.MethodGet, pattern, h.TokenMonitoringKey, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if r.gotQuery.KeyID != ids["collector"] || r.gotQuery.Model != "" || r.gotQuery.Window != analytics.Window7d {
		t.Errorf("query = %+v", r.gotQuery)
	}
	var got tokenMonitoringView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Key == nil || got.Key.ID != ids["collector"] || got.Key.Name != "collector" || got.Key.Role != "interceptor" {
		t.Errorf("key = %+v, want collector/interceptor", got.Key)
	}

	// Another tenant's key id is just unknown here: empty name, role other.
	req = withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/token-monitoring/keys/"+ids["elsewhere"], nil), "tenant-a", "viewer")
	w = serve(http.MethodGet, pattern, h.TokenMonitoringKey, req)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Key.Name != "" || got.Key.Role != "other" {
		t.Errorf("other tenant's key = %+v (%v), want unnamed/other", got.Key, err)
	}
}
