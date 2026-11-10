package clickhouse

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

/* ---------------------------------------------------------------------- */
/* Client -> path -> model | connector traffic-flow Sankey                 */
/* ---------------------------------------------------------------------- */

// TrafficFlowSankey implements analytics.Reader.
func (s *Sink) TrafficFlowSankey(ctx context.Context, tenantID string, q analytics.SankeyQuery) (*analytics.TrafficFlow, error) {
	from, to := q.Period.Start, q.Period.End
	sel := newFlowSelect(q)
	// Profile and key id are only read when the left column needs them;
	// grouping by client keeps the narrower, original queries.
	byAgent := sel.groupBy != analytics.SankeyGroupClient

	// Both planes are read unfiltered: the response's Agents list has to
	// cover every node in the window, so the ClientName and Agent filters
	// are applied in buildTrafficFlow (the grouped rows are a handful
	// either way).
	llm, err := s.sankeyUsage(ctx, tenantID, from, to, "", byAgent)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: traffic-flow sankey: %w", err)
	}

	// Tokens and cost are LLM-only measures, so the MCP query is skipped
	// (not just zeroed) for those metrics -- see analytics.TrafficFlow.
	var mcp []mcpFlowRow
	if q.Metric == "" || q.Metric == analytics.SankeyMetricCalls {
		if mcp, err = s.mcpFlowUsage(ctx, tenantID, from, to, byAgent); err != nil {
			return nil, fmt.Errorf("clickhouse: traffic-flow sankey: %w", err)
		}
	}

	limit := q.Limit
	if limit <= 0 {
		limit = analytics.SankeyDefaultLimit
	}

	out := buildTrafficFlow(llm, mcp, limit, q.Metric, sel)
	out.Range = q.Range
	out.Metric = q.Metric
	return out, nil
}

// mcpFlowRow is one (client family, connector, profile, key) grouped count
// of tools/call rows read from mcp_access_logs. Client is already
// classified through sink.ClientFamily ("" for no User-Agent at all, like
// sankeyRow.Client). Profile is mcp_access_logs.profile and KeyID is
// mcp_access_logs.key_id; both are "" when the query did not ask for them
// or the row has none.
type mcpFlowRow struct {
	Client    string
	Connector string
	Profile   string
	KeyID     string
	Calls     int64
}

// mcpClientKey classifies a raw MCP-plane User-Agent into the same CLIENT
// node key the LLM plane stores in llm_calls.client_name, so one agent is
// one node whichever plane it called through: sink.ClientFamily is the
// single classifier for both, and its "" (no header) result folds into
// the unknown bucket exactly like clientKey does for llm_calls.
func mcpClientKey(userAgent string) string {
	return clientKey(sink.ClientFamily(userAgent))
}

// mcpFlowUsage counts tools/call rows in mcp_access_logs grouped by the
// raw user_agent and connector_id (plus profile and key_id when byAgent),
// then classifies each user_agent in Go with sink.ClientFamily.
// mcp_access_logs stores no client_name column; grouping by the raw header
// first keeps the row count to the number of distinct agent/version
// strings (small) and keeps the classification in one place instead of
// re-expressing it in SQL.
func (s *Sink) mcpFlowUsage(ctx context.Context, tenantID string, from, to time.Time, byAgent bool) ([]mcpFlowRow, error) {
	where := []string{"tenant_id = ?", "timestamp >= ?", "timestamp < ?", "method = 'tools/call'"}
	args := []any{tenantID, from, to}

	agentCols, agentGroup := "'' AS profile, '' AS key_id", ""
	if byAgent {
		agentCols, agentGroup = "profile, key_id", ", profile, key_id"
	}
	query := fmt.Sprintf(`SELECT user_agent, connector_id, %s, count() AS calls
		FROM mcp_access_logs
		WHERE %s
		GROUP BY user_agent, connector_id%s
		ORDER BY calls DESC, connector_id, user_agent`, agentCols, strings.Join(where, " AND "), agentGroup)
	rows, err := s.c.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Merge every user_agent that classifies to the same family.
	type mcpFlowKey struct{ client, connector, profile, keyID string }
	merged := map[mcpFlowKey]int64{}
	for rows.Next() {
		var ua, connector, profile, keyID string
		var calls uint64
		if err := rows.Scan(&ua, &connector, &profile, &keyID, &calls); err != nil {
			return nil, err
		}
		merged[mcpFlowKey{sink.ClientFamily(ua), connector, profile, keyID}] += int64(calls)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]mcpFlowRow, 0, len(merged))
	for k, calls := range merged {
		out = append(out, mcpFlowRow{Client: k.client, Connector: k.connector, Profile: k.profile, KeyID: k.keyID, Calls: calls})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Calls != b.Calls {
			return a.Calls > b.Calls
		}
		if a.Client != b.Client {
			return a.Client < b.Client
		}
		if a.Connector != b.Connector {
			return a.Connector < b.Connector
		}
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		return a.KeyID < b.KeyID
	})
	return out, nil
}

