import type {
  SankeyData,
  SankeyLink as BalancedLink,
  SankeyNode as BalancedNode,
} from '@/components/app/sankey'
import type { SankeyNode, TrafficFlow } from '@/lib/sankey'

/** The three drawn columns. MODEL and CONNECTOR nodes share the last one:
 * they are alternatives (an LLM call ends at a model, an MCP call at a
 * connector), so together they sum to the same total as the other two
 * columns and the balanced layout draws all three edge to edge. */
export const TRAFFIC_FLOW_COLUMNS: SankeyData['columns'] = [
  { id: 'CLIENT', label: 'Agent', labelSide: 'left' },
  { id: 'PATH', label: 'Path' },
  { id: 'TARGET', label: 'Model / MCP' },
]

function columnOf(type: SankeyNode['type']): string {
  return type === 'MODEL' || type === 'CONNECTOR' ? 'TARGET' : type
}

/** The response, re-keyed for the balanced renderer: node keys become the
 * response's globally unique `id` ("TYPE:key"), links reference those ids,
 * and every node's percent is its share of the whole window. `meta` maps a
 * drawn key back to its response node for labels and clicks. */
export function toBalancedData(data: TrafficFlow): {
  sankey: SankeyData
  meta: Map<string, SankeyNode>
} {
  const meta = new Map<string, SankeyNode>()
  const nodes: BalancedNode[] = data.nodes.map((n) => {
    meta.set(n.id, n)
    return {
      key: n.id,
      column: columnOf(n.type),
      label: n.label,
      value: n.value,
      percent: data.total > 0 ? (n.value / data.total) * 100 : 0,
    }
  })
  const links: BalancedLink[] = data.links.map((l) => {
    const source = `${l.sourceType}:${l.source}`
    const sourceValue = meta.get(source)?.value ?? 0
    return {
      source,
      target: `${l.targetType}:${l.target}`,
      value: l.value,
      percent: sourceValue > 0 ? (l.value / sourceValue) * 100 : 0,
    }
  })
  return {
    sankey: { columns: TRAFFIC_FLOW_COLUMNS, nodes, links, total: data.total },
    meta,
  }
}
