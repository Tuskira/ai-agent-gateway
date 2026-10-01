import { apiFetch, ApiError } from '@/lib/api'

/**
 * Data contract for the Overview page.
 *
 * Two kinds of loaders live here:
 *
 * 1. Real today — thin wrappers around endpoints the gateway already
 *    serves (`/connectors`, `/profiles`, `/api-keys`).
 * 2. Not available yet — `OverviewMetrics` describes the shape the
 *    analytics service will return once it ships alongside the
 *    ClickHouse sink. `fetchOverviewMetrics` calls it defensively and
 *    resolves to `null` on any failure (404 because the route doesn't
 *    exist yet, or a network error) — callers render the full mock
 *    layout with honest empty states rather than throwing.
 */

export type TimeRange = '24h' | '7d' | '30d'

export const TIME_RANGES: { value: TimeRange; label: string }[] = [
  { value: '24h', label: 'Last 24h' },
  { value: '7d', label: 'Last 7d' },
  { value: '30d', label: 'Last 30d' },
]

interface Page<T> {
  items: T[]
  total: number
}

/* ---------------------------------------------------------------------- */
/* Real endpoints                                                          */
/* ---------------------------------------------------------------------- */

export type ConnectorStatus = 'healthy' | 'unhealthy' | 'unknown' | string

export interface ConnectorSummary {
  id: string
  name: string
  slug: string
  status: ConnectorStatus
}

export interface ProfileSummary {
  id: string
  name: string
  slug: string
}

export interface ApiKeySummary {
  id: string
  name: string
  role: string
  prefix: string
}

/** `GET /api/v1/connectors` */
export function fetchConnectors(): Promise<Page<ConnectorSummary>> {
  return apiFetch<Page<ConnectorSummary>>('/connectors')
}

/** `GET /api/v1/profiles` */
export function fetchProfiles(): Promise<Page<ProfileSummary>> {
  return apiFetch<Page<ProfileSummary>>('/profiles')
}

/**
 * `GET /api/v1/api-keys?limit=500` (the server max, so log rows can resolve
 * any key's name) — admin-only. A 403 (caller signed in with a
 * non-admin key) is expected, not exceptional: resolve to `null` so the
 * page can simply omit anything gated on it.
 */
export async function fetchApiKeys(): Promise<Page<ApiKeySummary> | null> {
  try {
    return await apiFetch<Page<ApiKeySummary>>('/api-keys?limit=500')
  } catch (err) {
    if (err instanceof ApiError && err.status === 403) return null
    throw err
  }
}

/** `GET /api/v1/connectors/{id}/tools` — returns only the total count. */
export async function fetchConnectorToolCount(connectorId: string): Promise<number> {
  const page = await apiFetch<Page<unknown>>(
    `/connectors/${encodeURIComponent(connectorId)}/tools`,
  )
  return page.total
}

/** `GET /api/v1/profiles/{id}/tools` — returns only the total count. */
export async function fetchProfileToolCount(profileId: string): Promise<number> {
  const page = await apiFetch<Page<unknown>>(
    `/profiles/${encodeURIComponent(profileId)}/tools`,
  )
  return page.total
}

/** At most this many connectors/profiles get a follow-up tools request. */
export const MAX_TOOL_COUNT_REQUESTS = 6

/* ---------------------------------------------------------------------- */
/* Analytics endpoint (not deployed yet — types describe the future shape) */
/* ---------------------------------------------------------------------- */

export interface KpiDelta {
  /** Signed percent change vs. the previous period, e.g. -7.38 or 47.33. */
  pct: number
  direction: 'up' | 'down'
}

export interface CountKpi {
  value: number
  delta: KpiDelta
}

export interface OverviewKpis {
  llmAgentCalls: CountKpi
  mcpToolCalls: CountKpi
  totalTokens: CountKpi
  totalCost: CountKpi
  successRate: {
    value: number
    /** e.g. "200 / 204 responses" */
    sub: string
  }
}

export interface LlmUsageRow {
  model: string
  calls: number
  tokens: number
  cost: number
}

export interface NamedCount {
  name: string
  count: number
}

export interface StatusCodeSlice {
  label: string
  pct: number
}

export interface SlowCall {
  name: string
  ms: number
}

export interface TrafficPoint {
  /** X-axis tick label, e.g. "9PM" */
  label: string
  llmCalls: number
  mcpCalls: number
}

