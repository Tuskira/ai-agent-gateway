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
  /** Secondary line under `label`; only traffic-flow MODEL nodes carry one
   * (the provider key that served most of the model's traffic). */
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

/** One client family with traffic in the window (`TrafficFlow.agents`). */
export interface SankeyAgent {
  key: string
  label: string
  value: number
}

/** `GET /api/v1/analytics/traffic-flow`'s body -- the shared graph shape
 * over four node types, plus `agents`: every client family in the window,
 * value desc, computed before any `client_name` filter (so the agent select
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
 * `GET /api/v1/analytics/traffic-flow?range=…` (or `from=&to=`) `&client_name=…` (metric is
 * always `calls` -- MCP calls carry no tokens or cost). Like
 * `fetchModelsSummary`, any failure (404 because ClickHouse isn't enabled,
 * or a network error) resolves to `null` rather than throwing -- the card
 * renders its "analytics are off" state instead of an error screen.
 */
export async function fetchTrafficFlow(
  period: Period,
  clientName = '',
): Promise<TrafficFlow | null> {
  const params = new URLSearchParams({ ...periodParams(period), metric: 'calls' })
  if (clientName) params.set('client_name', clientName)
  try {
    return await apiFetch<TrafficFlow>(`/analytics/traffic-flow?${params.toString()}`)
  } catch {
    return null
  }
}
