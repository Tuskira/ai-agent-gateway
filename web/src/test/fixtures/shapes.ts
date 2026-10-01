import { expect } from 'vitest'
import type {
  ModelLimits,
  ModelPrice,
  ModelsHighestTraffic,
  ModelsSummary,
  ModelTarget,
  ModelUsageRow,
  RegisteredModel,
} from '@/lib/models'

/**
 * The console's types as key lists, to hold JSON that came from the API to
 * them. Each `*_KEYS` object must list exactly the keys of its type --
 * `satisfies Record<keyof T, ...>` makes a key missing here, or one the type
 * does not have, a compile error -- and says whether the API always sends
 * the key. So the types cannot drift from the API in either direction
 * without a test failing.
 */
type Presence = 'always' | 'optional'

const REGISTERED_MODEL_KEYS = {
  id: 'always',
  name: 'always',
  description: 'always',
  enabled: 'always',
  targets: 'always',
  price: 'always',
  limits: 'always',
  scope: 'always',
  metadata: 'always',
  created_at: 'always',
  updated_at: 'always',
} satisfies Record<keyof RegisteredModel, Presence>

const TARGET_KEYS = {
  vendor: 'always',
  model: 'always',
  base_url: 'optional',
  credential: 'optional',
  region: 'optional',
  allow_caller_key: 'optional',
  label: 'optional',
} satisfies Record<keyof ModelTarget, Presence>

const PRICE_KEYS = {
  input: 'always',
  output: 'always',
  cache_read: 'optional',
  cache_write: 'optional',
} satisfies Record<keyof ModelPrice, Presence>

const LIMITS_KEYS = {
  daily_usd: 'optional',
  monthly_usd: 'optional',
  rpm: 'optional',
  max_tokens: 'optional',
} satisfies Record<keyof ModelLimits, Presence>

const USAGE_ROW_KEYS = {
  name: 'always',
  provider: 'always',
  calls: 'always',
  tokens: 'always',
  cost_usd: 'always',
  used_by: 'always',
  last_seen: 'always',
  status: 'always',
} satisfies Record<keyof ModelUsageRow, Presence>

const SUMMARY_KEYS = {
  range: 'always',
  models: 'always',
  total_models: 'always',
  highest_traffic: 'always',
} satisfies Record<keyof ModelsSummary, Presence>

const HIGHEST_TRAFFIC_KEYS = {
  name: 'always',
  tokens: 'always',
} satisfies Record<keyof ModelsHighestTraffic, Presence>

export function expectShape(what: string, value: object, keys: Record<string, Presence>) {
  const got = Object.keys(value)
  const unknown = got.filter((k) => !(k in keys))
  const missing = Object.entries(keys)
    .filter(([k, presence]) => presence === 'always' && !got.includes(k))
    .map(([k]) => k)
  expect({ what, unknown, missing }).toEqual({ what, unknown: [], missing: [] })
}

export function expectModelShape(what: string, m: RegisteredModel) {
  expectShape(what, m, REGISTERED_MODEL_KEYS)
  expect(['tenant', 'platform']).toContain(m.scope)
  expect(m.targets.length).toBeGreaterThan(0)
  m.targets.forEach((t, i) => expectShape(`${what}.targets[${i}]`, t, TARGET_KEYS))
  if (m.price !== null) expectShape(`${what}.price`, m.price, PRICE_KEYS)
  if (m.limits !== null) expectShape(`${what}.limits`, m.limits, LIMITS_KEYS)
}

export function expectSummaryShape(what: string, summary: ModelsSummary) {
  expectShape(what, summary, SUMMARY_KEYS)
  summary.models.forEach((row, i) =>
    expectShape(`${what}.models[${i}]`, row, USAGE_ROW_KEYS),
  )
  if (summary.highest_traffic !== null) {
    expectShape(`${what}.highest_traffic`, summary.highest_traffic, HIGHEST_TRAFFIC_KEYS)
  }
}
