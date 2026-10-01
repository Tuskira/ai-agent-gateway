package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func TestAnalytics_SkillsUsage(t *testing.T) {
	seen := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	fake := &fakeAnalyticsReader{skillUsage: []analytics.SkillUsage{
		{Name: "review-pr", Calls: 9, UsedBy: 2, LastSeen: seen},
		{Name: "summarize", Calls: 4, UsedBy: 1, LastSeen: seen},
		{Name: "mystery", Calls: 1, UsedBy: 1, LastSeen: seen},
	}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	ctx := context.Background()
	for _, sk := range []*store.Skill{
		{TenantID: "tenant-a", Name: "Review-PR", Kind: "skill", Description: "d"},
		{TenantID: "tenant-a", Name: "summarize", Kind: "command", Description: "d"},
	} {
		if err := h.Store.Skills().Create(ctx, sk, []store.SkillFile{{Path: "SKILL.md", Content: "---\nname: " + sk.Name + "\ndescription: d\n---\nB"}}, "test"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/skills/usage?range=30d", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/skills/usage", h.SkillsUsage, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if fake.gotUsageRange != analytics.Range30d {
		t.Errorf("range = %q, want 30d", fake.gotUsageRange)
	}
	var got analytics.DiscoveredSkills
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Range != analytics.Range30d || len(got.Skills) != 3 {
		t.Fatalf("got %+v", got)
	}
	want := []struct {
		name, kind string
		reg        bool
	}{{"review-pr", "skill", true}, {"summarize", "command", true}, {"mystery", "", false}}
	for i, w := range want {
		s := got.Skills[i]
		if s.Name != w.name || s.Kind != w.kind || s.Registered != w.reg {
			t.Errorf("row %d = %+v, want %+v", i, s, w)
		}
	}
	if got.Skills[0].Calls != 9 || got.Skills[0].UsedBy != 2 || !got.Skills[0].LastSeen.Equal(seen) {
		t.Errorf("row 0 stats = %+v", got.Skills[0])
	}
}

func TestAnalytics_SkillsUsage_TenantIsolation(t *testing.T) {
	fake := &fakeAnalyticsReader{skillUsage: []analytics.SkillUsage{{Name: "theirs", Calls: 1}}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	if err := h.Store.Skills().Create(context.Background(), &store.Skill{TenantID: "tenant-b", Name: "theirs", Kind: "skill", Description: "d"},
		[]store.SkillFile{{Path: "SKILL.md", Content: "---\nname: theirs\ndescription: d\n---\nB"}}, "test"); err != nil {
		t.Fatal(err)
	}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/skills/usage", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/skills/usage", h.SkillsUsage, req)
	var got analytics.DiscoveredSkills
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if fake.gotUsageRange != analytics.Range7d || got.Skills[0].Registered {
		t.Fatalf("range=%q row=%+v: another tenant's skill must not register", fake.gotUsageRange, got.Skills[0])
	}
}

func TestAnalytics_Usage_ErrorsAndRange(t *testing.T) {
	for _, tc := range []struct {
		path string
		fn   func(Analytics) http.HandlerFunc
	}{
		{"/analytics/skills/usage", func(h Analytics) http.HandlerFunc { return h.SkillsUsage }},
		{"/analytics/mcps/usage", func(h Analytics) http.HandlerFunc { return h.MCPsUsage }},
	} {
		h := Analytics{Deps: newAnalyticsTestDeps(nil)}
		w := serve(http.MethodGet, tc.path, tc.fn(h), withPrincipal(httptest.NewRequest(http.MethodGet, tc.path, nil), "tenant-a", "agent"))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s nil reader: %d", tc.path, w.Code)
		}
		h = Analytics{Deps: newAnalyticsTestDeps(&fakeAnalyticsReader{})}
		w = serve(http.MethodGet, tc.path, tc.fn(h), withPrincipal(httptest.NewRequest(http.MethodGet, tc.path+"?range=3w", nil), "tenant-a", "agent"))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s bad range: %d", tc.path, w.Code)
		}
		h = Analytics{Deps: newAnalyticsTestDeps(&fakeAnalyticsReader{usageErr: errors.New("boom")})}
		w = serve(http.MethodGet, tc.path, tc.fn(h), withPrincipal(httptest.NewRequest(http.MethodGet, tc.path, nil), "tenant-a", "agent"))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s reader error: %d", tc.path, w.Code)
		}
	}
}

