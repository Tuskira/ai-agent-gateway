import { useMemo, useState } from 'react'
import { CircleAlert, CircleCheck, CircleX } from 'lucide-react'
import { toast } from 'sonner'
import { ApiError } from '@/lib/api'
import { useConnectProvider, useCredentialsList, useTestProviderConnection } from '@/lib/queries'
import {
  capabilityWarnings,
  defaultCredentialName,
  type CatalogModel,
  type CatalogProvider,
  type ConnectCredentialInput,
  type TestConnectionResult,
} from '@/lib/model-catalog'
import { ApiKeyInput } from './ApiKeyInput'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

interface ConnectProviderDialogProps {
  /** `null` closes the dialog. */
  provider: CatalogProvider | null
  /** Catalog model id to preselect — the row the "Set up" action was
   * clicked from. */
  preselectModelId?: string
  onOpenChange: (open: boolean) => void
}

/** Outer shell: only mounts the body while a provider is set, keyed so it
 * re-initializes its state for a new provider or a different preselected
 * model — same pattern as `ModelFormDialog`. */
export function ConnectProviderDialog({
  provider,
  preselectModelId,
  onOpenChange,
}: ConnectProviderDialogProps) {
  return (
    <Dialog open={provider !== null} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[calc(100vh-3rem)] w-full flex-col overflow-hidden sm:max-w-xl">
        {provider ? (
          <ConnectProviderBody
            key={`${provider.id}:${preselectModelId ?? ''}`}
            provider={provider}
            preselectModelId={preselectModelId}
            onDone={() => onOpenChange(false)}
            onCancel={() => onOpenChange(false)}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  )
}

type CredentialMode = 'existing' | 'new'

function ModelCapabilityWarnings({ model }: { model: CatalogModel }) {
  const warnings = capabilityWarnings(model)
  if (warnings.length === 0 && !model.notes) return null
  return (
    <div className="flex flex-col gap-1">
      {warnings.length > 0 ? (
        <ul className="flex flex-col gap-0.5">
          {warnings.map((w) => (
            <li
              key={w}
              className="flex items-start gap-1.5 text-[11.5px] leading-snug text-sev-medium-fg"
            >
              <CircleAlert className="mt-0.5 size-3 shrink-0" aria-hidden="true" />
              {w}
            </li>
          ))}
        </ul>
      ) : null}
      {model.notes ? (
        <p className="text-[11.5px] leading-snug text-text-subtle">{model.notes}</p>
      ) : null}
    </div>
  )
}

interface ConnectProviderBodyProps {
  provider: CatalogProvider
  preselectModelId?: string
  onDone: () => void
  onCancel: () => void
}

function ConnectProviderBody({
  provider,
  preselectModelId,
  onDone,
  onCancel,
}: ConnectProviderBodyProps) {
  const credentialsQuery = useCredentialsList()
  const credentials = credentialsQuery.data?.items ?? []
  const preferredCredentialName = defaultCredentialName(provider.slug)
  // Only the exact `<slug>-api-key` credential defaults the dialog into
  // "existing" mode -- an unrelated credential happening to exist doesn't;
  // the user can still pick any of them from the dropdown once there.
  const preferredCredential = credentials.find((c) => c.name === preferredCredentialName)

  // Already-set-up models (`tenant_model_id` set) render checked+disabled
  // for context, but are never part of the submitted selection -- they're
  // already connected, so resubmitting them adds nothing. `selectedIds`
  // only ever holds ids the user can still toggle.
  const alreadySetUpIds = useMemo(
    () => new Set(provider.models.filter((m) => m.tenant_model_id).map((m) => m.id)),
    [provider],
  )
  const [selectedIds, setSelectedIds] = useState<Set<string>>(() =>
    preselectModelId && !alreadySetUpIds.has(preselectModelId)
      ? new Set([preselectModelId])
      : new Set(),
  )
  // The credentials list is a separate, async fetch that usually hasn't
  // resolved on first render, so "existing" vs "new" and which credential
  // is selected can't be fixed at mount. Instead of syncing state from an
  // effect (which would cascade an extra render every time the query
  // settles), these are derived on every render from the query's current
  // data, with only an explicit user choice ever stored in state -- once
  // set, it always wins over the derived default.
  const [modeOverride, setModeOverride] = useState<CredentialMode | null>(null)
  const [existingCredentialOverride, setExistingCredentialOverride] = useState<string | null>(
    null,
  )
  const mode: CredentialMode = modeOverride ?? (preferredCredential ? 'existing' : 'new')
  const existingCredentialName = existingCredentialOverride ?? preferredCredential?.name ?? ''

  const [newCredentialName, setNewCredentialName] = useState(preferredCredentialName)
  const [newApiKey, setNewApiKey] = useState('')
  const [testResult, setTestResult] = useState<TestConnectionResult | null>(null)
  const [formError, setFormError] = useState<string | null>(null)

  function setCredentialMode(next: CredentialMode) {
    setModeOverride(next)
  }

  const testConnection = useTestProviderConnection(provider.id)
  const connect = useConnectProvider(provider.id)

  function toggleModel(id: string, checked: boolean) {
    setSelectedIds((prev) => {
      const next = new Set(prev)
      if (checked) next.add(id)
      else next.delete(id)
      return next
    })
  }

  function credentialTestInput(): { credential?: string; api_key?: string } | null {
    if (mode === 'existing') {
      if (!existingCredentialName) return null
      return { credential: existingCredentialName }
    }
    if (!newApiKey.trim()) return null
    return { api_key: newApiKey.trim() }
  }

  function handleTest() {
    const input = credentialTestInput()
    if (!input) {
      toast.error(
        mode === 'existing' ? 'Choose a credential first.' : 'Enter an API key first.',
      )
      return
    }
    setFormError(null)
    testConnection.mutate(input, {
      onSuccess: setTestResult,
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Test connection failed')
      },
    })
  }

  function handleSubmit() {
    if (selectedIds.size === 0) {
      toast.error('Select at least one model.')
      return
    }
    let credential: ConnectCredentialInput
    if (mode === 'existing') {
      if (!existingCredentialName) {
        toast.error('Choose a credential.')
        return
      }
      credential = { name: existingCredentialName }
    } else {
      if (!newApiKey.trim()) {
        toast.error('Enter an API key.')
        return
      }
      credential = {
        new: { name: newCredentialName.trim() || undefined, api_key: newApiKey.trim() },
      }
    }

    setFormError(null)
    connect.mutate(
      { credential, models: Array.from(selectedIds) },
      {
        onSuccess: (result) => {
          const count = result.models.length
          toast.success(`Set up ${count} model${count === 1 ? '' : 's'}`)
          onDone()
        },
        onError: (err) => {
          if (err instanceof ApiError && (err.status === 400 || err.status === 409)) {
            setFormError(err.message)
            return
          }
          toast.error(err instanceof Error ? err.message : 'Failed to connect provider')
        },
      },
    )
  }

  const checkedCount = alreadySetUpIds.size + selectedIds.size
  // `catalog_matches` is keyed by the vendor's own model id (`model.model_id`,
  // e.g. "zai-org/GLM-5.3"), not the catalog row's uuid (`model.id`) -- the
  // selection sets are uuids, so this walks `provider.models` to translate.
  const matchedCount = useMemo(() => {
    if (!testResult) return null
    let n = 0
    for (const model of provider.models) {
      if (!selectedIds.has(model.id) && !alreadySetUpIds.has(model.id)) continue
      if (testResult.catalog_matches[model.model_id]) n++
    }
    return n
  }, [testResult, selectedIds, alreadySetUpIds, provider.models])

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <DialogHeader>
        <DialogTitle>Connect {provider.display_name}</DialogTitle>
        <DialogDescription>
          Supplies an API key once and registers the models you pick below, so they show up
          as callable models. <span className="font-mono text-[12px]">{provider.base_url}</span>
        </DialogDescription>
      </DialogHeader>

      <div className="-mx-1 flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto px-1 py-4">
        <div className="flex flex-col gap-2">
          <div className="text-[13px] font-semibold text-foreground">Models</div>
          <div className="flex flex-col gap-2.5">
            {provider.models.map((model) => {
              const alreadySetUp = alreadySetUpIds.has(model.id)
              const checked = alreadySetUp || selectedIds.has(model.id)
              const matched = testResult?.catalog_matches[model.model_id]
              return (
                <div
                  key={model.id}
                  className="flex items-start gap-2.5 rounded-r-4 border border-border bg-bg-subtle p-3"
                >
                  <Checkbox
                    className="mt-0.5"
                    aria-label={model.display_name || model.model_id}
                    checked={checked}
                    disabled={alreadySetUp}
                    onCheckedChange={(value) => toggleModel(model.id, value === true)}
                  />
                  <div className="flex min-w-0 flex-1 flex-col gap-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-mono text-[12.5px] font-semibold text-foreground">
                        {model.suggested_name}
                      </span>
                      <span className="text-xs text-text-subtle">{model.model_id}</span>
                      {alreadySetUp ? (
                        <span className="text-xs font-medium text-text-subtle">
                          Already set up as{' '}
                          <span className="font-mono">{model.tenant_model_name}</span>
                        </span>
                      ) : null}
                      {testResult && checked ? (
                        matched ? (
                          <span className="inline-flex items-center gap-1 text-[11px] font-medium text-status-resolved">
                            <CircleCheck className="size-3" aria-hidden="true" />
                            Found on provider
                          </span>
                        ) : (
                          <span className="inline-flex items-center gap-1 text-[11px] font-medium text-sev-medium-fg">
                            <CircleX className="size-3" aria-hidden="true" />
                            Not seen on provider
                          </span>
                        )
                      ) : null}
                    </div>
                    {checked ? <ModelCapabilityWarnings model={model} /> : null}
                  </div>
                </div>
              )
            })}
          </div>
        </div>

        <div className="flex flex-col gap-3">
          <div className="text-[13px] font-semibold text-foreground">Credential</div>
          <div className="flex gap-1.5">
            <Button
              type="button"
              size="sm"
              variant={mode === 'existing' ? 'default' : 'outline'}
              disabled={credentials.length === 0}
              onClick={() => setCredentialMode('existing')}
            >
              Use existing credential
            </Button>
            <Button
              type="button"
              size="sm"
              variant={mode === 'new' ? 'default' : 'outline'}
              onClick={() => setCredentialMode('new')}
            >
              Enter API key
            </Button>
          </div>

          {mode === 'existing' ? (
            <div className="flex flex-col gap-1.5">
              <Label className="text-xs text-text-muted">Credential</Label>
              <Select
                items={credentials.map((c) => ({ value: c.name, label: c.name }))}
                value={existingCredentialName}
                onValueChange={(value) => setExistingCredentialOverride(value as string)}
              >
                <SelectTrigger className="w-full" aria-label="Credential">
                  <SelectValue placeholder="Choose a credential" />
                </SelectTrigger>
                <SelectContent>
                  {credentials.map((c) => (
                    <SelectItem key={c.name} value={c.name}>
                      {c.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          ) : (
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div className="flex flex-col gap-1.5">
                <Label className="text-xs text-text-muted">Credential name</Label>
                <Input
                  className="font-mono"
                  value={newCredentialName}
                  onChange={(e) => setNewCredentialName(e.target.value)}
                  placeholder={preferredCredentialName}
                />
              </div>
              <ApiKeyInput
                aria-label={`${provider.display_name} API key`}
                value={newApiKey}
                onChange={setNewApiKey}
                helperText={`Stored encrypted as ${newCredentialName.trim() || preferredCredentialName}; it is never shown again.`}
              />
            </div>
          )}

          <div className="flex items-center gap-3">
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={testConnection.isPending}
              onClick={handleTest}
            >
              {testConnection.isPending ? 'Testing…' : 'Test connection'}
            </Button>
            {testResult ? (
              testResult.ok ? (
                <span className="inline-flex items-center gap-1.5 text-xs font-medium text-status-resolved">
                  <CircleCheck className="size-3.5" aria-hidden="true" />
                  Connected (HTTP {testResult.status}) — found {testResult.models_found.length}{' '}
                  model{testResult.models_found.length === 1 ? '' : 's'}, matching{' '}
                  {matchedCount} of {checkedCount} selected
                </span>
              ) : (
                <span className="inline-flex items-center gap-1.5 text-xs font-medium text-sev-high-fg">
                  <CircleX className="size-3.5" aria-hidden="true" />
                  Failed (HTTP {testResult.status}){testResult.error ? `: ${testResult.error}` : ''}
                </span>
              )
            ) : null}
          </div>
        </div>

        {formError ? (
          <p className="rounded-r-3 border border-destructive/20 bg-destructive/10 px-3 py-2 text-sm text-destructive">
            {formError}
          </p>
        ) : null}
      </div>

      <DialogFooter>
        <Button type="button" variant="outline" onClick={onCancel}>
          Cancel
        </Button>
        <Button type="button" disabled={connect.isPending} onClick={handleSubmit}>
          {connect.isPending ? 'Setting up…' : 'Connect'}
        </Button>
      </DialogFooter>
    </div>
  )
}
