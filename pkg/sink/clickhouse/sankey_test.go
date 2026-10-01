package clickhouse

import (
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
)

func TestBuildSankey_Empty(t *testing.T) {
	got := buildSankey(nil, 10, analytics.SankeyMetricCalls)
	if got.Total != 0 || len(got.Nodes) != 0 || len(got.Links) != 0 {
		t.Fatalf("empty rows: got %+v, want zero-value Sankey with empty (not nil) slices", got)
	}
	if got.Nodes == nil || got.Links == nil {
		t.Error("Nodes/Links must be empty slices, not nil (so they marshal as [] not null)")
	}
}

func TestBuildSankey_BasicThreeColumn(t *testing.T) {
	rows := []sankeyRow{
		{Client: "cursor", Model: "claude-sonnet-5", Provider: "anthropic", Calls: 300, Tokens: 3000, CostUSD: 3},
		{Client: "claude-code", Model: "claude-sonnet-5", Provider: "anthropic", Calls: 100, Tokens: 1000, CostUSD: 1},
	}
	got := buildSankey(rows, 10, analytics.SankeyMetricCalls)

	if got.Total != 400 {
		t.Errorf("Total = %v, want 400", got.Total)
	}
	if len(got.Nodes) != 4 { // 2 clients + 1 model + 1 provider
		t.Fatalf("Nodes = %+v, want 4 nodes", got.Nodes)
	}
	if len(got.Links) != 3 { // 2 client->model + 1 model->provider
		t.Fatalf("Links = %+v, want 3 links", got.Links)
	}

	byID := map[string]analytics.SankeyNode{}
	for _, n := range got.Nodes {
		byID[n.ID] = n
	}
	if n, ok := byID["CLIENT:cursor"]; !ok || n.Value != 300 || n.Label != "Cursor" || n.Type != analytics.SankeyNodeClient {
		t.Errorf("CLIENT:cursor node = %+v", n)
	}
	if n, ok := byID["CLIENT:claude-code"]; !ok || n.Value != 100 || n.Label != "Claude Code" {
		t.Errorf("CLIENT:claude-code node = %+v", n)
	}
	if n, ok := byID["MODEL:claude-sonnet-5"]; !ok || n.Value != 400 || n.Label != "claude-sonnet-5" {
		t.Errorf("MODEL:claude-sonnet-5 node = %+v (model node value must sum incoming client links)", n)
	}
	if n, ok := byID["PROVIDER:anthropic"]; !ok || n.Value != 400 || n.Label != "anthropic" {
		t.Errorf("PROVIDER:anthropic node = %+v", n)
	}
}

func TestBuildSankey_EmptyClientNameFoldsToUnknown(t *testing.T) {
	rows := []sankeyRow{
		{Client: "", Model: "claude-sonnet-5", Provider: "anthropic", Calls: 50, Tokens: 500, CostUSD: 0.5},
	}
	got := buildSankey(rows, 10, analytics.SankeyMetricCalls)

	if len(got.Nodes) == 0 {
		t.Fatal("expected nodes")
	}
	var clientNode *analytics.SankeyNode
	for i := range got.Nodes {
		if got.Nodes[i].Type == analytics.SankeyNodeClient {
			clientNode = &got.Nodes[i]
		}
	}
	if clientNode == nil {
		t.Fatal("no CLIENT node produced")
	}
	if clientNode.Key != "unknown" || clientNode.ID != "CLIENT:unknown" || clientNode.Label != "Unknown" {
		t.Errorf("CLIENT node for empty client_name = %+v, want key=unknown label=Unknown", clientNode)
	}
}

func TestBuildSankey_ClientLabels(t *testing.T) {
	cases := map[string]string{
		"claude-code":    "Claude Code",
		"claude-desktop": "Claude Desktop",
		"cursor":         "Cursor",
		"vscode":         "VS Code",
		"codex":          "Codex",
		"other":          "Other",
		"":               "Unknown",
	}
	for client, wantLabel := range cases {
		rows := []sankeyRow{{Client: client, Model: "m", Provider: "p", Calls: 1}}
		got := buildSankey(rows, 10, analytics.SankeyMetricCalls)
		var found bool
		for _, n := range got.Nodes {
			if n.Type == analytics.SankeyNodeClient {
				found = true
				if n.Label != wantLabel {
					t.Errorf("client %q: label = %q, want %q", client, n.Label, wantLabel)
				}
			}
		}
		if !found {
			t.Errorf("client %q: no CLIENT node produced", client)
		}
	}
}

