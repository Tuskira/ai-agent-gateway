package clickhouse

import (
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
)

func flowNodesByID(t *testing.T, got *analytics.TrafficFlow) map[string]analytics.SankeyNode {
	t.Helper()
	byID := map[string]analytics.SankeyNode{}
	for _, n := range got.Nodes {
		if _, dup := byID[n.ID]; dup {
			t.Errorf("duplicate node id %q", n.ID)
		}
		byID[n.ID] = n
	}
	return byID
}

func flowLink(got *analytics.TrafficFlow, sourceType, source, targetType, target string) (analytics.SankeyLink, bool) {
	for _, l := range got.Links {
		if l.SourceType == sourceType && l.Source == source && l.TargetType == targetType && l.Target == target {
			return l, true
		}
	}
	return analytics.SankeyLink{}, false
}

func TestBuildTrafficFlow_Empty(t *testing.T) {
	got := buildTrafficFlow(nil, nil, 10, analytics.SankeyMetricCalls, "")
	if got.Total != 0 || len(got.Nodes) != 0 || len(got.Links) != 0 {
		t.Fatalf("empty rows: got %+v, want zero-value graph", got)
	}
	if got.Nodes == nil || got.Links == nil {
		t.Error("Nodes/Links must be empty slices, not nil (so they marshal as [] not null)")
	}
}

func TestBuildTrafficFlow_BothBranches(t *testing.T) {
	llm := []sankeyRow{
		{Client: "claude-code", Model: "claude-sonnet-5", Provider: "anthropic", Calls: 240, Tokens: 2400, CostUSD: 2.4},
		{Client: "claude-code", Model: "claude-haiku-4-5", Provider: "bedrock", Calls: 60, Tokens: 600, CostUSD: 0.6},
		{Client: "cursor", Model: "claude-sonnet-5", Provider: "anthropic", Calls: 100, Tokens: 1000, CostUSD: 1},
	}
	mcp := []mcpFlowRow{
		{Client: "claude-code", Connector: "mesh", Calls: 118},
		{Client: "cursor", Connector: "mesh", Calls: 20},
		{Client: "cursor", Connector: "vulners", Calls: 30},
	}
	got := buildTrafficFlow(llm, mcp, 10, analytics.SankeyMetricCalls, "")

	if got.Total != 568 {
		t.Errorf("Total = %v, want 568 (400 LLM + 168 MCP)", got.Total)
	}
	byID := flowNodesByID(t, got)

	// CLIENT: sum of both planes.
	if n := byID["CLIENT:claude-code"]; n.Value != 418 || n.Label != "Claude Code" {
		t.Errorf("CLIENT:claude-code = %+v, want value 418", n)
	}
	if n := byID["CLIENT:cursor"]; n.Value != 150 {
		t.Errorf("CLIENT:cursor = %+v, want value 150", n)
	}

	// PATH: one node per plane, in llm-then-mcp order.
	if n := byID["PATH:llm"]; n.Value != 400 || n.Label != analytics.SankeyPathLLMLabel || n.Type != analytics.SankeyNodePath {
		t.Errorf("PATH:llm = %+v", n)
	}
	if n := byID["PATH:mcp"]; n.Value != 168 || n.Label != analytics.SankeyPathMCPLabel {
		t.Errorf("PATH:mcp = %+v", n)
	}

	// MODEL: provider in Sublabel.
	if n := byID["MODEL:claude-sonnet-5"]; n.Value != 340 || n.Sublabel != "anthropic" {
		t.Errorf("MODEL:claude-sonnet-5 = %+v, want value 340 sublabel anthropic", n)
	}
	if n := byID["MODEL:claude-haiku-4-5"]; n.Value != 60 || n.Sublabel != "bedrock" {
		t.Errorf("MODEL:claude-haiku-4-5 = %+v", n)
	}

	// CONNECTOR: no sublabel.
	if n := byID["CONNECTOR:mesh"]; n.Value != 138 || n.Label != "mesh" || n.Sublabel != "" || n.Type != analytics.SankeyNodeConnector {
		t.Errorf("CONNECTOR:mesh = %+v", n)
	}
	if n := byID["CONNECTOR:vulners"]; n.Value != 30 {
		t.Errorf("CONNECTOR:vulners = %+v", n)
	}

	// Links: CLIENT->PATH, PATH->MODEL, PATH->CONNECTOR.
	if l, ok := flowLink(got, "CLIENT", "claude-code", "PATH", "llm"); !ok || l.Value != 300 {
		t.Errorf("claude-code->llm = %+v ok=%v, want 300", l, ok)
	}
	if l, ok := flowLink(got, "CLIENT", "claude-code", "PATH", "mcp"); !ok || l.Value != 118 {
		t.Errorf("claude-code->mcp = %+v ok=%v, want 118", l, ok)
	}
	if l, ok := flowLink(got, "PATH", "llm", "MODEL", "claude-sonnet-5"); !ok || l.Value != 340 {
		t.Errorf("llm->claude-sonnet-5 = %+v ok=%v, want 340", l, ok)
	}
	if l, ok := flowLink(got, "PATH", "mcp", "CONNECTOR", "mesh"); !ok || l.Value != 138 {
		t.Errorf("mcp->mesh = %+v ok=%v, want 138", l, ok)
	}
	if len(got.Links) != 4+2+2 {
		t.Errorf("Links = %d, want 8 (4 client->path, 2 path->model, 2 path->connector)", len(got.Links))
	}

	// Column order in Nodes: CLIENT, PATH, MODEL, CONNECTOR.
	wantOrder := []string{"CLIENT", "CLIENT", "PATH", "PATH", "MODEL", "MODEL", "CONNECTOR", "CONNECTOR"}
	for i, n := range got.Nodes {
		if i < len(wantOrder) && n.Type != wantOrder[i] {
			t.Errorf("Nodes[%d].Type = %s, want %s", i, n.Type, wantOrder[i])
		}
	}
}

