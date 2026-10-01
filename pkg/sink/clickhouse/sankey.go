package clickhouse

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
)

/* ---------------------------------------------------------------------- */
/* Client -> model -> provider Sankey                                     */
/* ---------------------------------------------------------------------- */

// ClientModelSankey implements analytics.Reader.
func (s *Sink) ClientModelSankey(ctx context.Context, tenantID string, q analytics.SankeyQuery) (*analytics.Sankey, error) {
	now := time.Now().UTC()
	from, to := now.Add(-q.Range.Window()), now

	rows, err := s.sankeyUsage(ctx, tenantID, from, to, q.ClientName)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: client-model sankey: %w", err)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = analytics.SankeyDefaultLimit
	}

	out := buildSankey(rows, limit, q.Metric)
	out.Range = q.Range
	out.Metric = q.Metric
	return out, nil
}

// sankeyRow is one (client, model, provider) grouped row read from
// llm_calls, carrying every metric buildSankey might need so the caller
// picks the right one without a second query.
type sankeyRow struct {
	Client   string
	Model    string
	Provider string
	Calls    int64
	Tokens   int64
	CostUSD  float64
}

// sankeyUsage groups llm_calls by client_name, the model name the client
// asked for (requested_model, falling back to model like modelUsage), and
// the vendor that served most of the group's calls (resolved_vendor,
// falling back to provider -- the client's dialect -- exactly like
// modelUsage resolves ModelSummaryRow.Provider). clientName, when
// non-empty, narrows to one client family; "unknown" maps to the empty
// client_name bucket (no User-Agent sent at all -- see
// analytics.SankeyUnknownClientKey).
func (s *Sink) sankeyUsage(ctx context.Context, tenantID string, from, to time.Time, clientName string) ([]sankeyRow, error) {
	where := []string{"tenant_id = ?", "timestamp >= ?", "timestamp < ?", "(requested_model != '' OR model != '')"}
	args := []any{tenantID, from, to}
	if clientName != "" {
		if clientName == analytics.SankeyUnknownClientKey {
			where = append(where, "client_name = ''")
		} else {
			where = append(where, "client_name = ?")
			args = append(args, clientName)
		}
	}

	query := fmt.Sprintf(`SELECT client_name,
			if(requested_model != '', requested_model, model) AS model_name,
			topKIf(1)(resolved_vendor, resolved_vendor != '') AS vendors,
			topK(1)(provider) AS dialects,
			count() AS calls,
			sum(input_tokens + output_tokens + cache_read_tokens + cache_creation_tokens) AS tokens,
			toFloat64(ifNull(sum(cost_usd), 0)) AS cost_sum
		FROM llm_calls
		WHERE %s
		GROUP BY client_name, model_name
		ORDER BY tokens DESC, model_name`, strings.Join(where, " AND "))
	rowsRS, err := s.c.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rowsRS.Close()

	out := []sankeyRow{}
	for rowsRS.Next() {
		var client, modelName string
		var vendors, dialects []string
		var calls, tokens uint64
		var costSum float64
		if err := rowsRS.Scan(&client, &modelName, &vendors, &dialects, &calls, &tokens, &costSum); err != nil {
			return nil, err
		}
		provider := ""
		if len(vendors) > 0 {
			provider = vendors[0]
		} else if len(dialects) > 0 {
			provider = dialects[0]
		}
		out = append(out, sankeyRow{
			Client:   client,
			Model:    modelName,
			Provider: provider,
			Calls:    int64(calls),
			Tokens:   int64(tokens),
			CostUSD:  costSum,
		})
	}
	return out, rowsRS.Err()
}

/* ---------------------------------------------------------------------- */
/* buildSankey -- pure, unit-tested (sankey_test.go)                       */
/* ---------------------------------------------------------------------- */

// value returns the row's measure for metric: calls, tokens, or cost.
func (r sankeyRow) value(metric analytics.SankeyMetric) float64 {
	switch metric {
	case analytics.SankeyMetricTokens:
		return float64(r.Tokens)
	case analytics.SankeyMetricCost:
		return r.CostUSD
	default: // analytics.SankeyMetricCalls, and any unset/unknown value
		return float64(r.Calls)
	}
}