export interface OverviewMetrics {
  range: TimeRange
  kpis: OverviewKpis
  llmUsage: LlmUsageRow[]
  traffic: { llmCalls: number; mcpCalls: number }
  /**
   * Always true: every dollar figure above (kpis.totalCost, llmUsage[].cost)
   * comes from the gateway's rate card, not a provider-reported invoice.
   * Optional so the page tolerates an older gateway that doesn't send it yet.
   */
  costEstimated?: boolean
  /**
   * Which rate card the running gateway is costing calls from: the
   * bundled default, or an operator override file
   * (`llm_proxy.pricing_file`). Optional for the same reason as
   * `costEstimated`.
   */
  pricingSource?: 'embedded' | 'file'
  topAgents: NamedCount[]
  statusCodes: StatusCodeSlice[]
  successPct: number
  latency: {
    medianMs: number
    p95Ms: number
    slowest: SlowCall[]
  }
  requestsByClient: NamedCount[]
  mcpTools: NamedCount[]
  topConnectors: NamedCount[]
  trafficOverTime: TrafficPoint[]
}

/**
 * `GET /api/v1/analytics/overview?range=24h`.
 *
 * The analytics service lands with the ClickHouse sink and isn't deployed
 * yet, so any failure reaching it — a 404 today, a network error, or
 * anything else — resolves to `null`. The page never fabricates numbers:
 * `null` means "render the mock's layout with empty states," not "show a
 * fetch error."
 */
export async function fetchOverviewMetrics(
  range: TimeRange,
): Promise<OverviewMetrics | null> {
  try {
    return await apiFetch<OverviewMetrics>(`/analytics/overview?range=${range}`)
  } catch {
    return null
  }
}

/* ---------------------------------------------------------------------- */
/* Formatting helpers                                                      */
/* ---------------------------------------------------------------------- */

/** "602" under 1000, "515.49K" / "1.52M" above — matches the mock's precision. */
export function formatCompactNumber(n: number): string {
  const abs = Math.abs(n)
  if (abs < 1000) return Math.round(n).toString()
  const units: { value: number; suffix: string }[] = [
    { value: 1e9, suffix: 'B' },
    { value: 1e6, suffix: 'M' },
    { value: 1e3, suffix: 'K' },
  ]
  for (const u of units) {
    if (abs >= u.value) {
      return (n / u.value).toFixed(2) + u.suffix
    }
  }
  return n.toString()
}

/** "$11.00" */
export function formatCurrency(n: number): string {
  return `$${n.toFixed(2)}`
}

/** "97.5%" */
export function formatPercent(n: number, digits = 1): string {
  return `${n.toFixed(digits)}%`
}

/** "7.38%" / "1%" — rounds to 2dp, trailing zeros drop naturally. */
export function formatDeltaPct(pct: number): string {
  return `${Math.round(Math.abs(pct) * 100) / 100}%`
}

/** "487ms" */
export function formatMs(n: number): string {
  return `${Math.round(n)}ms`
}

/**
 * A duration at the precision a headline can carry: "906ms", "1.37s",
 * "24.1s", "2m 5s". Tables that list exact timings keep `formatMs`. Anything
 * that is not a usable duration reads as an em dash, never a made-up number.
 */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—'
  const rounded = Math.round(ms)
  if (rounded < 1000) return `${rounded}ms`
  const seconds = ms / 1000
  if (seconds < 9.995) return `${seconds.toFixed(2)}s`
  if (seconds < 59.95) return `${seconds.toFixed(1)}s`
  const whole = Math.round(seconds)
  return `${Math.floor(whole / 60)}m ${whole % 60}s`
}

/**
 * How many times `base` goes into `value`, for captions such as
 * "200× median": one decimal below ten, whole numbers above. `null` when the
 * comparison would mean nothing.
 */
export function formatTimes(value: number, base: number): string | null {
  if (!Number.isFinite(value) || !Number.isFinite(base) || base <= 0 || value < 0) {
    return null
  }
  const times = value / base
  return `${times < 10 ? times.toFixed(1) : Math.round(times).toLocaleString()}×`
}

/** Bar width as a percent of the largest value in the set, matching the
 * mock's own `Math.max(1.5, c / mx * 100)` (a value is never invisible). */
export function pctOfMax(value: number, max: number): number {
  if (max <= 0) return 0
  return Math.max(1.5, (value / max) * 100)
}

const CHART_COLORS = [
  'var(--chart-1)',
  'var(--chart-2)',
  'var(--chart-3)',
  'var(--chart-4)',
  'var(--chart-5)',
] as const

/** The theme's chart color for the row at `index`, cycling through all
 * five so neighbouring rows in a list never share one. */
export function chartColor(index: number): string {
  return CHART_COLORS[index % CHART_COLORS.length]!
}
