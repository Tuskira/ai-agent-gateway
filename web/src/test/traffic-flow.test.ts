import { describe, expect, it } from 'vitest'
import type { TrafficFlow } from '@/lib/sankey'
import {
  toBalancedData,
  TRAFFIC_FLOW_COLUMNS,
  trafficFlowColumns,
} from '@/lib/traffic-flow'

const FLOW: TrafficFlow = {
  range: '24h',
  metric: 'calls',
  total: 500,
  agents: [{ key: 'cursor', label: 'Cursor', value: 500 }],
  nodes: [
    { id: 'CLIENT:cursor', key: 'cursor', label: 'Cursor', type: 'CLIENT', value: 500 },
    { id: 'PATH:llm', key: 'llm', label: 'LLM calls', type: 'PATH', value: 400 },
    { id: 'PATH:mcp', key: 'mcp', label: 'MCP calls', type: 'PATH', value: 100 },
    {
      id: 'MODEL:m',
      key: 'm',
      label: 'm',
      type: 'MODEL',
      value: 400,
      sublabel: 'anthropic',
    },
    { id: 'CONNECTOR:mesh', key: 'mesh', label: 'mesh', type: 'CONNECTOR', value: 100 },
  ],
  links: [
    {
      source: 'cursor',
      sourceType: 'CLIENT',
      target: 'llm',
      targetType: 'PATH',
      value: 400,
    },
    {
      source: 'cursor',
      sourceType: 'CLIENT',
      target: 'mcp',
      targetType: 'PATH',
      value: 100,
    },
    { source: 'llm', sourceType: 'PATH', target: 'm', targetType: 'MODEL', value: 400 },
    {
      source: 'mcp',
      sourceType: 'PATH',
      target: 'mesh',
      targetType: 'CONNECTOR',
      value: 100,
    },
  ],
}

describe('toBalancedData', () => {
  const { sankey, meta } = toBalancedData(FLOW)

  it('keys nodes by the response id and folds MODEL + CONNECTOR into one column', () => {
    expect(sankey.columns).toBe(TRAFFIC_FLOW_COLUMNS)
    expect(sankey.nodes.map((n) => [n.key, n.column])).toEqual([
      ['CLIENT:cursor', 'CLIENT'],
      ['PATH:llm', 'PATH'],
      ['PATH:mcp', 'PATH'],
      ['MODEL:m', 'TARGET'],
      ['CONNECTOR:mesh', 'TARGET'],
    ])
    expect(sankey.total).toBe(500)
  })

  it('gives every column the same total so the balanced layout fills edge to edge', () => {
    const sums: Record<string, number> = {}
    for (const n of sankey.nodes) sums[n.column] = (sums[n.column] ?? 0) + n.value
    expect(sums).toEqual({ CLIENT: 500, PATH: 500, TARGET: 500 })
  })

  it('resolves links through (type, key) ids with percent of the source node', () => {
    expect(sankey.links[0]).toEqual({
      source: 'CLIENT:cursor',
      target: 'PATH:llm',
      value: 400,
      percent: 80,
    })
    expect(sankey.links[3]).toEqual({
      source: 'PATH:mcp',
      target: 'CONNECTOR:mesh',
      value: 100,
      percent: 100,
    })
  })

  it('computes node percent as share of the whole window and keeps the response node in meta', () => {
    expect(sankey.nodes[1]!.percent).toBe(80)
    expect(meta.get('MODEL:m')?.sublabel).toBe('anthropic')
  })

  it('survives a zero total without dividing by zero', () => {
    const empty = toBalancedData({
      ...FLOW,
      total: 0,
      nodes: FLOW.nodes.slice(0, 1),
      links: [],
    })
    expect(empty.sankey.nodes[0]!.percent).toBe(0)
  })
})

describe('trafficFlowColumns', () => {
  it('names the left column after the grouping and keeps the other two', () => {
    expect(trafficFlowColumns('agent')[0]!.label).toBe('Agent')
    expect(trafficFlowColumns('key')[0]!.label).toBe('API key')
    expect(trafficFlowColumns('client')[0]!.label).toBe('Harness')
    for (const g of ['agent', 'key', 'client'] as const) {
      const cols = trafficFlowColumns(g)
      expect(cols.map((c) => c.id)).toEqual(['CLIENT', 'PATH', 'TARGET'])
      expect(cols[0]!.labelSide).toBe('left')
    }
  })

  it('toBalancedData takes its column label from the grouping', () => {
    expect(toBalancedData(FLOW).sankey.columns[0]!.label).toBe('Agent')
    expect(toBalancedData(FLOW, 'key').sankey.columns[0]!.label).toBe('API key')
    expect(toBalancedData(FLOW, 'client').sankey.columns[0]!.label).toBe('Harness')
  })

  it('links a kind-prefixed agent key to its node', () => {
    const { sankey } = toBalancedData({
      ...FLOW,
      nodes: [
        {
          id: 'CLIENT:profile:Research Agent',
          key: 'profile:Research Agent',
          label: 'Research Agent',
          type: 'CLIENT',
          value: 500,
          sublabel: 'profile',
        },
        ...FLOW.nodes.slice(1),
      ],
      links: [
        {
          source: 'profile:Research Agent',
          sourceType: 'CLIENT',
          target: 'llm',
          targetType: 'PATH',
          value: 400,
        },
        {
          source: 'profile:Research Agent',
          sourceType: 'CLIENT',
          target: 'mcp',
          targetType: 'PATH',
          value: 100,
        },
        ...FLOW.links.slice(2),
      ],
    })
    const ids = new Set(sankey.nodes.map((n) => n.key))
    expect(sankey.links[0]!.source).toBe('CLIENT:profile:Research Agent')
    for (const l of sankey.links) {
      expect(ids.has(l.source)).toBe(true)
      expect(ids.has(l.target)).toBe(true)
    }
  })
})