// clientKey normalizes sink.LLMCall.ClientName ("claude-code", "cursor",
// "vscode", "codex", "claude-desktop", "other", or "" for no User-Agent
// sent at all) into the Sankey CLIENT node's key: "" folds into
// analytics.SankeyUnknownClientKey, everything else passes through.
func clientKey(raw string) string {
	if raw == "" {
		return analytics.SankeyUnknownClientKey
	}
	return raw
}

// clientLabels maps every sink.ClientFamily output (plus the unknown
// bucket) to the CLIENT node's display label.
var clientLabels = map[string]string{
	"claude-code":                    "Claude Code",
	"claude-desktop":                 "Claude Desktop",
	"cursor":                         "Cursor",
	"vscode":                         "VS Code",
	"codex":                          "Codex",
	"other":                          "Other",
	analytics.SankeyUnknownClientKey: analytics.SankeyUnknownClientLabel,
}

// clientLabel returns key's display label, falling back to key itself for
// any family sink.ClientFamily might add in the future that this map
// hasn't been updated for yet -- never an empty label.
func clientLabel(key string) string {
	if label, ok := clientLabels[key]; ok {
		return label
	}
	return key
}

// providerKey normalizes an empty resolved provider (should not happen in
// practice -- sink.LLMCall.Provider, the client's dialect, is always set --
// but a defensive fallback keeps buildSankey total instead of ever
// producing a blank node key/id).
func providerKey(raw string) string {
	if raw == "" {
		return analytics.SankeyUnknownClientKey
	}
	return raw
}

func providerLabel(key string) string {
	if key == analytics.SankeyUnknownClientKey {
		return analytics.SankeyUnknownClientLabel
	}
	return key
}

