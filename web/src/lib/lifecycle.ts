import type { PillTone } from '@/components/app/StatusPill'
import type { SkillUsageRow } from '@/lib/analytics'
import type { DiscoveredMcp, DiscoveredSkill } from '@/lib/discovery'
import type { Connector } from '@/lib/connectors'
import type { CatalogEntry } from '@/lib/mcpCatalog'
import type { Skill } from '@/lib/skills'

/**
 * One shared vocabulary for "where is this thing in its life" across the
 * registry pages (Skills, MCPs, Models):
 *
 * - available:  in the platform catalog, this tenant has not set it up. Not
 *               callable. Action: Set up / Add.
 * - registered: a tenant row, enabled, with no traffic in the selected range.
 * - active:     a tenant row with traffic in the selected range.
 * - disabled:   a tenant row with enabled=false. Wins over traffic: a
 *               disabled row that still sees calls is Disabled, not Active.
 * - discovered: seen in traffic with no registry row. Read-only; the only
 *               action is Register / Add.
 *
 * With analytics off there is no traffic, so only registered, disabled and
 * available can occur and discovered is never produced.
 */
export type LifecycleState = 'available' | 'registered' | 'active' | 'disabled' | 'discovered'

export const LIFECYCLE_STATES: LifecycleState[] = [
  'active',
  'registered',
  'disabled',
  'discovered',
  'available',
]

const LABELS: Record<LifecycleState, string> = {
  available: 'Available',
  registered: 'Registered',
  active: 'Active',
  disabled: 'Disabled',
  discovered: 'Discovered',
}

const TONES: Record<LifecycleState, PillTone> = {
  active: 'positive',
  registered: 'info',
  disabled: 'neutral',
  discovered: 'neutral',
  available: 'accent',
}

const TITLES: Record<LifecycleState, string> = {
  available: 'In the catalog. Not set up for this tenant yet, so it cannot be called.',
  registered: 'Set up and enabled, with no traffic in the selected range.',
  active: 'Set up and enabled, with traffic in the selected range.',
  disabled: 'Turned off. It is not callable, whatever traffic it still receives.',
  discovered: 'Seen in traffic but not registered. Register it to manage it here.',
}

export function lifecycleLabel(state: LifecycleState): string {
  return LABELS[state]
}

export function lifecycleTone(state: LifecycleState): PillTone {
  return TONES[state]
}

export function lifecycleTitle(state: LifecycleState): string {
  return TITLES[state]
}

/** Discovered and Available rows are not this tenant's: drawn as a dashed
 * outline rather than a filled pill. */
export function lifecycleOutline(state: LifecycleState): boolean {
  return state === 'discovered' || state === 'available'
}

/** State of a tenant registry row. `calls` is the traffic seen in the
 * selected range (0 or undefined when analytics is off). Disabled wins. */
export function deriveRegistryState(enabled: boolean, calls: number | undefined): LifecycleState {
  if (!enabled) return 'disabled'
  return (calls ?? 0) > 0 ? 'active' : 'registered'
}

/* ------------------------------------------------------------------ */
/* Skills                                                              */
/* ------------------------------------------------------------------ */

export interface SkillLifecycleRow {
  key: string
  name: string
  state: LifecycleState
  /** The registry row; null for a Discovered name. */
  skill: Skill | null
  calls: number
  used_by: number
  last_seen: string | null
}

/**
 * Merges the skill/command registry with observed usage into one row set.
 *
 * `discovery` is `GET /analytics/skills/usage` (what the model asked to use)
 * and is the single source of calls / used_by / last_seen per name. A
 * registered row the model never asked for falls back to `summary`
 * (`GET /analytics/skills`, gateway-served loads and commands) so a native
 * command with only prompts/get traffic still reads Active; the two are never
 * summed. `discovery === null` means analytics is off: no traffic, and no
 * Discovered rows.
 */
export function mergeSkillRows(
  skills: Skill[],
  discovery: DiscoveredSkill[] | null,
  summary: SkillUsageRow[] | null,
): SkillLifecycleRow[] {
  const seen = new Map<string, DiscoveredSkill>()
  for (const d of discovery ?? []) seen.set(d.name, d)
  const served = new Map<string, SkillUsageRow>()
  for (const s of summary ?? []) served.set(s.name, s)
  const analyticsOn = discovery !== null

  const rows: SkillLifecycleRow[] = skills.map((skill) => {
    const u = seen.get(skill.name) ?? served.get(skill.name)
    const calls = analyticsOn ? (u?.calls ?? 0) : 0
    return {
      key: skill.id,
      name: skill.name,
      state: deriveRegistryState(skill.enabled, calls),
      skill,
      calls,
      used_by: analyticsOn ? (u?.used_by ?? 0) : 0,
      last_seen: analyticsOn ? (u?.last_seen ?? null) : null,
    }
  })

  if (analyticsOn) {
    const registered = new Set(skills.map((s) => s.name))
    for (const d of discovery ?? []) {
      if (registered.has(d.name) || d.registered) continue
      rows.push({
        key: `discovered:${d.name}`,
        name: d.name,
        state: 'discovered',
        skill: null,
        calls: d.calls,
        used_by: d.used_by,
        last_seen: d.last_seen,
      })
    }
  }
  return rows
}

