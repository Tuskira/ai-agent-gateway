import { apiFetch, ApiError } from '@/lib/api'
import type { MergedModelRow, ModelPrice } from '@/lib/models'

/**
 * Data contract for the Model Catalog admin page (`/model-catalog`) and the
 * "Connect <Provider>" flow off the Models page. See
 * `MODEL-CATALOG-CONTRACT.md`'s "Control-plane API" section for the exact
 * shapes -- this file is pinned to it by hand (no Go fixture yet; phase 4
 * adds e2e coverage against the real thing).
 *
 * Catalog = platform-level list of known providers and their models
 * (templates, not callable). A tenant "connects" a provider: supplies an
 * API key once (stored as a normal credential) and enables one or more
 * catalog models, which creates ordinary tenant registry models
 * (`lib/models.ts`) from them. Only those registered models are callable.
 */

/** v1: OpenAI-compatible providers only. */
export type CatalogVendor = 'openai_compat'

/** `{tools, vision, streaming}`: `true`/`false` when known, `null` when
 * unknown -- never guessed. `max_context`: token count, or `null`. Mirrors
 * `models.capabilities` on a registered model created from a catalog entry. */
export interface CatalogCapabilities {
  tools: boolean | null
  vision: boolean | null
  streaming: boolean | null
  max_context: number | null
}

/** One model template under a catalog provider. */
export interface CatalogModel {
  id: string
  /** The vendor's own model id, e.g. "zai-org/GLM-5.3". */
  model_id: string
  display_name: string
  /** Default registry name when this model is connected, e.g.
   * "glm-5.3-nebius" -- `MODEL_NAME_PATTERN`. */
  suggested_name: string
  /** `null` = unknown, never a fabricated number. */
  price: ModelPrice | null
  capabilities: CatalogCapabilities
  /** Free-text admin note explaining a false/limited capability above
   * (e.g. "Multi-turn tool use through Chat Completions translation is not
   * supported yet; plain chat works."); `""` when there is nothing to say. */
  notes: string
  enabled: boolean
  /** Set when this tenant has already connected this catalog model -- the
   * id of the tenant model it created. `null` otherwise. */
  tenant_model_id: string | null
  /** The tenant model's name, alongside `tenant_model_id`. `null` otherwise. */
  tenant_model_name: string | null
}

export interface CatalogProvider {
  id: string
  /** `^[a-z0-9][a-z0-9-]{0,31}$`. */
  slug: string
  display_name: string
  vendor: CatalogVendor
  /** OpenAI-compatible base, including the version segment, no trailing
   * slash -- e.g. "https://api.openai.com/v1". */
  base_url: string
  docs_url: string
  enabled: boolean
  models: CatalogModel[]
}

export interface ModelCatalog {
  providers: CatalogProvider[]
}

/** `GET /api/v1/model-catalog` -- `model.read`. */
export function listModelCatalog(): Promise<ModelCatalog> {
  return apiFetch<ModelCatalog>('/model-catalog')
}

/**
 * Same as `listModelCatalog`, but resolves to `null` on any failure (a 403
 * because this principal lacks `model.read`, or a network error) instead of
 * throwing. Used by the Models page, where catalog data is an enrichment
 * ("Available" rows, provider names) -- not something to fail the whole
 * page over. Mirrors `fetchModelsSummary`'s convention in `lib/models.ts`.
 */
export async function fetchModelCatalogSafe(): Promise<ModelCatalog | null> {
  try {
    return await listModelCatalog()
  } catch {
    return null
  }
}

/* ---------------------------------------------------------------------- */
/* Provider CRUD -- platform.catalog.manage                                */
/* ---------------------------------------------------------------------- */

export interface CatalogProviderInput {
  slug: string
  display_name: string
  base_url: string
  docs_url?: string
  enabled?: boolean
}

/** Create/update response: the provider's own fields (no nested `models`)
 * plus any base-URL warnings -- e.g. "no version segment" -- that don't
 * block the save. Empty when there's nothing to warn about. */