func TestAnalytics_MCPsUsage(t *testing.T) {
	seen := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	later := seen.Add(time.Hour)
	fake := &fakeAnalyticsReader{mcpUsage: []analytics.MCPToolUsage{
		// A registered connector used directly (name matches case-insensitively).
		{Server: "github", Tool: "create_issue", Calls: 3, Keys: []string{"k1"}, LastSeen: seen},
		{Server: "github", Tool: "list_prs", Calls: 2, Keys: []string{"k1", "k2"}, LastSeen: later},
		// Not registered anywhere: discovered.
		{Server: "figma", Tool: "get_file", Calls: 1, Keys: []string{"k3"}, LastSeen: seen},
		// Through the gateway's MCP plane under the client's alias "gw".
		{Server: "gw", Tool: "langfuse__get_trace", Calls: 10, Keys: []string{"k1"}, LastSeen: later},
		{Server: "gw", Tool: "langfuse__list_traces", Calls: 4, Keys: []string{"k2"}, LastSeen: seen},
		// Gateway alias but the connector is not registered: stays "gw".
		{Server: "gw", Tool: "unknownconn__x", Calls: 1, Keys: []string{"k1"}, LastSeen: seen},
	}, mcpCalls: []analytics.MCPServerCalls{
		// Distinct LLM calls, not the per-tool sum: 10+4 tool uses came from 11
		// calls (3 used both tools); 3+2 from 4 calls (1 used both).
		{Server: "langfuse", Via: true, Calls: 11},
		{Server: "github", Calls: 4},
		{Server: "figma", Calls: 1},
		{Server: "gw", Calls: 1},
	}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	for _, c := range []*store.Connector{
		{TenantID: "tenant-a", Name: "GitHub", Slug: "gh-slug", Endpoint: "https://gh.example/mcp"},
		{TenantID: "tenant-a", Name: "Langfuse", Slug: "langfuse", Endpoint: "https://lf.example/mcp"},
	} {
		if err := h.Store.Connectors().Create(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/mcps/usage?range=24h", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/mcps/usage", h.MCPsUsage, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if fake.gotUsageRange != analytics.Range24h {
		t.Errorf("range = %q", fake.gotUsageRange)
	}
	var got analytics.DiscoveredMCPServers
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	by := map[string]analytics.DiscoveredMCPServer{}
	for _, s := range got.Servers {
		by[s.Server] = s
	}
	if len(got.Servers) != 4 {
		t.Fatalf("servers = %+v", got.Servers)
	}
	// Sorted by calls desc: langfuse(11), github(4), then the 1-call ties by name.
	if got.Servers[0].Server != "langfuse" || got.Servers[1].Server != "github" {
		t.Errorf("order = %s, %s", got.Servers[0].Server, got.Servers[1].Server)
	}
	lf := by["langfuse"]
	if !lf.ViaGateway || lf.RegisteredConnectorSlug != "langfuse" || lf.Calls != 11 || lf.UsedBy != 2 ||
		len(lf.Tools) != 2 || lf.Tools[0] != "get_trace" || !lf.LastSeen.Equal(later) {
		t.Errorf("langfuse via gateway = %+v", lf)
	}
	gh := by["github"]
	if gh.ViaGateway || gh.RegisteredConnectorSlug != "gh-slug" || gh.Calls != 4 || gh.UsedBy != 2 || len(gh.Tools) != 2 {
		t.Errorf("github = %+v", gh)
	}
	if f := by["figma"]; f.RegisteredConnectorSlug != "" || f.ViaGateway || f.Calls != 1 {
		t.Errorf("figma (discovered) = %+v", f)
	}
	if g := by["gw"]; g.RegisteredConnectorSlug != "" || g.ViaGateway || g.Tools[0] != "unknownconn__x" {
		t.Errorf("gw unregistered = %+v", g)
	}
}

func TestAnalytics_MCPsUsage_OtherTenantConnectorDoesNotRegister(t *testing.T) {
	fake := &fakeAnalyticsReader{mcpUsage: []analytics.MCPToolUsage{{Server: "slack", Tool: "post", Calls: 1, Keys: []string{"k"}}}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	if err := h.Store.Connectors().Create(context.Background(), &store.Connector{TenantID: "tenant-b", Name: "Slack", Slug: "slack", Endpoint: "https://s.example/mcp"}); err != nil {
		t.Fatal(err)
	}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/mcps/usage", nil), "tenant-a", "agent")
	w := serve(http.MethodGet, "/analytics/mcps/usage", h.MCPsUsage, req)
	var got analytics.DiscoveredMCPServers
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if fake.gotUsageRange != analytics.Range7d || len(got.Servers) != 1 || got.Servers[0].RegisteredConnectorSlug != "" {
		t.Fatalf("got %+v", got)
	}
}

// Without a distinct-call count for a row the fold falls back to the busiest
// tool (a lower bound), never the per-tool sum that double counts.
func TestFoldMCPUsage_FallbackNeverSumsTools(t *testing.T) {
	usage := []analytics.MCPToolUsage{
		{Server: "figma", Tool: "a", Calls: 5},
		{Server: "figma", Tool: "b", Calls: 3},
	}
	got := foldMCPUsage(usage, nil, map[string]string{})
	if len(got) != 1 || got[0].Calls != 5 {
		t.Fatalf("got %+v, want one row with calls=5", got)
	}
	got = foldMCPUsage(usage, []analytics.MCPServerCalls{{Server: "figma", Calls: 6}}, nil)
	if got[0].Calls != 6 {
		t.Fatalf("distinct count not used: %+v", got)
	}
}

func TestAnalytics_MCPsUsage_PassesAliasesToReader(t *testing.T) {
	fake := &fakeAnalyticsReader{mcpUsage: []analytics.MCPToolUsage{{Server: "gw", Tool: "langfuse__x", Calls: 1}}}
	h := Analytics{Deps: newAnalyticsTestDeps(fake)}
	if err := h.Store.Connectors().Create(context.Background(), &store.Connector{TenantID: "tenant-a", Name: "Langfuse EU", Slug: "langfuse", Endpoint: "https://lf.example/mcp"}); err != nil {
		t.Fatal(err)
	}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/analytics/mcps/usage", nil), "tenant-a", "agent")
	serve(http.MethodGet, "/analytics/mcps/usage", h.MCPsUsage, req)
	if fake.gotAliases["langfuse eu"] != "langfuse" || fake.gotAliases["langfuse"] != "langfuse" {
		t.Errorf("aliases = %v", fake.gotAliases)
	}
}
