import { apiFetch } from '@/lib/api'
import type { TimeRange } from '@/lib/overview'

/**
 * Data contract for the Models page (`/models`) and its Add/Edit dialog.
 *
 * Two APIs feed this page:
 *
 * 1. The model registry (`/api/v1/models`,
 *    `internal/api/handlers/models.go`). The shapes below are pinned to
 *    that handler's real output by a shared fixture,
 *    `src/test/fixtures/models-api.json`: a Go test
 *    (`TestModels_ConsoleContract`) replays the fixture's requests through
 *    the handler and compares its responses with the fixture's, and
 *    `models.contract.test.ts` checks the same fixture against these types
 *    and against what the dialog sends.
 * 2. The traffic summary (`GET /api/v1/analytics/models`, real today —
 *    see `internal/api/handlers/analytics.go`), which needs the
 *    ClickHouse sink the same way `/analytics/overview` does.
 *
 * `mergeModelRows` combines the two: a registered model with no traffic
 * still shows up (0 calls, status "registered"/"disabled"); traffic for a
 * model nobody registered still shows up too (the gateway forwards
 * unregistered names byte-for-byte — see the contract's "Goal") as status
 * "discovered". See `MergedModelStatus` for all four states.
 */

interface Page<T> {
  items: T[]
  total: number
}

/* ---------------------------------------------------------------------- */
/* Model registry (`/api/v1/models`, Phase 1)                              */
/* ---------------------------------------------------------------------- */

export type ModelVendor = 'anthropic' | 'bedrock' | 'openai_compat' | 'gemini'

export const MODEL_VENDORS: { value: ModelVendor; label: string }[] = [
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'bedrock', label: 'Amazon Bedrock' },
  { value: 'openai_compat', label: 'OpenAI-compatible' },
  { value: 'gemini', label: 'Google Gemini' },
]

/** OpenAI-compatible providers the model form offers by name. Each is an
 * `openai_compat` target with a fixed `label` (`tag`) and a default
 * `base_url`: the API root the gateway appends `/chat/completions` to when
 * it translates an Anthropic-format call. "Google Gemini" is Google's
 * OpenAI-compatible endpoint, the one Anthropic clients can reach; its
 * value differs from the native `gemini` vendor's. */
export const PROVIDER_PRESETS = [
  {
    value: 'google_gemini',
    label: 'Google Gemini',
    tag: 'gemini',
    baseUrl: 'https://generativelanguage.googleapis.com/v1beta/openai/',
  },
  {
    value: 'openai',
    label: 'OpenAI',
    tag: 'openai',
    baseUrl: 'https://api.openai.com/v1',
  },
  {
    value: 'nebius',
    label: 'Nebius',
    tag: 'nebius',
    baseUrl: 'https://api.tokenfactory.eu-west2.nebius.com/v1/',
  },
  {
    value: 'together',
    label: 'Together AI',
    tag: 'together',
    baseUrl: 'https://api.together.ai/v1',
  },
] as const

export type ProviderPreset = (typeof PROVIDER_PRESETS)[number]['value']

/** One entry in a model's ordered target list — where a call actually
 * goes. Order matters: the LLM plane tries targets in order, falling back
 * to the next one on a dial/TLS/5xx failure before any response byte
 * reaches the client. */
export interface ModelTarget {
  vendor: ModelVendor
  /** The vendor's own model id, e.g. "claude-sonnet-4-5-20250929". */
  model: string
  /** Required for `openai_compat`; optional for `anthropic`/`gemini` (the
   * gateway's configured default host otherwise); rejected for `bedrock`.
   * `https`, or `http` for a loopback host or `host.docker.internal` only. */
  base_url?: string
  /** A credential name from `GET /api/v1/credentials` — never a value. */
  credential?: string
  /** Required for `bedrock`; rejected for any other vendor. */
  region?: string
  /** Lets a target WITHOUT a credential receive the caller's own vendor
   * key even though it is not on the gateway's default host for the
   * vendor. Cannot be combined with `credential` (the API answers 400).
   * Omitted by the API when false. */
  allow_caller_key?: boolean
  /** Names the vendor behind an `openai_compat` target ("groq",
   * "deepseek", "ollama", ...). It is what the traffic columns show as
   * the provider, what the call is priced as, and the suffix of the
   * header a caller sends its key for this vendor in
   * (`X-Provider-Key-<label>`). `openai_compat` only; omitted by the API
   * when unset. */
  label?: string
}

/** `[a-z0-9._-]{1,32}` -- the registry's own rule for a target label. */
export const MODEL_LABEL_PATTERN = /^[a-z0-9._-]{1,32}$/

/** Labels the registry refuses: the gateway's native wires own them. */
export const RESERVED_MODEL_LABELS: readonly string[] = ['anthropic', 'bedrock']

/** Optional per-1M-token USD pricing that replaces `pkg/pricing`'s rate
 * card for this model name. It is a flat override, not a patch: the API
 * stores `input` and `output` as given (a missing one is 0, i.e. free),
 * so the dialog requires both whenever any price field is set. */