func TestBuildSankey_ModelFoldingBeyondLimit(t *testing.T) {
	rows := []sankeyRow{
		{Client: "cursor", Model: "model-a", Provider: "anthropic", Calls: 500},
		{Client: "cursor", Model: "model-b", Provider: "anthropic", Calls: 300},
		{Client: "cursor", Model: "model-c", Provider: "openai", Calls: 100},
		{Client: "cursor", Model: "model-d", Provider: "openai", Calls: 50},
	}
	got := buildSankey(rows, 2, analytics.SankeyMetricCalls)

	var modelKeys []string
	modelValues := map[string]float64{}
	for _, n := range got.Nodes {
		if n.Type == analytics.SankeyNodeModel {
			modelKeys = append(modelKeys, n.Key)
			modelValues[n.Key] = n.Value
		}
	}
	if len(modelKeys) != 3 { // model-a, model-b kept, model-c+model-d folded
		t.Fatalf("MODEL nodes = %v, want 3 (2 kept + other-models)", modelKeys)
	}
	if modelValues["model-a"] != 500 || modelValues["model-b"] != 300 {
		t.Errorf("top models values = %+v, want model-a=500 model-b=300", modelValues)
	}
	other, ok := modelValues[analytics.SankeyOtherModelsKey]
	if !ok || other != 150 {
		t.Errorf("other-models value = %v (ok=%v), want 150 (100+50)", other, ok)
	}

	var otherNode *analytics.SankeyNode
	for i := range got.Nodes {
		if got.Nodes[i].Key == analytics.SankeyOtherModelsKey {
			otherNode = &got.Nodes[i]
		}
	}
	if otherNode == nil || otherNode.Label != analytics.SankeyOtherModelsLabel {
		t.Errorf("other-models node = %+v, want label %q", otherNode, analytics.SankeyOtherModelsLabel)
	}

	// The folded model's traffic still fans out to BOTH original providers
	// (anthropic and openai) via the other-models node -- folding must not
	// merge providers together.
	var otherLinks []analytics.SankeyLink
	for _, l := range got.Links {
		if l.Source == analytics.SankeyOtherModelsKey {
			otherLinks = append(otherLinks, l)
		}
	}
	if len(otherLinks) != 1 {
		t.Fatalf("other-models -> provider links = %+v, want 1 (model-c and model-d share the same openai provider)", otherLinks)
	}
	if otherLinks[0].Target != "openai" || otherLinks[0].Value != 150 {
		t.Errorf("other-models link = %+v, want target=openai value=150", otherLinks[0])
	}
}

func TestBuildSankey_LimitZeroOrNegativeIsUnlimited(t *testing.T) {
	rows := []sankeyRow{
		{Client: "cursor", Model: "model-a", Provider: "anthropic", Calls: 1},
		{Client: "cursor", Model: "model-b", Provider: "anthropic", Calls: 1},
		{Client: "cursor", Model: "model-c", Provider: "anthropic", Calls: 1},
	}
	for _, limit := range []int{0, -1, -10} {
		got := buildSankey(rows, limit, analytics.SankeyMetricCalls)
		count := 0
		for _, n := range got.Nodes {
			if n.Type == analytics.SankeyNodeModel {
				count++
			}
		}
		if count != 3 {
			t.Errorf("limit=%d: MODEL node count = %d, want 3 (no folding)", limit, count)
		}
	}
}

func TestBuildSankey_MetricSelection(t *testing.T) {
	rows := []sankeyRow{
		{Client: "cursor", Model: "m", Provider: "p", Calls: 10, Tokens: 2000, CostUSD: 5.5},
	}
	if got := buildSankey(rows, 10, analytics.SankeyMetricCalls); got.Total != 10 {
		t.Errorf("calls metric: Total = %v, want 10", got.Total)
	}
	if got := buildSankey(rows, 10, analytics.SankeyMetricTokens); got.Total != 2000 {
		t.Errorf("tokens metric: Total = %v, want 2000", got.Total)
	}
	if got := buildSankey(rows, 10, analytics.SankeyMetricCost); got.Total != 5.5 {
		t.Errorf("cost metric: Total = %v, want 5.5", got.Total)
	}
}

func TestBuildSankey_ZeroValueRowsProduceNoLinks(t *testing.T) {
	rows := []sankeyRow{
		{Client: "cursor", Model: "m", Provider: "p", Calls: 0, Tokens: 0, CostUSD: 0},
	}
	got := buildSankey(rows, 10, analytics.SankeyMetricCalls)
	if len(got.Nodes) != 0 || len(got.Links) != 0 {
		t.Errorf("all-zero-value rows: got %+v, want no nodes/links", got)
	}
}

func TestBuildSankey_LinksSortedByValueDesc(t *testing.T) {
	rows := []sankeyRow{
		{Client: "cursor", Model: "model-a", Provider: "anthropic", Calls: 10},
		{Client: "vscode", Model: "model-a", Provider: "anthropic", Calls: 90},
	}
	got := buildSankey(rows, 10, analytics.SankeyMetricCalls)
	var clientModelLinks []analytics.SankeyLink
	for _, l := range got.Links {
		if l.SourceType == analytics.SankeyNodeClient {
			clientModelLinks = append(clientModelLinks, l)
		}
	}
	if len(clientModelLinks) != 2 {
		t.Fatalf("client->model links = %+v, want 2", clientModelLinks)
	}
	if clientModelLinks[0].Value < clientModelLinks[1].Value {
		t.Errorf("links not sorted by value desc: %+v", clientModelLinks)
	}
	if clientModelLinks[0].Source != "vscode" {
		t.Errorf("highest-value link source = %q, want vscode", clientModelLinks[0].Source)
	}
}

func TestBuildSankey_NodeIDsAreUniqueTypeKey(t *testing.T) {
	// A model and a provider that happen to share the same name string
	// must not collide -- IDs are namespaced by type.
	rows := []sankeyRow{
		{Client: "cursor", Model: "anthropic", Provider: "anthropic", Calls: 5},
	}
	got := buildSankey(rows, 10, analytics.SankeyMetricCalls)
	ids := map[string]bool{}
	for _, n := range got.Nodes {
		if ids[n.ID] {
			t.Errorf("duplicate node ID %q", n.ID)
		}
		ids[n.ID] = true
	}
	if !ids["MODEL:anthropic"] || !ids["PROVIDER:anthropic"] {
		t.Errorf("expected both MODEL:anthropic and PROVIDER:anthropic, got %v", ids)
	}
}
