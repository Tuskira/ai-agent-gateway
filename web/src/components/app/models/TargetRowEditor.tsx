import { useMemo } from 'react'
import { ArrowDown, ArrowUp, Trash2 } from 'lucide-react'
import { useCredentialsList, useModelCatalogSummary } from '@/lib/queries'
import {
  apiKeyCredentialName,
  applyVendorChoice,
  resolveProviderPresets,
  targetRowError,
  vendorChoice,
  vendorTakesBaseUrl,
  vendorChoicesFor,
  vendorTakesLabel,
  type TargetRowState,
  type VendorChoice,
} from './targets'
import { ApiKeyInput } from './ApiKeyInput'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

interface TargetRowEditorProps {
  row: TargetRowState
  index: number
  total: number
  onChange: (next: TargetRowState) => void
  onRemove: () => void
  onMove: (direction: -1 | 1) => void
}

/** Credential select values that are not credential names. */
const NO_CREDENTIAL = '__none__'
const ACCEPT_API_KEY = '__api_key__'

/**
 * One row of a model's ordered target list — see `CROSS-MODEL-CONTRACT.md`:
 * the LLM plane tries targets in this order, falling back to the next one
 * on a dial/TLS/5xx failure before any response byte reaches the client.
 * `base_url` shows for every vendor but `bedrock` (required for
 * `openai_compat` only), `region` only for `bedrock`, matching the
 * registry's own validation (`targetRowError`). `label` shows for
 * `openai_compat` only; a named provider (OpenAI, Google Gemini, Nebius,
 * Together AI) sets it, and the Vendor dropdown follows it. "Accept API key" takes a key the dialog stores as a credential
 * on save (not offered for Bedrock, which signs with an AWS key pair).
 * "Allow caller key" is only meaningful without a credential (the API
 * refuses the combination).
 */