export interface ModelPrice {
  input: number
  output: number
  cache_read?: number
  cache_write?: number
}

/** Budgets and caps on a model name, per tenant — the same object an API
 * key carries (`docs/llm-plane.md`, "Model-level limits"). Every field is
 * optional, but a `limits` object must set at least one; `0` is a real
 * limit (it blocks). */
export interface ModelLimits {
  daily_usd?: number
  monthly_usd?: number
  /** Integer, 0–100000. */
  rpm?: number
  /** Integer ≥ 0. */
  max_tokens?: number
}

export const MODEL_MAX_RPM = 100000

export type ModelScope = 'tenant' | 'platform'

/** A model as the API returns it. Every key is always present: `price`
 * and `limits` are `null` when unset, `description` is `""`, `metadata`
 * is `{}`. */
export interface RegisteredModel {
  id: string
  name: string
  description: string
  enabled: boolean
  targets: ModelTarget[]
  price: ModelPrice | null
  limits: ModelLimits | null
  /** "platform" = a tenant-id-NULL default visible to every tenant,
   * read-only in the console; "tenant" = this tenant's own row. */
  scope: ModelScope
  metadata: Record<string, unknown>
  created_at: string
  updated_at: string
}

/** `POST`/`PUT /api/v1/models` body. PUT replaces the row: `price` and
 * `limits` left out are cleared (`metadata` left out is kept). */
export interface ModelInput {
  name: string
  description?: string
  enabled?: boolean
  targets: ModelTarget[]
  price?: ModelPrice
  limits?: ModelLimits
  metadata?: Record<string, unknown>
}

/** `[a-z0-9._-]{1,64}` — matches the registry's own validation, checked
 * client-side too so the form fails fast instead of round-tripping a 400. */
export const MODEL_NAME_PATTERN = /^[a-z0-9._-]{1,64}$/

/** `GET /api/v1/models` */
export function listRegisteredModels(): Promise<Page<RegisteredModel>> {
  return apiFetch<Page<RegisteredModel>>('/models')
}