/* ---------------------------------------------------------------------- */
/* buildTrafficFlow -- pure, unit-tested (trafficflow_test.go)             */
/* ---------------------------------------------------------------------- */

// connectorKey normalizes an empty connector_id (a gateway-native tool
// served in-process, never routed to a connector) into the "gateway"
// CONNECTOR node.
func connectorKey(raw string) string {
	if raw == "" {
		return analytics.SankeyGatewayToolsKey
	}
	return raw
}

func connectorLabel(key string) string {
	switch key {
	case analytics.SankeyGatewayToolsKey:
		return analytics.SankeyGatewayToolsLabel
	case analytics.SankeyOtherConnectorsKey:
		return analytics.SankeyOtherConnectorsLabel
	}
	return key
}

// topKeys ranks totals' keys by value desc (key asc on ties) and returns
// the set of the first `limit` -- every other key folds into a synthetic
// "other" node. limit <= 0 keeps every key.
func topKeys(totals map[string]float64, limit int) map[string]bool {
	ranked := make([]string, 0, len(totals))
	for k := range totals {
		ranked = append(ranked, k)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if totals[ranked[i]] != totals[ranked[j]] {
			return totals[ranked[i]] > totals[ranked[j]]
		}
		return ranked[i] < ranked[j]
	})
	kept := len(ranked)
	if limit > 0 && limit < kept {
		kept = limit
	}
	top := make(map[string]bool, kept)
	for _, k := range ranked[:kept] {
		top[k] = true
	}
	return top
}

// flowSelect is what buildTrafficFlow needs to know about the left column:
// what it groups by, which node (if any) to narrow to, and the names the
// Reader cannot resolve itself.
type flowSelect struct {
	groupBy    analytics.SankeyGroupBy
	agent      string
	clientName string
	dir        analytics.AgentDirectory
}

func newFlowSelect(q analytics.SankeyQuery) flowSelect {
	g := q.GroupBy
	if g == "" {
		g = analytics.SankeyGroupClient
	}
	sel := flowSelect{groupBy: g, agent: q.Agent, clientName: q.ClientName}
	if q.Directory != nil {
		sel.dir = *q.Directory
	}
	return sel
}

// flowAgent is one node of the CLIENT column: its key, display label and,
// when grouped by agent, the identity kind that named it.
type flowAgent struct{ key, label, kind string }

const (
	flowProfilePrefix = "profile:"
	flowKeyPrefix     = "key:"
)

