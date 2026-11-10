import { apiFetch } from '@/lib/api'
import { MODEL_VENDORS } from '@/lib/models'
import { periodParams, type Period, type TimeRange } from '@/lib/overview'

/**
 * Data contracts for the gateway's Sankey analytics routes
 * (`internal/api/handlers/analytics.go`, backed by `pkg/analytics.Sankey`):
 *
 * - `GET /api/v1/analytics/client-models` -- CLIENT -> MODEL -> PROVIDER
 *   over `llm_calls` (docs/observability.md#client--model-sankey).
 * - `GET /api/v1/analytics/traffic-flow` -- CLIENT -> PATH -> MODEL |
 *   CONNECTOR joining both planes (docs/observability.md#agent-traffic-flow),
 *   which the Overview "Agent traffic flow" card renders.
 *
 * Both share one node/link shape: nodes keyed by `key` within a `type`
 * column, links referencing `(key, type)` pairs, and `id` = "TYPE:key"
 * unique across the whole response.
 */

export type SankeyMetric = 'calls' | 'tokens' | 'cost'

export type SankeyNodeType = 'CLIENT' | 'MODEL' | 'PROVIDER' | 'PATH' | 'CONNECTOR'

export interface SankeyNode {
  /** Unique across the whole response ("TYPE:key"). */
  id: string
  /** Unique only within `type` -- what `SankeyLink.source`/`target` reference. */
  key: string
  label: string
  type: SankeyNodeType
  value: number
  /** Secondary line under `label`. A traffic-flow MODEL node carries the
   * provider key that served most of the model's traffic; a CLIENT node,
   * when grouped by agent, carries the identity that named it
   * (`AgentKind`). Absent everywhere else. */
  sublabel?: string
}

export interface SankeyLink {
  source: string
  sourceType: SankeyNodeType
  target: string
  targetType: SankeyNodeType
  value: number
}

export interface SankeyGraph {
  range: TimeRange | 'custom'
  metric: SankeyMetric
  total: number
  nodes: SankeyNode[]
  links: SankeyLink[]
}

/** What the traffic-flow left column groups by (`group_by`): the caller's
 * agent profile (falling back to its API key's name, then its harness),
 * its API key, or its harness (client family) only. */
export type TrafficFlowGroupBy = 'agent' | 'key' | 'client'

export const TRAFFIC_FLOW_GROUP_BY_DEFAULT: TrafficFlowGroupBy = 'agent'

/** Narrow an untrusted value (a URL parameter) to a grouping, defaulting
 * to `agent`. */
export function parseGroupBy(raw: string | null | undefined): TrafficFlowGroupBy {
  return raw === 'key' || raw === 'client' ? raw : TRAFFIC_FLOW_GROUP_BY_DEFAULT
}

/** The identity that named a CLIENT node when grouped by agent
 * (`pkg/analytics.SankeyKind*`): shown as the node's sublabel. */
export type AgentKind = 'profile' | 'key' | 'client'

/** Reader-facing wording for an `AgentKind`. The `client` kind is called
 * the harness everywhere in the console (it is the tool the agent runs in:
 * Claude Code, Cursor, ...). */
export const AGENT_KIND_LABEL: Record<AgentKind, string> = {
  profile: 'profile',
  key: 'API key',
  client: 'harness',
}

/** The badge text for a node/agent `sublabel`, or `''` when it is not an
 * `AgentKind` (the provider key on a MODEL node, or nothing). */
export function agentKindLabel(sublabel: string | undefined): string {
  return sublabel && sublabel in AGENT_KIND_LABEL
    ? AGENT_KIND_LABEL[sublabel as AgentKind]
    : ''
}

/** One node of the traffic-flow left column with traffic in the window
 * (`TrafficFlow.agents`). */
export interface SankeyAgent {
  key: string
  label: string
  value: number
  /** The `AgentKind` that named it; only when grouped by agent. */
  sublabel?: string
}

/** `GET /api/v1/analytics/traffic-flow`'s body -- the shared graph shape
 * over four node types, plus `agents`: every left-column node in the
 * window, value desc, computed before any `agent` filter (so the select
 * stays fully populated while one is picked). */
export interface TrafficFlow extends SankeyGraph {
  agents: SankeyAgent[]
}

/** PATH column keys (`pkg/analytics.SankeyPath*Key`). */
export type TrafficPath = 'llm' | 'mcp'

/** Synthetic / fold-in node keys the card must not deep-link from
 * (`pkg/analytics.SankeyOther*Key`, `SankeyGatewayToolsKey`). */
export const SANKEY_OTHER_MODELS_KEY = 'other-models'
export const SANKEY_OTHER_CONNECTORS_KEY = 'other-connectors'
export const SANKEY_OTHER_AGENTS_KEY = 'other-agents'
export const SANKEY_GATEWAY_TOOLS_KEY = 'gateway'

/** Display name for the provider key a MODEL node's `sublabel` carries --
 * the vendor that served the model (`resolved_vendor`, else the client's
 * dialect), read from the data. Names come from the model registry's own
 * vendor list; the LLM plane's `openai` dialect maps onto its
 * `openai_compat` entry, and any other key shows as-is. */
export function providerLabel(key: string | undefined): string {
  if (!key) return ''
  const vendor = key === 'openai' ? 'openai_compat' : key
  return MODEL_VENDORS.find((v) => v.value === vendor)?.label ?? key
}

/**
 * `GET /api/v1/analytics/traffic-flow?range=…` (or `from=&to=`) `&group_by=…`
 * `&agent=…` (metric is always `calls` -- MCP calls carry no tokens or
 * cost). `agent` is a left-column node key from the response's `agents`
 * list and only means something with the same `groupBy` that produced it.
 * Like `fetchModelsSummary`, any failure (404 because ClickHouse isn't
 * enabled, or a network error) resolves to `null` rather than throwing --
 * the card renders its "analytics are off" state instead of an error
 * screen.
 */
export async function fetchTrafficFlow(
  period: Period,
  groupBy: TrafficFlowGroupBy = TRAFFIC_FLOW_GROUP_BY_DEFAULT,
  agent = '',
): Promise<TrafficFlow | null> {
  const params = new URLSearchParams({
    ...periodParams(period),
    metric: 'calls',
    group_by: groupBy,
  })
  if (agent) params.set('agent', agent)
  try {
    return await apiFetch<TrafficFlow>(`/analytics/traffic-flow?${params.toString()}`)
  } catch {
    return null
  }
}