/** `POST /api/v1/models` */
export function createModel(input: ModelInput): Promise<RegisteredModel> {
  return apiFetch<RegisteredModel>('/models', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

/** `PUT /api/v1/models/{id}` — tenant-scope rows only; the registry 403s on
 * a platform row (see `RegisteredModel.scope`). */
export function updateModel(id: string, input: ModelInput): Promise<RegisteredModel> {
  return apiFetch<RegisteredModel>(`/models/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

/** `DELETE /api/v1/models/{id}` (soft delete). */
export function deleteModel(id: string): Promise<void> {
  return apiFetch<void>(`/models/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/* ---------------------------------------------------------------------- */
/* Traffic summary (`GET /api/v1/analytics/models`, real today)            */
/* ---------------------------------------------------------------------- */

/** One model's row from observed gateway traffic. Field names are
 * snake_case (unlike `overview.ts`'s camelCase) — this route follows the
 * gateway's normal REST convention; see
 * `pkg/analytics.ModelSummaryRow`'s doc comment. */
export interface ModelUsageRow {
  name: string
  provider: string
  calls: number
  tokens: number
  /** `null` when none of this model's calls in the window had a known
   * price — never a fabricated 0. */
  cost_usd: number | null
  /** Distinct API keys (`key_id`) that called this model in the window. */
  used_by: number
  last_seen: string
  /** Always `"active"` from the server — every row here is observed
   * traffic. `mergeModelRows` turns this into `"active"` (registered and
   * seen) or `"discovered"` (seen, no registry record), and adds
   * `"registered"`/`"disabled"` for registry-only rows that saw no
   * traffic. */
  status: string
}

export interface ModelsHighestTraffic {
  name: string
  tokens: number
}

export interface ModelsSummary {
  range: TimeRange
  models: ModelUsageRow[]
  total_models: number
  highest_traffic: ModelsHighestTraffic | null
}

/** The range the Models page always asks for — its footer reads "Observed
 * from gateway traffic over the last 7 days", so there's no range picker
 * on this page (unlike Overview/LLM Logs). */
export const MODELS_TRAFFIC_RANGE: TimeRange = '7d'

/**
 * `GET /api/v1/analytics/models?range=7d`. Like `fetchOverviewMetrics`,
 * any failure (404 because ClickHouse isn't enabled, or a network error)
 * resolves to `null` rather than throwing — the page renders its
 * "analytics are off" state instead of an error screen.
 */
export async function fetchModelsSummary(
  range: TimeRange = MODELS_TRAFFIC_RANGE,
): Promise<ModelsSummary | null> {
  try {
    return await apiFetch<ModelsSummary>(`/analytics/models?range=${range}`)
  } catch {
    return null
  }
}

/* ---------------------------------------------------------------------- */
/* Merge                                                                   */
/* ---------------------------------------------------------------------- */

/**
 * - "active": registered and seen in traffic.
 * - "registered": registered, enabled, not seen in traffic.
 * - "disabled": registered, disabled, not seen in traffic. (A disabled
 *   model that IS seen in traffic still reports "active" -- the registry
 *   check happens at call time, not in this summary. See `mergeModelRows`.)
 * - "discovered": seen in traffic, no registry record at all -- the
 *   gateway forwards an unregistered name byte-for-byte.
 * - "available": a Model Catalog model (`lib/model-catalog.ts`) this tenant
 *   hasn't set up yet -- no registry record, no traffic. Added to the row
 *   set by `mergeModelCatalogRows`, not by `mergeModelRows` itself (the
 *   catalog is a separate, optional fetch -- see that function's doc
 *   comment).
 */
export type MergedModelStatus = 'active' | 'registered' | 'disabled' | 'discovered' | 'available'

/** Points a row at the Model Catalog entry the "Set up" row action should
 * open the Connect dialog to. Set by `mergeModelCatalogRows` on an
 * "available" row, and on a "discovered" row whose name or model id
 * matches a catalog model. `modelId` is the catalog model's own id (not a
 * registry id). */
export interface CatalogSetupRef {
  providerId: string
  modelId: string
}

/** One row of the Models table: a registered model, observed traffic, or
 * both, reconciled by name. */
export interface MergedModelRow {
  /** Stable React key: the registry id when registered, else the model
   * name (traffic-only rows have no id), else `catalog:<catalog model id>`
   * for an "available" row. */
  key: string
  name: string
  provider: string
  calls: number
  tokens: number
  costUsd: number | null
  usedBy: number
  lastSeen: string | null
  status: MergedModelStatus
  /** The full registry record, when this row has one — the Add/Edit/
   * Delete actions need it. `null` for a traffic-only row (an
   * unregistered name the gateway is still passing through) or an
   * "available" catalog row (nothing registered yet). */
  registered: RegisteredModel | null
  /** See `CatalogSetupRef`. Absent on every row `mergeModelRows` itself
   * produces; only `mergeModelCatalogRows` sets it. */
  catalogSetup?: CatalogSetupRef
}

/** First target's vendor -- its label when it has one, as the traffic
 * summary reports it -- or "—" for a registered model with somehow no
 * targets (the API requires >=1, but a stale/edge-case row shouldn't
 * crash the table). */
function primaryVendor(model: RegisteredModel): string {
  const first = model.targets[0]
  return first?.label || first?.vendor || '—'
}

/**
 * Combines the model registry with the traffic summary into one row set,
 * per model name: registered-and-seen -> "active" (server-reported, real
 * numbers); registered-and-unseen -> "registered" or "disabled", zeroed
 * traffic columns; seen-but-unregistered -> "discovered", no registry
 * record (the gateway forwards an unregistered name byte-for-byte, so
 * traffic for one is entirely expected). Sorted by tokens descending,
 * matching the traffic summary's own order.
 */
export function mergeModelRows(
  registered: RegisteredModel[],
  usage: ModelUsageRow[],
): MergedModelRow[] {
  const usageByName = new Map(usage.map((row) => [row.name, row]))
  const registeredNames = new Set(registered.map((m) => m.name))

  const rows: MergedModelRow[] = registered.map((model) => {
    const seen = usageByName.get(model.name)
    if (seen) {
      // NOTE: this is "active" even when `model.enabled` is false -- a
      // disabled-but-still-seen row is not distinguished from a normal
      // active one today. Left as-is; out of scope for the "discovered"
      // status (see MergedModelStatus's doc comment).
      return {
        key: model.id,
        name: model.name,
        provider: seen.provider || primaryVendor(model),
        calls: seen.calls,
        tokens: seen.tokens,
        costUsd: seen.cost_usd,
        usedBy: seen.used_by,
        lastSeen: seen.last_seen,
        status: 'active',
        registered: model,
      }
    }
    return {
      key: model.id,
      name: model.name,
      provider: primaryVendor(model),
      calls: 0,
      tokens: 0,
      costUsd: null,
      usedBy: 0,
      lastSeen: null,
      status: model.enabled ? 'registered' : 'disabled',
      registered: model,
    }
  })

  for (const seen of usage) {
    if (registeredNames.has(seen.name)) continue
    rows.push({
      key: seen.name,
      name: seen.name,
      provider: seen.provider,
      calls: seen.calls,
      tokens: seen.tokens,
      costUsd: seen.cost_usd,
      usedBy: seen.used_by,
      lastSeen: seen.last_seen,
      // Seen in traffic but nobody registered this name -- distinct from
      // "active" (a registered model with traffic) so the table doesn't
      // imply this call path is managed.
      status: 'discovered',
      registered: null,
    })
  }

  return rows.sort((a, b) => b.tokens - a.tokens || a.name.localeCompare(b.name))
}