// shortKeyID labels an API key whose name the directory cannot supply (a
// key deleted since the call), without printing a whole uuid.
func shortKeyID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// identify names the CLIENT node one grouped row belongs to.
//
// Grouped by client it is the client family. Grouped by key it is the API
// key id (rows with none -- ingested interceptor rows -- are "unknown").
// Grouped by agent it is the first of: the profile stored on the row (the
// key's bound profile at call time, else the caller's X-Agent-Profile-Name
// header), the profile the key is bound to now (for rows written before
// the profile was recorded), the API key's name, the client family, and
// finally "unknown". Profile and key nodes get a kind prefix so a profile
// called "cursor" never collides with the Cursor client; a client node
// keeps its bare family key, the same key group_by=client gives it.
func (s flowSelect) identify(client, profile, keyID string) flowAgent {
	switch s.groupBy {
	case analytics.SankeyGroupClient:
		return flowAgent{key: clientKey(client), label: clientLabel(clientKey(client))}
	case analytics.SankeyGroupKey:
		if keyID == "" {
			return flowAgent{key: analytics.SankeyUnknownClientKey, label: analytics.SankeyUnknownClientLabel}
		}
		if name := s.dir.KeyNames[keyID]; name != "" {
			return flowAgent{key: keyID, label: name}
		}
		return flowAgent{key: keyID, label: shortKeyID(keyID)}
	}

	if profile = strings.TrimSpace(profile); profile != "" {
		return flowAgent{key: flowProfilePrefix + profile, label: profile, kind: analytics.SankeyKindProfile}
	}
	if keyID != "" {
		if bound := s.dir.KeyProfiles[keyID]; bound != "" {
			return flowAgent{key: flowProfilePrefix + bound, label: bound, kind: analytics.SankeyKindProfile}
		}
		if name := s.dir.KeyNames[keyID]; name != "" {
			return flowAgent{key: flowKeyPrefix + keyID, label: name, kind: analytics.SankeyKindKey}
		}
	}
	if ck := clientKey(client); ck != analytics.SankeyUnknownClientKey {
		return flowAgent{key: ck, label: clientLabel(ck), kind: analytics.SankeyKindClient}
	}
	return flowAgent{key: analytics.SankeyUnknownClientKey, label: analytics.SankeyUnknownClientLabel}
}

