import { apiFetch } from '@/lib/api'
import { periodQuery, type Period } from '@/lib/overview'

/**
 * Observed usage from LLM traffic: the skills and MCP servers the model asked
 * to use, registered or not (`GET /api/v1/analytics/skills/usage` and
 * `/analytics/mcps/usage`). Both need the ClickHouse sink; like
 * `fetchSkillsSummary` any failure (404 when analytics is off, a network
 * error) resolves to `null` so pages fall back to registry-only states.
 */

export interface DiscoveredSkill {
  name: string
  calls: number
  used_by: number
  last_seen: string
  /** A tenant or platform skill/command of this name exists. */
  registered: boolean
  kind: 'skill' | 'command' | ''
}

export interface DiscoveredMcp {
  server: string
  tools: string[]
  /** Distinct LLM calls that used at least one tool of the server. */
  calls: number
  used_by: number
  last_seen: string
  /** Slug of the tenant connector this traffic belongs to, "" when none. */
  registered_connector_slug: string
  via_gateway: boolean
}

export async function fetchSkillsUsage(period: Period): Promise<DiscoveredSkill[] | null> {
  try {
    const res = await apiFetch<{ skills: DiscoveredSkill[] | null }>(
      `/analytics/skills/usage?${periodQuery(period)}`,
    )
    return res.skills ?? []
  } catch {
    return null
  }
}

export async function fetchMcpsUsage(period: Period): Promise<DiscoveredMcp[] | null> {
  try {
    const res = await apiFetch<{ servers: DiscoveredMcp[] | null }>(
      `/analytics/mcps/usage?${periodQuery(period)}`,
    )
    return (res.servers ?? []).map((s) => ({ ...s, tools: s.tools ?? [] }))
  } catch {
    return null
  }
}