func TestBuildTrafficFlow_TokensDropsMCPBranch(t *testing.T) {
	llm := []sankeyRow{{Client: "cursor", Model: "m", Provider: "anthropic", Calls: 1, Tokens: 500, CostUSD: 0.5}}
	mcp := []mcpFlowRow{{Client: "cursor", Connector: "mesh", Calls: 99}}

	for _, metric := range []analytics.SankeyMetric{analytics.SankeyMetricTokens, analytics.SankeyMetricCost} {
		got := buildTrafficFlow(llm, mcp, 10, metric, "")
		byID := flowNodesByID(t, got)
		if _, ok := byID["PATH:mcp"]; ok {
			t.Errorf("metric %s: PATH:mcp present, MCP branch must be omitted", metric)
		}
		if _, ok := byID["CONNECTOR:mesh"]; ok {
			t.Errorf("metric %s: CONNECTOR:mesh present", metric)
		}
		want := 500.0
		if metric == analytics.SankeyMetricCost {
			want = 0.5
		}
		if got.Total != want || byID["CLIENT:cursor"].Value != want {
			t.Errorf("metric %s: Total=%v client=%v, want %v", metric, got.Total, byID["CLIENT:cursor"].Value, want)
		}
	}
}

func TestBuildTrafficFlow_FoldsEachTerminalColumn(t *testing.T) {
	llm := []sankeyRow{
		{Client: "cursor", Model: "m1", Provider: "anthropic", Calls: 30},
		{Client: "cursor", Model: "m2", Provider: "anthropic", Calls: 20},
		{Client: "cursor", Model: "m3", Provider: "bedrock", Calls: 10},
	}
	mcp := []mcpFlowRow{
		{Client: "cursor", Connector: "c1", Calls: 5},
		{Client: "cursor", Connector: "c2", Calls: 4},
		{Client: "cursor", Connector: "c3", Calls: 3},
	}
	got := buildTrafficFlow(llm, mcp, 2, analytics.SankeyMetricCalls, "")
	byID := flowNodesByID(t, got)

	if _, ok := byID["MODEL:m3"]; ok {
		t.Error("MODEL:m3 should be folded into other-models")
	}
	if n := byID["MODEL:"+analytics.SankeyOtherModelsKey]; n.Value != 10 || n.Label != analytics.SankeyOtherModelsLabel || n.Sublabel != "" {
		t.Errorf("other-models = %+v", n)
	}
	if _, ok := byID["CONNECTOR:c3"]; ok {
		t.Error("CONNECTOR:c3 should be folded into other-connectors")
	}
	if n := byID["CONNECTOR:"+analytics.SankeyOtherConnectorsKey]; n.Value != 3 || n.Label != analytics.SankeyOtherConnectorsLabel {
		t.Errorf("other-connectors = %+v", n)
	}
	if got.Total != 72 {
		t.Errorf("Total = %v, want 72 (folding never drops traffic)", got.Total)
	}
}