// buildTrafficFlow turns the two planes' grouped rows into the CLIENT ->
// PATH -> MODEL | CONNECTOR graph GET /api/v1/analytics/traffic-flow
// returns. Each LLM row contributes its metric Value to one CLIENT->PATH:llm
// link and one PATH:llm->MODEL link; each MCP row contributes its Calls to
// CLIENT->PATH:mcp and PATH:mcp->CONNECTOR. MCP rows count only under
// SankeyMetricCalls -- tokens and cost are LLM-only measures, so for those
// metrics mcp is ignored outright (whatever the caller passed). A PATH
// node exists only when its branch has traffic, so a tokens/cost graph has
// a single LLM path with no empty MCP stub beside it.
//
// The CLIENT node of each row comes from sel.identify (see there); when
// the column is grouped by agent or key, the top analytics.SankeyAgentLimit
// nodes keep their own node and the rest fold into "other-agents".
//
// Folding mirrors buildSankey per terminal column: the top `limit` models
// keep their own MODEL node (the rest fold into "other-models") and,
// separately, the top `limit` connectors keep their own CONNECTOR node
// (the rest fold into "other-connectors"). A MODEL node's Sublabel is the
// provider that served most of that model's traffic; folded and CONNECTOR
// nodes carry none.
//
// Agents is computed from every row before the filters are applied; when
// sel.clientName or sel.agent is set only the matching rows build the
// graph (and the fold ranking), so the graph is exactly what a filtered
// query of each plane would have produced.
func buildTrafficFlow(llm []sankeyRow, mcp []mcpFlowRow, limit int, metric analytics.SankeyMetric, sel flowSelect) *analytics.TrafficFlow {
	out := &analytics.TrafficFlow{
		Sankey: analytics.Sankey{Nodes: []analytics.SankeyNode{}, Links: []analytics.SankeyLink{}},
		Agents: []analytics.SankeyAgent{},
	}

	if metric != "" && metric != analytics.SankeyMetricCalls {
		mcp = nil
	}

	llmAgent := func(r sankeyRow) flowAgent { return sel.identify(r.Client, r.Profile, r.KeyID) }
	mcpAgent := func(r mcpFlowRow) flowAgent { return sel.identify(r.Client, r.Profile, r.KeyID) }

	// Every node in the window, before filtering.
	agentTotals := map[string]float64{}
	agents := map[string]flowAgent{}
	for _, r := range llm {
		a := llmAgent(r)
		agentTotals[a.key] += r.value(metric)
		agents[a.key] = a
	}
	for _, r := range mcp {
		a := mcpAgent(r)
		agentTotals[a.key] += float64(r.Calls)
		agents[a.key] = a
	}
	for _, n := range sortedNodes(agentTotals, analytics.SankeyNodeClient, nil) {
		if n.Value == 0 {
			continue
		}
		a := agents[n.Key]
		out.Agents = append(out.Agents, analytics.SankeyAgent{Key: a.key, Label: a.label, Value: n.Value, Sublabel: a.kind})
	}

	if sel.clientName != "" {
		llm = filterRows(llm, func(r sankeyRow) bool { return clientKey(r.Client) == sel.clientName })
		mcp = filterRows(mcp, func(r mcpFlowRow) bool { return clientKey(r.Client) == sel.clientName })
	}
	if sel.agent != "" {
		llm = filterRows(llm, func(r sankeyRow) bool { return llmAgent(r).key == sel.agent })
		mcp = filterRows(mcp, func(r mcpFlowRow) bool { return mcpAgent(r).key == sel.agent })
	}

	// The left column folds its tail unless it is the handful of client
	// families.
	agentNode := func(a flowAgent) string { return a.key }
	if sel.groupBy != analytics.SankeyGroupClient {
		kept := map[string]float64{}
		for _, r := range llm {
			kept[llmAgent(r).key] += r.value(metric)
		}
		for _, r := range mcp {
			kept[mcpAgent(r).key] += float64(r.Calls)
		}
		topAgents := topKeys(kept, analytics.SankeyAgentLimit)
		agentNode = func(a flowAgent) string {
			if topAgents[a.key] {
				return a.key
			}
			return analytics.SankeyOtherAgentsKey
		}
	}

	// Rank the terminal columns to decide who keeps their own node.
	modelTotals := map[string]float64{}
	for _, r := range llm {
		modelTotals[r.Model] += r.value(metric)
	}
	connectorTotals := map[string]float64{}
	for _, r := range mcp {
		connectorTotals[connectorKey(r.Connector)] += float64(r.Calls)
	}
	topModels := topKeys(modelTotals, limit)
	topConnectors := topKeys(connectorTotals, limit)

	clientPath := map[sankeyLinkKey]float64{}
	pathModel := map[sankeyLinkKey]float64{}
	pathConnector := map[sankeyLinkKey]float64{}
	clientValue := map[string]float64{}
	pathValue := map[string]float64{}
	modelValue := map[string]float64{}
	connectorValue := map[string]float64{}
	// modelProviders[model][provider] = value, to pick the dominant one.
	modelProviders := map[string]map[string]float64{}

	for _, r := range llm {
		v := r.value(metric)
		if v == 0 {
			continue
		}
		out.Total += v
		ck := agentNode(llmAgent(r))
		mk := r.Model
		if !topModels[mk] {
			mk = analytics.SankeyOtherModelsKey
		}
		clientPath[sankeyLinkKey{ck, analytics.SankeyPathLLMKey}] += v
		pathModel[sankeyLinkKey{analytics.SankeyPathLLMKey, mk}] += v
		clientValue[ck] += v
		pathValue[analytics.SankeyPathLLMKey] += v
		modelValue[mk] += v
		if mk != analytics.SankeyOtherModelsKey {
			if modelProviders[mk] == nil {
				modelProviders[mk] = map[string]float64{}
			}
			modelProviders[mk][r.Provider] += v
		}
	}

	for _, r := range mcp {
		v := float64(r.Calls)
		if v == 0 {
			continue
		}
		out.Total += v
		ck := agentNode(mcpAgent(r))
		nk := connectorKey(r.Connector)
		if !topConnectors[nk] {
			nk = analytics.SankeyOtherConnectorsKey
		}
		clientPath[sankeyLinkKey{ck, analytics.SankeyPathMCPKey}] += v
		pathConnector[sankeyLinkKey{analytics.SankeyPathMCPKey, nk}] += v
		clientValue[ck] += v
		pathValue[analytics.SankeyPathMCPKey] += v
		connectorValue[nk] += v
	}

	if len(clientValue) == 0 {
		return out
	}

	// Nodes, one column at a time.
	clientLabels_ := map[string]string{}
	for k := range clientValue {
		if k == analytics.SankeyOtherAgentsKey {
			clientLabels_[k] = analytics.SankeyOtherAgentsLabel
		} else {
			clientLabels_[k] = agents[k].label
		}
	}
	clientNodes := sortedNodes(clientValue, analytics.SankeyNodeClient, clientLabels_)
	for i := range clientNodes {
		clientNodes[i].Sublabel = agents[clientNodes[i].Key].kind
	}
	out.Nodes = append(out.Nodes, clientNodes...)

	// PATH keeps a fixed LLM-then-MCP order regardless of volume so the
	// two planes never swap places between ranges.
	for _, p := range []struct{ key, label string }{
		{analytics.SankeyPathLLMKey, analytics.SankeyPathLLMLabel},
		{analytics.SankeyPathMCPKey, analytics.SankeyPathMCPLabel},
	} {
		if v, ok := pathValue[p.key]; ok {
			out.Nodes = append(out.Nodes, analytics.SankeyNode{
				ID: analytics.SankeyNodePath + ":" + p.key, Key: p.key, Label: p.label,
				Type: analytics.SankeyNodePath, Value: v,
			})
		}
	}

	modelLabels := map[string]string{}
	for name := range modelValue {
		if name == analytics.SankeyOtherModelsKey {
			modelLabels[name] = analytics.SankeyOtherModelsLabel
		} else {
			modelLabels[name] = name
		}
	}
	modelNodes := sortedNodes(modelValue, analytics.SankeyNodeModel, modelLabels)
	for i := range modelNodes {
		modelNodes[i].Sublabel = dominantProvider(modelProviders[modelNodes[i].Key])
	}
	out.Nodes = append(out.Nodes, modelNodes...)

	connectorLabels := map[string]string{}
	for k := range connectorValue {
		connectorLabels[k] = connectorLabel(k)
	}
	out.Nodes = append(out.Nodes, sortedNodes(connectorValue, analytics.SankeyNodeConnector, connectorLabels)...)

	// Links: CLIENT->PATH, PATH->MODEL, PATH->CONNECTOR, each Value desc.
	out.Links = append(out.Links, sortedLinks(clientPath, analytics.SankeyNodeClient, analytics.SankeyNodePath)...)
	out.Links = append(out.Links, sortedLinks(pathModel, analytics.SankeyNodePath, analytics.SankeyNodeModel)...)
	out.Links = append(out.Links, sortedLinks(pathConnector, analytics.SankeyNodePath, analytics.SankeyNodeConnector)...)

	return out
}

// dominantProvider returns the provider key carrying the largest share of
// values (key asc on ties), or "" when values is empty/nil -- an empty
// resolved provider never becomes a sublabel.
func dominantProvider(values map[string]float64) string {
	best, bestV := "", 0.0
	for p, v := range values {
		if p == "" {
			continue
		}
		if v > bestV || (v == bestV && best != "" && p < best) {
			best, bestV = p, v
		}
	}
	return best
}

// filterRows returns the rows for which keep is true, in order.
func filterRows[T any](rows []T, keep func(T) bool) []T {
	out := rows[:0:0]
	for _, r := range rows {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}