// buildSankey turns sankeyUsage's grouped rows into the CLIENT -> MODEL ->
// PROVIDER graph GET /api/v1/analytics/client-models returns, picking each
// row's Value from metric.
//
// Model folding: rows are first grouped by model name and ranked by their
// total Value across every client/provider; only the top `limit` names
// become their own MODEL node -- every other name's traffic (still split
// by its own client and provider) is folded into one synthetic
// "other-models" node (analytics.SankeyOtherModelsKey), so a long tail of
// rarely-used models never balloons the node count while their traffic
// still counts.
//
// Node Value: CLIENT is the sum of its outgoing links; MODEL and PROVIDER
// are the sum of their incoming links. Because every row contributes the
// same Value to exactly one CLIENT->MODEL link and one MODEL->PROVIDER
// link, a MODEL node's incoming sum always equals its outgoing sum -- both
// are "this model/bucket's total traffic".
//
// limit <= 0 is treated as unlimited (every model keeps its own node) --
// callers (ClientModelSankey) apply analytics.SankeyDefaultLimit before
// this is ever called with a non-positive value themselves; buildSankey
// stays defensive so its unit tests can exercise "no folding" directly.
func buildSankey(rows []sankeyRow, limit int, metric analytics.SankeyMetric) *analytics.Sankey {
	out := &analytics.Sankey{Nodes: []analytics.SankeyNode{}, Links: []analytics.SankeyLink{}}
	if len(rows) == 0 {
		return out
	}

	// Rank models by total Value to decide which keep their own node.
	modelTotals := map[string]float64{}
	for _, r := range rows {
		v := r.value(metric)
		out.Total += v
		modelTotals[r.Model] += v
	}
	rankedModels := make([]string, 0, len(modelTotals))
	for name := range modelTotals {
		rankedModels = append(rankedModels, name)
	}
	sort.Slice(rankedModels, func(i, j int) bool {
		if modelTotals[rankedModels[i]] != modelTotals[rankedModels[j]] {
			return modelTotals[rankedModels[i]] > modelTotals[rankedModels[j]]
		}
		return rankedModels[i] < rankedModels[j]
	})
	topModels := map[string]bool{}
	kept := len(rankedModels)
	if limit > 0 && limit < kept {
		kept = limit
	}
	for _, name := range rankedModels[:kept] {
		topModels[name] = true
	}
	modelNodeKey := func(name string) string {
		if topModels[name] {
			return name
		}
		return analytics.SankeyOtherModelsKey
	}

	// Aggregate the two link layers plus every node's total.
	clientModel := map[sankeyLinkKey]float64{}
	modelProvider := map[sankeyLinkKey]float64{}
	clientValue := map[string]float64{}
	modelValue := map[string]float64{}
	providerValue := map[string]float64{}
	clientLabel_ := map[string]string{}
	providerLabel_ := map[string]string{}

	for _, r := range rows {
		v := r.value(metric)
		if v == 0 {
			continue
		}
		ck := clientKey(r.Client)
		mk := modelNodeKey(r.Model)
		pk := providerKey(r.Provider)

		clientModel[sankeyLinkKey{ck, mk}] += v
		modelProvider[sankeyLinkKey{mk, pk}] += v
		clientValue[ck] += v
		modelValue[mk] += v
		providerValue[pk] += v
		clientLabel_[ck] = clientLabel(ck)
		providerLabel_[pk] = providerLabel(pk)
	}

	// Nodes, one column at a time, each sorted Value desc then key asc.
	out.Nodes = append(out.Nodes, sortedNodes(clientValue, analytics.SankeyNodeClient, clientLabel_)...)
	modelLabels := map[string]string{}
	for name := range modelValue {
		if name == analytics.SankeyOtherModelsKey {
			modelLabels[name] = analytics.SankeyOtherModelsLabel
		} else {
			modelLabels[name] = name
		}
	}
	out.Nodes = append(out.Nodes, sortedNodes(modelValue, analytics.SankeyNodeModel, modelLabels)...)
	out.Nodes = append(out.Nodes, sortedNodes(providerValue, analytics.SankeyNodeProvider, providerLabel_)...)

	// Links: CLIENT->MODEL then MODEL->PROVIDER, each sorted Value desc.
	out.Links = append(out.Links,
		sortedLinks(clientModel, analytics.SankeyNodeClient, analytics.SankeyNodeModel)...)
	out.Links = append(out.Links,
		sortedLinks(modelProvider, analytics.SankeyNodeModel, analytics.SankeyNodeProvider)...)

	return out
}

func sortedNodes(values map[string]float64, nodeType string, labels map[string]string) []analytics.SankeyNode {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if values[keys[i]] != values[keys[j]] {
			return values[keys[i]] > values[keys[j]]
		}
		return keys[i] < keys[j]
	})
	out := make([]analytics.SankeyNode, 0, len(keys))
	for _, k := range keys {
		out = append(out, analytics.SankeyNode{
			ID:    nodeType + ":" + k,
			Key:   k,
			Label: labels[k],
			Type:  nodeType,
			Value: values[k],
		})
	}
	return out
}

// sankeyLinkKey is the (source key, target key) pair a link's aggregated
// Value is keyed by -- a named type so buildSankey's maps and sortedLinks'
// parameter are the identical Go type (an anonymous struct type here would
// not be, even with the same fields).
type sankeyLinkKey struct{ source, target string }

func sortedLinks(values map[sankeyLinkKey]float64, sourceType, targetType string) []analytics.SankeyLink {
	type entry struct {
		key   sankeyLinkKey
		value float64
	}
	entries := make([]entry, 0, len(values))
	for k, v := range values {
		entries = append(entries, entry{k, v})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].value != entries[j].value {
			return entries[i].value > entries[j].value
		}
		if entries[i].key.source != entries[j].key.source {
			return entries[i].key.source < entries[j].key.source
		}
		return entries[i].key.target < entries[j].key.target
	})
	out := make([]analytics.SankeyLink, 0, len(entries))
	for _, e := range entries {
		out = append(out, analytics.SankeyLink{
			Source:     e.key.source,
			SourceType: sourceType,
			Target:     e.key.target,
			TargetType: targetType,
			Value:      e.value,
		})
	}
	return out
}