export interface CatalogProviderWriteResult {
  id: string
  slug: string
  display_name: string
  vendor: CatalogVendor
  base_url: string
  docs_url: string
  enabled: boolean
  warnings: string[]
}

export function createCatalogProvider(
  input: CatalogProviderInput,
): Promise<CatalogProviderWriteResult> {
  return apiFetch<CatalogProviderWriteResult>('/model-catalog/providers', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function updateCatalogProvider(
  id: string,
  input: CatalogProviderInput,
): Promise<CatalogProviderWriteResult> {
  return apiFetch<CatalogProviderWriteResult>(
    `/model-catalog/providers/${encodeURIComponent(id)}`,
    {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    },
  )
}

/** `DELETE /model-catalog/providers/{id}`. 409s (with a usage count) when
 * any tenant model still references one of its catalog models -- pass
 * `force: true` to delete anyway (`ModelCatalogPage`'s usage-confirm flow). */
export function deleteCatalogProvider(id: string, opts?: { force?: boolean }): Promise<void> {
  const qs = opts?.force ? '?force=true' : ''
  return apiFetch<void>(`/model-catalog/providers/${encodeURIComponent(id)}${qs}`, {
    method: 'DELETE',
  })
}

/* ---------------------------------------------------------------------- */
/* Model CRUD -- platform.catalog.manage                                   */
/* ---------------------------------------------------------------------- */

export interface CatalogModelInput {
  model_id: string
  display_name?: string
  suggested_name?: string
  price?: ModelPrice
  capabilities?: CatalogCapabilities
  notes?: string
  enabled?: boolean
}

export function createCatalogModel(
  providerId: string,
  input: CatalogModelInput,
): Promise<CatalogModel> {
  return apiFetch<CatalogModel>(
    `/model-catalog/providers/${encodeURIComponent(providerId)}/models`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    },
  )
}

export function updateCatalogModel(id: string, input: CatalogModelInput): Promise<CatalogModel> {
  return apiFetch<CatalogModel>(`/model-catalog/models/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

/** Same 409/`force` rule as `deleteCatalogProvider`. */
export function deleteCatalogModel(id: string, opts?: { force?: boolean }): Promise<void> {
  const qs = opts?.force ? '?force=true' : ''
  return apiFetch<void>(`/model-catalog/models/${encodeURIComponent(id)}${qs}`, {
    method: 'DELETE',
  })
}

export interface CatalogModelUsage {
  tenant_models: number
}

/** `GET /model-catalog/models/{id}/usage` -- how many tenant models (across
 * every tenant) still reference this catalog model. Used to word the
 * usage-confirm dialog before a forced delete. */
export function getCatalogModelUsage(id: string): Promise<CatalogModelUsage> {
  return apiFetch<CatalogModelUsage>(`/model-catalog/models/${encodeURIComponent(id)}/usage`)
}

/**
 * Best-effort usage count for a 409 on a catalog delete, whichever entity.
 * There's no dedicated `GET /providers/{id}/usage`, so this reads
 * `ApiError.details` (the raw error JSON body -- see `lib/api.ts`) for a
 * `tenant_models` number the server may have included alongside the 409,
 * falling back to `null` (an unworded "some tenant models still use this")
 * when it isn't there.
 */
export function usageCountFromError(err: unknown): number | null {
  if (!(err instanceof ApiError)) return null
  const details = err.details
  if (details && typeof details === 'object' && 'tenant_models' in details) {
    const n = (details as { tenant_models?: unknown }).tenant_models
    if (typeof n === 'number') return n
  }
  return null
}

/* ---------------------------------------------------------------------- */
/* Test connection -- model.create                                         */
/* ---------------------------------------------------------------------- */

/** Exactly one of `credential` (an existing credential's name) or `api_key`
 * (a raw key, not yet saved) is set. */
export interface TestConnectionInput {
  credential?: string
  api_key?: string
}

export interface TestConnectionResult {
  ok: boolean
  status: number
  error?: string
  /** Model ids (capped at 200) the provider's own `/models` endpoint listed. */
  models_found: string[]
  /** Keyed by the vendor's own model id (`CatalogModel.model_id`, e.g.
   * "zai-org/GLM-5.3") -- NOT the catalog row's uuid (`CatalogModel.id`) --
   * to whether `models_found` included it. */
  catalog_matches: Record<string, boolean>
}

export function testProviderConnection(
  providerId: string,
  input: TestConnectionInput,
): Promise<TestConnectionResult> {
  return apiFetch<TestConnectionResult>(
    `/model-catalog/providers/${encodeURIComponent(providerId)}/test`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    },
  )
}

/* ---------------------------------------------------------------------- */
/* Connect -- model.create + credential.create (+ credential.read)         */
/* ---------------------------------------------------------------------- */

export type ConnectCredentialInput =
  | { name: string }
  | { new: { name?: string; api_key: string } }

export interface ConnectProviderInput {
  credential: ConnectCredentialInput
  /** Catalog model ids -- at least one. */
  models: string[]
}

export interface ConnectResultModel {
  catalog_model_id: string
  /** The new (or existing) tenant model's registry id. */
  model_id: string
  name: string
  /** "exists" when a tenant model of that name was already there -- not an
   * error; the connect call is otherwise idempotent per model. */
  status: 'created' | 'exists'
}

export interface ConnectResult {
  credential: { name: string; created: boolean }
  models: ConnectResultModel[]
}

export function connectProvider(
  providerId: string,
  input: ConnectProviderInput,
): Promise<ConnectResult> {
  return apiFetch<ConnectResult>(
    `/model-catalog/providers/${encodeURIComponent(providerId)}/connect`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    },
  )
}

/* ---------------------------------------------------------------------- */
/* Merge into the Models page's row set                                    */
/* ---------------------------------------------------------------------- */

/**
 * Layers the Model Catalog onto `mergeModelRows`'s output (`lib/models.ts`):
 *
 * - A catalog model this tenant hasn't set up (`tenant_model_id === null`)
 *   that matches no existing row becomes a new "available" row.
 * - A catalog model already set up (`tenant_model_id` set) points back at
 *   the matching registered/active/disabled row by registry id, and that
 *   row's `provider` is replaced with the catalog's friendly provider
 *   display name (e.g. "Nebius AI Studio" instead of the raw
 *   `openai_compat` vendor the Connect flow's targets carry no label for).
 * - A "discovered" row (traffic for a name nobody registered) whose name
 *   matches a catalog model's `suggested_name` or raw `model_id` gets a
 *   `catalogSetup` reference too, so the Models page can offer "Set up" on
 *   it — its status stays "discovered" (it may still be some other
 *   passthrough name that happens to collide; only the Connect action
 *   changes).
 *
 * A `null`/`undefined` catalog (the safe fetch failed, or hasn't loaded
 * yet) is a no-op — callers always get back at least what they passed in.
 */
export function mergeModelCatalogRows(
  rows: MergedModelRow[],
  catalog: ModelCatalog | null | undefined,
): MergedModelRow[] {
  if (!catalog) return rows

  const next = rows.map((r) => ({ ...r }))
  const byRegisteredId = new Map<string, MergedModelRow>()
  for (const row of next) {
    if (row.registered) byRegisteredId.set(row.registered.id, row)
  }
  const discovered = next.filter((r) => r.status === 'discovered')
  const available: MergedModelRow[] = []

  for (const provider of catalog.providers) {
    for (const model of provider.models) {
      if (model.tenant_model_id) {
        const row = byRegisteredId.get(model.tenant_model_id)
        if (row) row.provider = provider.display_name
        continue
      }
      const match = discovered.find(
        (r) => r.name === model.suggested_name || r.name === model.model_id,
      )
      if (match) {
        match.catalogSetup = { providerId: provider.id, modelId: model.id }
        continue
      }
      available.push({
        key: `catalog:${model.id}`,
        name: model.suggested_name,
        provider: provider.display_name,
        calls: 0,
        tokens: 0,
        costUsd: null,
        usedBy: 0,
        lastSeen: null,
        status: 'available',
        registered: null,
        catalogSetup: { providerId: provider.id, modelId: model.id },
      })
    }
  }

  return [...next, ...available].sort(
    (a, b) => b.tokens - a.tokens || a.name.localeCompare(b.name),
  )
}

/** Default credential name the Connect dialog proposes for a new key. */
export function defaultCredentialName(providerSlug: string): string {
  return `${providerSlug}-api-key`
}

/**
 * Per-model warnings shown in the Connect dialog for a selected catalog
 * model -- capability/price gaps the tenant should know about before
 * relying on it, never blocking. Order matches the contract: tools, then
 * price.
 */
export function capabilityWarnings(model: CatalogModel): string[] {
  const warnings: string[] = []
  if (model.capabilities.tools === false) {
    warnings.push(
      'No tool calling: will not work with Claude Code or other agents that use tools',
    )
  } else if (model.capabilities.tools === null) {
    warnings.push('Tool support unknown')
  }
  if (model.price === null) {
    warnings.push("No price set: cost and dollar budgets won't apply until an admin sets one")
  }
  return warnings
}

/* ---------------------------------------------------------------------- */
/* Price refresh -- platform.catalog.manage                                */
/* ---------------------------------------------------------------------- */

/** Catalog provider slugs with a registered price source
 * (`internal/modelcatalog/prices`) -- the admin catalog page's "Refresh
 * prices" row action is shown only for these. */
const PRICE_REFRESH_SLUGS: ReadonlySet<string> = new Set(['nebius', 'together'])

/** Whether slug has a registered price source. Nebius's feed is public
 * (no key needed); Together's needs one. */
export function supportsPriceRefresh(slug: string): boolean {
  return PRICE_REFRESH_SLUGS.has(slug)
}

/** `POST .../prices/preview` body. Both optional for `nebius` (public
 * feed); exactly one required for `together`. */
export interface PreviewPricesInput {
  credential?: string
  api_key?: string
}

/** One catalog model's price diff, as previewed against the provider's own
 * pricing API. */
export interface CatalogPriceItem {
  catalog_model_id: string
  model_id: string
  current_price: ModelPrice | null
  /** `null` when the provider's pricing feed didn't mention this model's
   * vendor id -- nothing to apply for it; never force-cleared. */
  new_price: ModelPrice | null
  changed: boolean
}

export interface PreviewPricesResult {
  /** The exact URL fetched -- no key, no query secret. */
  source_url: string
  fetched_at: string
  items: CatalogPriceItem[]
  /** Vendor model ids the feed priced that match none of this provider's
   * catalog models. */
  unmatched_provider_models: number
}

/** `POST /model-catalog/providers/{id}/prices/preview` --
 * `platform.catalog.manage`. Read-only. */
export function previewCatalogPrices(
  providerId: string,
  input: PreviewPricesInput,
): Promise<PreviewPricesResult> {
  return apiFetch<PreviewPricesResult>(
    `/model-catalog/providers/${encodeURIComponent(providerId)}/prices/preview`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    },
  )
}

export interface ApplyPricesItemInput {
  catalog_model_id: string
  price: ModelPrice | null
}

/** `POST .../prices/apply` body. */
export interface ApplyPricesInput {
  items: ApplyPricesItemInput[]
  /** When true, a connected tenant model whose price still equals its
   * catalog model's OLD price is refreshed too -- a tenant's own manual
   * override is left alone either way. */
  update_tenant_models: boolean
}

export interface ApplyPricesResult {
  catalog_updated: number
  tenant_updated: number
}

/** `POST /model-catalog/providers/{id}/prices/apply` --
 * `platform.catalog.manage`. */
export function applyCatalogPrices(
  providerId: string,
  input: ApplyPricesInput,
): Promise<ApplyPricesResult> {
  return apiFetch<ApplyPricesResult>(
    `/model-catalog/providers/${encodeURIComponent(providerId)}/prices/apply`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    },
  )
}
