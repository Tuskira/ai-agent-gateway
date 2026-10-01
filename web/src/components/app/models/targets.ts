import {
  MODEL_LABEL_PATTERN,
  PROVIDER_PRESETS,
  RESERVED_MODEL_LABELS,
  type ModelTarget,
  type ModelVendor,
} from '@/lib/models'

/** Local, non-react-hook-form editable row — the shape of a single model
 * target while it's being edited. `baseUrl`/`region`/`allowCallerKey`/`label` are
 * only sent to the API for the targets that may carry them (see
 * `rowsToTargets`), but kept on every row so switching vendors doesn't
 * lose what was typed. */
export interface TargetRowState {
  id: string
  vendor: ModelVendor
  model: string
  baseUrl: string
  region: string
  credential: string
  allowCallerKey: boolean
  label: string
  /** A vendor API key typed into the form ("Accept API key"), set while
   * that option is chosen. Never sent as a target field: on save it is
   * stored (or, if a same-name credential already exists, replaces its
   * value) as a credential first, and the target references it by name.
   * See `apiKeyCredentialName`. */
  apiKey?: string
}

/** What the Vendor dropdown shows: Anthropic, Bedrock, the named
 * OpenAI-compatible providers, and `openai_compat` as "Others" (any other
 * OpenAI-compatible endpoint, named by its label). The native Gemini API
 * (`gemini`) is listed only for a row already on it. */
export type VendorChoice = ModelVendor | string

/** A named OpenAI-compatible provider the Vendor dropdown offers, with the
 * `label` (`tag`) its targets carry and the base URL it starts a row at.
 * Resolved from the Model Catalog when one is configured (`resolveProviderPresets`),
 * falling back to `PROVIDER_PRESETS` otherwise -- see ADDENDUM 1 item 6 in
 * MODEL-CATALOG-CONTRACT.md. */
export interface VendorPreset {
  value: string
  label: string
  tag: string
  baseUrl: string
}

/** Minimal shape `resolveProviderPresets` needs from a Model Catalog
 * provider -- structurally satisfied by `CatalogProvider`
 * (`lib/model-catalog.ts`) without importing it (this module is also used
 * from the plain model registry, with no catalog in play). */
export interface CatalogProviderPresetSource {
  slug: string
  display_name: string
  base_url: string
  enabled: boolean
}

const STATIC_PRESETS: VendorPreset[] = PROVIDER_PRESETS.map((p) => ({
  value: p.value,
  label: p.label,
  tag: p.tag,
  baseUrl: p.baseUrl,
}))

/**
 * The provider list the Vendor dropdown's named entries come from: the
 * Model Catalog's enabled providers when there are any (its base URLs
 * win over the static ones), else `PROVIDER_PRESETS`. A `null`/`undefined`
 * or empty catalog (not configured yet, unreachable, or nothing enabled)
 * falls back to the static list so the form still offers Nebius/Together
 * AI/OpenAI/Google Gemini without a catalog.
 */
export function resolveProviderPresets(
  catalogProviders: readonly CatalogProviderPresetSource[] | null | undefined,
): VendorPreset[] {
  const enabled = (catalogProviders ?? []).filter((p) => p.enabled)
  if (enabled.length === 0) return STATIC_PRESETS
  return enabled.map((p) => ({
    value: p.slug,
    label: p.display_name,
    tag: p.slug,
    baseUrl: p.base_url,
  }))
}

const NATIVE_CHOICES: { value: VendorChoice; label: string }[] = [
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'bedrock', label: 'Amazon Bedrock' },
]
const OTHERS = { value: 'openai_compat' as const, label: 'Others' }
const NATIVE_GEMINI = { value: 'gemini' as const, label: 'Google Gemini (native API)' }

export function vendorChoicesFor(
  row: TargetRowState,
  presets: VendorPreset[] = STATIC_PRESETS,
): { value: VendorChoice; label: string }[] {
  return [
    ...NATIVE_CHOICES,
    ...presets.map(({ value, label }) => ({ value, label })),
    OTHERS,
    ...(row.vendor === 'gemini' ? [NATIVE_GEMINI] : []),
  ]
}