export function TargetRowEditor({
  row,
  index,
  total,
  onChange,
  onRemove,
  onMove,
}: TargetRowEditorProps) {
  const credentialsQuery = useCredentialsList()
  const credentials = credentialsQuery.data?.items ?? []
  // The Model Catalog's enabled providers, when there are any, are the
  // single source for the named-provider choices below (its base URLs
  // win); PROVIDER_PRESETS is the fallback when it's empty or unreachable
  // -- `useModelCatalogSummary` never throws (see its doc comment), so a
  // catalog outage just means the static list. ADDENDUM 1 item 6.
  const catalogQuery = useModelCatalogSummary()
  const presets = useMemo(
    () => resolveProviderPresets(catalogQuery.data?.providers),
    [catalogQuery.data],
  )
  const slotName = apiKeyCredentialName(row)
  const slotExists = credentials.some((c) => c.name === slotName)
  const credentialItems = [
    { value: NO_CREDENTIAL, label: 'None' },
    ...credentials.map((c) => ({ value: c.name, label: c.name })),
    ...(row.vendor === 'bedrock'
      ? []
      : [{ value: ACCEPT_API_KEY, label: slotExists ? `Replace ${slotName}` : 'Accept API key' }]),
  ]
  const typingKey = row.apiKey !== undefined
  const choice = vendorChoice(row, presets)
  const vendorChoices = vendorChoicesFor(row, presets)
  const error = targetRowError(row)
  const baseUrlRequired = row.vendor === 'openai_compat'
  const callerKeyId = `allow-caller-key-${row.id}`

  return (
    <div className="flex flex-col gap-3 rounded-r-4 border border-border bg-bg-subtle p-3.5">
      <div className="flex items-center justify-between gap-2">
        <span className="text-[11px] font-semibold tracking-[.06em] text-text-subtle uppercase">
          Target {index + 1}
          {index === 0 ? ' · tried first' : ''}
        </span>
        <div className="flex items-center gap-1">
          <Button
            type="button"
            variant="ghost"
            size="icon-sm"
            aria-label="Move target up"
            disabled={index === 0}
            onClick={() => onMove(-1)}
          >
            <ArrowUp className="size-3.5" aria-hidden="true" />
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="icon-sm"
            aria-label="Move target down"
            disabled={index === total - 1}
            onClick={() => onMove(1)}
          >
            <ArrowDown className="size-3.5" aria-hidden="true" />
          </Button>
        </div>
      </div>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div className="flex flex-col gap-1.5">
          <Label className="text-xs text-text-muted">Vendor</Label>
          <Select
            items={vendorChoices}
            value={choice}
            onValueChange={(value) =>
              onChange(applyVendorChoice(row, value as VendorChoice, credentials, presets))
            }
          >
            <SelectTrigger className="w-full" aria-label={`Target ${index + 1} vendor`}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {vendorChoices.map((v) => (
                <SelectItem key={v.value} value={v.value}>
                  {v.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="flex flex-col gap-1.5">
          <Label className="text-xs text-text-muted">Model id</Label>
          <Input
            placeholder="e.g. claude-sonnet-4-5-20250929"
            className="font-mono"
            value={row.model}
            onChange={(e) => onChange({ ...row, model: e.target.value })}
          />
        </div>
      </div>

      {vendorTakesBaseUrl(row.vendor) ? (
        <div className="flex flex-col gap-1.5">
          <Label className="text-xs text-text-muted">
            {baseUrlRequired ? 'Base URL *' : 'Base URL'}
          </Label>
          <Input
            placeholder={
              baseUrlRequired
                ? 'https://api.example.com/v1'
                : "Default: the gateway's configured host"
            }
            aria-label={`Target ${index + 1} base URL`}
            className="font-mono"
            value={row.baseUrl}
            onChange={(e) => onChange({ ...row, baseUrl: e.target.value })}
          />
          {baseUrlRequired ? (
            <p className="text-xs text-text-subtle">
              The API root the gateway appends /chat/completions to for Anthropic-format
              clients (Claude Code, the Anthropic SDK); it usually ends in /v1.
            </p>
          ) : null}
        </div>
      ) : null}

      {vendorTakesLabel(row.vendor) ? (
        <div className="flex flex-col gap-1.5">
          <Label className="text-xs text-text-muted">Label (optional, e.g. groq)</Label>
          <Input
            placeholder="groq"
            className="font-mono"
            aria-label={`Target ${index + 1} label`}
            value={row.label}
            onChange={(e) => onChange({ ...row, label: e.target.value })}
          />
          <p className="text-xs text-text-subtle">
            The vendor behind this endpoint: shown as the provider, used for pricing, and
            the suffix of the caller's key header (X-Provider-Key-
            {row.label.trim() || 'label'}).
          </p>
        </div>
      ) : null}

      {row.vendor === 'bedrock' ? (
        <div className="flex flex-col gap-1.5">
          <Label className="text-xs text-text-muted">Region *</Label>
          <Input
            placeholder="us-east-1"
            className="font-mono"
            value={row.region}
            onChange={(e) => onChange({ ...row, region: e.target.value })}
          />
        </div>
      ) : null}

      <div className="flex flex-col gap-1.5">
        <Label className="text-xs text-text-muted">Credential</Label>
        <Select
          items={credentialItems}
          value={typingKey ? ACCEPT_API_KEY : row.credential || NO_CREDENTIAL}
          onValueChange={(value) =>
            onChange(
              value === ACCEPT_API_KEY
                ? { ...row, credential: '', apiKey: '' }
                : {
                    ...row,
                    credential: !value || value === NO_CREDENTIAL ? '' : value,
                    apiKey: undefined,
                  },
            )
          }
        >
          <SelectTrigger className="w-full" aria-label={`Target ${index + 1} credential`}>
            <SelectValue placeholder="None" />
          </SelectTrigger>
          <SelectContent>
            {credentialItems.map((item) => (
              <SelectItem key={item.value} value={item.value}>
                {item.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {typingKey ? (
          <ApiKeyInput
            aria-label={`Target ${index + 1} API key`}
            value={row.apiKey ?? ''}
            onChange={(value) => onChange({ ...row, apiKey: value })}
            helperText={
              slotExists
                ? `Replaces the current value of ${slotName} for every model that uses it.`
                : `Stored encrypted as ${slotName} when you save; it is never shown again.`
            }
          />
        ) : null}
      </div>

      <div className="flex items-start gap-3">
        <Switch
          id={callerKeyId}
          aria-labelledby={`${callerKeyId}-name`}
          checked={row.allowCallerKey && !row.credential && !typingKey}
          disabled={Boolean(row.credential) || typingKey}
          onCheckedChange={(checked) => onChange({ ...row, allowCallerKey: checked })}
        />
        <div className="flex flex-col gap-0.5">
          <span id={`${callerKeyId}-name`} className="sr-only">
            Target {index + 1} allow caller key
          </span>
          <Label htmlFor={callerKeyId} className="text-xs text-text-muted">
            Allow caller key
          </Label>
          <p className="text-xs text-text-subtle">
            {row.credential || typingKey
              ? 'Not used: this target sends its own credential.'
              : "Send the caller's own vendor key to this target. Without it, a target with no credential is only usable on the gateway's default host for the vendor."}
          </p>
        </div>
      </div>

      {error ? <p className="text-xs font-medium text-destructive">{error}</p> : null}

      <Button
        type="button"
        variant="ghost"
        size="sm"
        className="self-start text-sev-high hover:bg-sev-high-bg hover:text-sev-high"
        onClick={onRemove}
        disabled={total <= 1}
        title={total <= 1 ? 'A model needs at least one target' : undefined}
      >
        <Trash2 className="size-3.5" aria-hidden="true" />
        Remove target
      </Button>
    </div>
  )
}