/* ------------------------------------------------------------------ */
/* MCPs                                                                */
/* ------------------------------------------------------------------ */

export interface McpLifecycleRow {
  key: string
  /** Display name: the connector's name, or the observed server name. */
  name: string
  state: LifecycleState
  /** The tenant connector; null for a Discovered server. */
  connector: Connector | null
  /** Discovered only: observed tool names. */
  tools: string[]
  calls: number
  used_by: number
  last_seen: string | null
  /** Some of this server's calls went through the gateway's own MCP plane. */
  via_gateway: boolean
  /** Some of this server's calls reached it directly (not via the gateway). */
  direct: boolean
  /** Discovered only: the catalog entry matching the server name, if any. */
  catalogMatch: CatalogEntry | null
}

export function connectorEnabled(c: Pick<Connector, 'metadata'>): boolean {
  return c.metadata?.enabled !== false
}

function norm(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9]/g, '')
}

/** The catalog entry a discovered server name corresponds to (by slug or name,
 * ignoring case and punctuation), or null. Entries that cannot be added
 * (already added, disabled, unsupported) never match: Add MCP takes over. */
export function matchCatalogEntry(server: string, entries: CatalogEntry[]): CatalogEntry | null {
  const want = norm(server)
  if (want === '') return null
  return (
    entries.find(
      (e) =>
        !e.added && e.supported && e.enabled && (norm(e.slug) === want || norm(e.name) === want),
    ) ?? null
  )
}

/**
 * Merges tenant connectors with `GET /analytics/mcps/usage`. A connector is
 * Active when any usage row attributes calls to its slug, directly or through
 * the gateway (via_gateway). The two row kinds are disjoint tool sets, so their
 * calls are added; a single LLM call using tools both ways would count twice.
 * `usage === null` means analytics is off.
 */
export function mergeMcpRows(
  connectors: Connector[],
  usage: DiscoveredMcp[] | null,
  catalog: CatalogEntry[],
): McpLifecycleRow[] {
  const analyticsOn = usage !== null
  const bySlug = new Map<
    string,
    { calls: number; keys: number; last: string | null; via: boolean; direct: boolean }
  >()
  for (const u of usage ?? []) {
    const slug = u.registered_connector_slug?.toLowerCase()
    if (!slug) continue
    const cur = bySlug.get(slug) ?? { calls: 0, keys: 0, last: null, via: false, direct: false }
    cur.calls += u.calls
    if (u.via_gateway) cur.via = true
    else cur.direct = true
    cur.keys = Math.max(cur.keys, u.used_by)
    if (!cur.last || u.last_seen > cur.last) cur.last = u.last_seen
    bySlug.set(slug, cur)
  }

  const rows: McpLifecycleRow[] = connectors.map((c) => {
    const u = analyticsOn ? bySlug.get(c.slug.toLowerCase()) : undefined
    const calls = u?.calls ?? 0
    return {
      key: c.id,
      name: c.name,
      state: deriveRegistryState(connectorEnabled(c), calls),
      connector: c,
      tools: [],
      calls,
      used_by: u?.keys ?? 0,
      last_seen: u?.last ?? null,
      via_gateway: u?.via ?? false,
      direct: u?.direct ?? false,
      catalogMatch: null,
    }
  })

  for (const u of usage ?? []) {
    if (u.registered_connector_slug) continue
    rows.push({
      key: `discovered:${u.server}`,
      name: u.server,
      state: 'discovered',
      connector: null,
      tools: u.tools,
      calls: u.calls,
      used_by: u.used_by,
      last_seen: u.last_seen,
      via_gateway: u.via_gateway,
      direct: !u.via_gateway,
      catalogMatch: matchCatalogEntry(u.server, catalog),
    })
  }
  return rows
}

/** State of a catalog card: Available until added, then the connector's own
 * state (falling back to Registered when it is not in `byConnectorId`). */
export function catalogEntryState(
  entry: Pick<CatalogEntry, 'added' | 'connector_id'>,
  byConnectorId: ReadonlyMap<string, LifecycleState> | undefined,
): LifecycleState {
  if (!entry.added) return 'available'
  return (entry.connector_id && byConnectorId?.get(entry.connector_id)) || 'registered'
}