function presetOf(choice: string, presets: VendorPreset[]) {
  return presets.find((p) => p.value === choice)
}

/** The dropdown entry for a row: a named provider when an OpenAI-compatible
 * target carries its label, else the row's own vendor. */
export function vendorChoice(
  row: TargetRowState,
  presets: VendorPreset[] = STATIC_PRESETS,
): VendorChoice {
  const label = row.label.trim()
  const preset = row.vendor === 'openai_compat' ? presets.find((p) => p.tag === label) : undefined
  return preset ? preset.value : row.vendor
}

/** The provider-scoped credential name "Accept API key" manages for a row:
 * `<label>-api-key` for a named/Others provider, `<vendor>-api-key`
 * otherwise (e.g. `anthropic-api-key`). One per provider, not one per
 * model -- see ADDENDUM 1 item 5. */
export function apiKeyCredentialName(row: TargetRowState): string {
  return `${row.label.trim() || row.vendor}-api-key`
}

/** The row after the Vendor dropdown changes to `choice`: the vendor's
 * default link (editable afterwards; empty for Others and Bedrock), and
 * for a named provider its label. Leaving a named provider drops its
 * label. A typed key is cleared on every change -- it belongs to the
 * vendor it was typed for -- and Bedrock (an AWS key pair) takes none.
 * When the row's credential is empty, or was the auto-managed slot for
 * the vendor it's leaving, it's repointed at the new vendor's own slot
 * name if a credential by that name already exists (so switching from a
 * provider whose key is on file to another one with a key on file doesn't
 * leave a stale, mismatched credential selected) -- a credential the user
 * picked by hand is left alone. */
export function applyVendorChoice(
  row: TargetRowState,
  choice: VendorChoice,
  credentials: readonly { name: string }[] = [],
  presets: VendorPreset[] = STATIC_PRESETS,
): TargetRowState {
  const preset = presetOf(choice, presets)
  const vendor = preset ? 'openai_compat' : (choice as ModelVendor)
  const label = preset ? preset.tag : presetOf(vendorChoice(row, presets), presets) ? '' : row.label
  const wasAutoSlot = row.credential !== '' && row.credential === apiKeyCredentialName(row)
  const next: TargetRowState = {
    ...row,
    vendor,
    label,
    baseUrl: choice === 'anthropic' ? 'https://api.anthropic.com' : (preset?.baseUrl ?? ''),
    apiKey: row.apiKey === undefined || choice === 'bedrock' ? undefined : '',
  }
  if (choice === 'bedrock') return { ...next, credential: '' }
  if (row.credential === '' || wasAutoSlot) {
    const slotName = apiKeyCredentialName(next)
    next.credential = credentials.some((c) => c.name === slotName) ? slotName : ''
  }
  return next
}

export function newTargetRow(): TargetRowState {
  return {
    id: crypto.randomUUID(),
    vendor: 'anthropic',
    model: '',
    baseUrl: '',
    region: '',
    credential: '',
    allowCallerKey: false,
    label: '',
  }
}

/** Converts a registered model's `targets` (API shape) into editable rows.
 * A model being created starts from one blank row, same as a connector
 * with no headers yet starts from none — but a target list can't be
 * empty (the API requires >=1), so `ModelFormDialog` always has at least
 * one row on screen. */
export function targetsToRows(targets?: ModelTarget[]): TargetRowState[] {
  if (!targets || targets.length === 0) return [newTargetRow()]
  return targets.map((t) => ({
    id: crypto.randomUUID(),
    vendor: t.vendor,
    model: t.model,
    baseUrl: t.base_url ?? '',
    region: t.region ?? '',
    credential: t.credential ?? '',
    allowCallerKey: t.allow_caller_key === true,
    label: t.label ?? '',
  }))
}

