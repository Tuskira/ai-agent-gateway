import { ApiError, apiFetch } from '@/lib/api'
import { formatCompactNumber } from '@/lib/overview'

/**
 * Token Monitoring — `GET /api/v1/analytics/token-monitoring*`
 * (`internal/api/handlers/analytics_monitoring.go`). Every figure comes from
 * the ClickHouse `llm_usage_canonical` view: tokens = input + output, cache
 * reads/writes reported alongside, cost from the gateway's price table
 * (`null` = no priced call in the window). Snake-case, like the API.
 */

export type MonitoringWindow = 'today' | '7d' | '30d'

export const MONITORING_WINDOWS: { value: MonitoringWindow; label: string }[] = [
  { value: 'today', label: 'Today' },
  { value: '7d', label: 'Last 7 Days' },
  { value: '30d', label: 'Last 30 Days' },
]

export type CallerRole = 'agent' | 'admin' | 'interceptor' | 'other'

export const ROLE_LABEL: Record<CallerRole, string> = {
  agent: 'Agent',
  admin: 'Admin',
  interceptor: 'Interceptor',
  other: 'Other',
}

export const NO_USAGE_LABEL = 'No LLM usage in this window yet'

/** "14:00" for an hourly bucket, "Oct 5" for a daily one, in UTC. */
export function bucketLabel(bucket: string, granularity: 'hour' | 'day'): string {
  const d = new Date(bucket)
  return granularity === 'hour'
    ? `${String(d.getUTCHours()).padStart(2, '0')}:00`
    : d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' })
}

export interface TokenUsage {
  tokens: number
  prompt_tokens: number
  completion_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  cost_usd: number | null
  calls: number
  unpriced_calls: number
  prev_tokens: number
}

export interface TokenTotals extends TokenUsage {
  /** Change vs the previous period in percent; `null` when it had none. */
  delta_pct: number | null
}

export interface ModelTokens extends TokenTotals {
  model: string
}

export interface CallerTokens extends TokenTotals {
  key_id: string
  source: string
  models: number
  /** API key name; '' when the key is unknown (deleted, or no key). */
  name: string
  role: CallerRole
}

export interface RoleTokens {
  role: CallerRole
  tokens: number
  calls: number
}

export interface TokenBucket {
  bucket: string
  tokens: number
}

export interface SessionTokens extends TokenUsage {
  session_id: string
  key_id: string
  key_name: string
  role: CallerRole
  models: string[]
  last_seen: string
}

/** The page (by model, caller and role) or one drill-down: a model
 * (`model`, by caller) or an API key (`key`, by model), with its sessions. */
export interface TokenMonitoring {
  window: MonitoringWindow
  start: string
  end: string
  granularity: 'hour' | 'day'
  model?: string
  key?: { id: string; name: string; role: CallerRole }
  totals: TokenTotals
  by_model: ModelTokens[]
  by_key: CallerTokens[]
  /** The page only. */
  by_role?: RoleTokens[]
  burn: TokenBucket[]
  /** Drill-downs only; omitted when there are none. */
  sessions?: SessionTokens[]
  sessions_total?: number
}

/** `null` when analytics are off (no ClickHouse sink: 404). */
async function orNull(path: string): Promise<TokenMonitoring | null> {
  try {
    return await apiFetch<TokenMonitoring>(path)
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return null
    throw err
  }
}

export function fetchTokenMonitoring(window: MonitoringWindow) {
  return orNull(`/analytics/token-monitoring?window=${window}`)
}

export function fetchTokenMonitoringModel(
  model: string,
  window: MonitoringWindow,
  limit: number,
  offset: number,
) {
  const q = new URLSearchParams({
    model,
    window,
    limit: String(limit),
    offset: String(offset),
  })
  return orNull(`/analytics/token-monitoring/model?${q.toString()}`)
}

export function fetchTokenMonitoringKey(
  keyId: string,
  window: MonitoringWindow,
  limit: number,
  offset: number,
) {
  const q = new URLSearchParams({ window, limit: String(limit), offset: String(offset) })
  return orNull(
    `/analytics/token-monitoring/keys/${encodeURIComponent(keyId)}?${q.toString()}`,
  )
}

/** A caller's display name: its key's name, else why it has none. */
export function callerLabel(c: Pick<CallerTokens, 'name' | 'key_id'>): string {
  return c.name || (c.key_id ? 'Unknown key' : 'No key')
}

/** RankedBarList rows by tokens, for models and for callers. */
export function modelBars(models: ModelTokens[]) {
  return models.map((m) => ({
    key: m.model,
    label: m.model,
    value: m.tokens,
    displayValue: formatCompactNumber(m.tokens),
  }))
}

export function callerBars(callers: CallerTokens[]) {
  return callers.map((c) => ({
    key: c.key_id || 'none',
    label: callerLabel(c),
    value: c.tokens,
    displayValue: formatCompactNumber(c.tokens),
  }))
}

/* ---------------------------------------------------------------------- */
/* Formatting: a real non-zero value is never shown as zero.               */
/* ---------------------------------------------------------------------- */

const usd = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD' })

/** "$4.12" from one cent up; below a cent, two significant digits
 * ("$0.0000097") so a tiny cost never reads as "$0.00"; "—" when unknown. */
export function formatTokenCost(cost: number | null): string {
  if (cost === null || !Number.isFinite(cost)) return '—'
  if (cost === 0 || cost >= 0.01) return usd.format(cost)
  const digits = Math.min(20, 1 - Math.floor(Math.log10(cost)))
  return `$${cost.toFixed(digits)}`
}

/** part as a share of total: "88.5%", "<0.1%" for a tiny non-zero part. */
export function formatShare(part: number, total: number): string {
  if (total <= 0) return '—'
  if (part <= 0) return '0%'
  const pct = (part / total) * 100
  if (pct < 0.1) return '<0.1%'
  return `${Number(pct.toFixed(1))}%`
}

export interface SplitPart {
  key: 'in' | 'out' | 'cr' | 'cw'
  name: string
  value: number
  label: string
}

/** Input / output / cache read / cache write as shares of all four. */
export function tokenSplit(
  u: Pick<
    TokenUsage,
    'prompt_tokens' | 'completion_tokens' | 'cache_read_tokens' | 'cache_write_tokens'
  >,
): SplitPart[] {
  const parts: Omit<SplitPart, 'label'>[] = [
    { key: 'in', name: 'In', value: u.prompt_tokens },
    { key: 'out', name: 'Out', value: u.completion_tokens },
    { key: 'cr', name: 'Cache read', value: u.cache_read_tokens },
    { key: 'cw', name: 'Cache write', value: u.cache_write_tokens },
  ]
  const total = parts.reduce((s, p) => s + p.value, 0)
  return parts.map((p) => ({ ...p, label: formatShare(p.value, total) }))
}