func TestBuildTrafficFlow_UnknownClientAndGatewayTools(t *testing.T) {
	mcp := []mcpFlowRow{
		{Client: "", Connector: "", Calls: 7},
	}
	got := buildTrafficFlow(nil, mcp, 10, analytics.SankeyMetricCalls, "")
	byID := flowNodesByID(t, got)

	if n := byID["CLIENT:"+analytics.SankeyUnknownClientKey]; n.Value != 7 || n.Label != analytics.SankeyUnknownClientLabel {
		t.Errorf("unknown client = %+v", n)
	}
	if n := byID["CONNECTOR:"+analytics.SankeyGatewayToolsKey]; n.Value != 7 || n.Label != analytics.SankeyGatewayToolsLabel {
		t.Errorf("gateway tools = %+v", n)
	}
	if _, ok := byID["PATH:llm"]; ok {
		t.Error("PATH:llm present with no LLM rows -- an empty branch must not produce a node")
	}
}

func TestBuildTrafficFlow_ModelSublabelIsDominantProvider(t *testing.T) {
	llm := []sankeyRow{
		{Client: "cursor", Model: "m", Provider: "bedrock", Calls: 10},
		{Client: "claude-code", Model: "m", Provider: "anthropic", Calls: 90},
	}
	got := buildTrafficFlow(llm, nil, 10, analytics.SankeyMetricCalls, "")
	byID := flowNodesByID(t, got)
	if n := byID["MODEL:m"]; n.Sublabel != "anthropic" {
		t.Errorf("MODEL:m sublabel = %q, want the provider serving most of its traffic (anthropic)", n.Sublabel)
	}
}

func TestMCPClientKey(t *testing.T) {
	cases := map[string]string{
		"":                            analytics.SankeyUnknownClientKey,
		"claude-cli/2.0.0":            "claude-code",
		"Cursor/1.2 (darwin)":         "cursor",
		"vscode-mcp/0.1":              "vscode",
		"codex-cli 0.4":               "codex",
		"Claude/1.0 (Claude Desktop)": "claude-desktop",
		"curl/8.4.0":                  "other",
	}
	for ua, want := range cases {
		if got := mcpClientKey(ua); got != want {
			t.Errorf("mcpClientKey(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestBuildTrafficFlow_AgentsListedBeforeFilter(t *testing.T) {
	llm := []sankeyRow{
		{Client: "claude-code", Model: "m", Provider: "anthropic", Calls: 10},
		{Client: "cursor", Model: "m", Provider: "anthropic", Calls: 5},
	}
	mcp := []mcpFlowRow{
		{Client: "", Connector: "mesh", Calls: 7},
		{Client: "cursor", Connector: "mesh", Calls: 1},
	}
	got := buildTrafficFlow(llm, mcp, 10, analytics.SankeyMetricCalls, "cursor")

	// Agents: every family in the window, Value desc, both planes summed.
	want := []analytics.SankeyAgent{
		{Key: "claude-code", Label: "Claude Code", Value: 10},
		{Key: "unknown", Label: "Unknown", Value: 7},
		{Key: "cursor", Label: "Cursor", Value: 6},
	}
	if len(got.Agents) != len(want) {
		t.Fatalf("Agents = %+v, want %+v", got.Agents, want)
	}
	for i := range want {
		if got.Agents[i] != want[i] {
			t.Errorf("Agents[%d] = %+v, want %+v", i, got.Agents[i], want[i])
		}
	}

	// Graph: only cursor's rows.
	byID := flowNodesByID(t, got)
	if len(byID) != 5 || byID["CLIENT:cursor"].Value != 6 || got.Total != 6 {
		t.Errorf("filtered graph nodes = %+v total = %v, want cursor -> llm/mcp -> m/mesh with total 6", got.Nodes, got.Total)
	}
	if _, ok := byID["CLIENT:claude-code"]; ok {
		t.Error("CLIENT:claude-code must not be in a cursor-filtered graph")
	}
}

func TestBuildTrafficFlow_AgentsIgnoreMCPForTokens(t *testing.T) {
	llm := []sankeyRow{{Client: "cursor", Model: "m", Provider: "anthropic", Calls: 1, Tokens: 50}}
	mcp := []mcpFlowRow{{Client: "codex", Connector: "mesh", Calls: 9}}
	got := buildTrafficFlow(llm, mcp, 10, analytics.SankeyMetricTokens, "")
	if len(got.Agents) != 1 || got.Agents[0].Key != "cursor" || got.Agents[0].Value != 50 {
		t.Errorf("Agents = %+v, want only cursor with 50 tokens (MCP has no tokens)", got.Agents)
	}
}