/** Whether a vendor's target may carry a `label` (the API takes one on an
 * OpenAI-compatible target only: the one vendor type that stands for many
 * vendors). */
export function vendorTakesLabel(vendor: ModelVendor): boolean {
  return vendor === 'openai_compat'
}

/** Whether a vendor's target may carry a `base_url` (the API rejects one
 * on a Bedrock target, whose host is computed from the region). */
export function vendorTakesBaseUrl(vendor: ModelVendor): boolean {
  return vendor !== 'bedrock'
}

/** Row order is preserved -- it's the fallback order the LLM plane tries
 * targets in. Rows with no model id are dropped rather than sent as
 * invalid targets. Only what the API accepts for the row's vendor is
 * sent: `region` for Bedrock alone, `base_url` for every vendor but
 * Bedrock, `label` for an OpenAI-compatible target alone,
 * `allow_caller_key` only without a credential (the API refuses the
 * combination). */
export function rowsToTargets(rows: TargetRowState[]): ModelTarget[] {
  return rows
    .filter((r) => r.model.trim().length > 0)
    .map((r) => {
      const target: ModelTarget = { vendor: r.vendor, model: r.model.trim() }
      if (vendorTakesBaseUrl(r.vendor) && r.baseUrl.trim())
        target.base_url = r.baseUrl.trim()
      if (r.vendor === 'bedrock' && r.region.trim()) target.region = r.region.trim()
      if (r.credential.trim()) target.credential = r.credential.trim()
      else if (r.allowCallerKey) target.allow_caller_key = true
      if (vendorTakesLabel(r.vendor) && r.label.trim()) target.label = r.label.trim()
      return target
    })
}

/** Whether a row has everything the registry requires (see
 * `config.ValidateModelTarget`: `openai_compat` needs `base_url`,
 * `bedrock` needs `region`, a `base_url` is https unless the host is
 * loopback). Used to block submit and show an inline error, mirroring
 * `header-rows.ts`'s `headerRowError`. */
export function targetRowError(row: TargetRowState): string | null {
  if (!row.model.trim()) return 'Model id is required'
  if (row.apiKey !== undefined && !row.apiKey.trim()) return 'API key is required'
  if (row.vendor === 'openai_compat' && !row.baseUrl.trim()) {
    return 'Base URL is required for an OpenAI-compatible target'
  }
  if (row.vendor === 'bedrock' && !row.region.trim()) {
    return 'Region is required for a Bedrock target'
  }
  if (vendorTakesBaseUrl(row.vendor) && row.baseUrl.trim()) {
    const problem = baseUrlError(row.baseUrl.trim())
    if (problem) return problem
  }
  if (vendorTakesLabel(row.vendor) && row.label.trim()) {
    const label = row.label.trim()
    if (!MODEL_LABEL_PATTERN.test(label)) {
      return 'Label: lowercase letters, numbers, dot, underscore, and dash only (max 32 chars)'
    }
    if (RESERVED_MODEL_LABELS.includes(label)) {
      return `Label "${label}" is reserved`
    }
  }
  return null
}

/** Hosts plain http is accepted for: the machine the gateway runs on
 * (`isLoopbackHost` in internal/config; host.docker.internal is matched
 * exactly). */
const LOOPBACK_HOSTS = new Set([
  'localhost',
  '127.0.0.1',
  '[::1]',
  'host.docker.internal',
])

function baseUrlError(raw: string): string | null {
  let url: URL
  try {
    url = new URL(raw)
  } catch {
    return 'Base URL must be an absolute http(s) URL'
  }
  if (url.protocol !== 'https:' && url.protocol !== 'http:') {
    return 'Base URL must be an absolute http(s) URL'
  }
  if (url.protocol === 'http:' && !LOOPBACK_HOSTS.has(url.hostname)) {
    return 'Base URL must use https (http is allowed for a loopback host and host.docker.internal only)'
  }
  if (url.search || url.hash || url.username || url.password) {
    return 'Base URL must not carry a query, fragment or credentials'
  }
  return null
}
