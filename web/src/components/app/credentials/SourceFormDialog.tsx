import { useState } from 'react'
import { toast } from 'sonner'
import {
  useCreateCredentialSource,
  useCredentialsList,
  useHeaderProviders,
  useUpdateCredentialSource,
} from '@/lib/queries'
import type { CredentialSource } from '@/lib/credential-sources'
import { providerNeedsSource, type HeaderProvider } from '@/lib/headers'
import { Button } from '@/components/ui/button'
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
import { Switch } from '@/components/ui/switch'

/** The source config field that names a credential in this gateway's own
 * store (the token it authenticates to the outside service with). */
const AUTH_CREDENTIAL_FIELD = 'auth_credential'

interface SourceFormDialogProps {
  open: boolean
  /** The source to edit; `null` to create one. */
  source: CredentialSource | null
  onOpenChange: (open: boolean) => void
}

/** Remounted per open (see the wrapper), so the form state always starts
 * from the source being edited, or empty for a new one. */
export function SourceFormDialog({ open, source, onOpenChange }: SourceFormDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        {open ? <SourceForm source={source} onDone={() => onOpenChange(false)} /> : null}
      </DialogContent>
    </Dialog>
  )
}

function configToStrings(
  config: Record<string, unknown> | undefined,
): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [key, value] of Object.entries(config ?? {})) {
    if (typeof value === 'string') out[key] = value
  }
  return out
}

function SourceForm({
  source,
  onDone,
}: {
  source: CredentialSource | null
  onDone: () => void
}) {
  const isEdit = source !== null
  const providersQuery = useHeaderProviders()
  const credentialsQuery = useCredentialsList()
  const createSource = useCreateCredentialSource()
  const updateSource = useUpdateCredentialSource()

  const sourceProviders = (providersQuery.data?.providers ?? []).filter((p) =>
    providerNeedsSource(p),
  )
  const [name, setName] = useState(source?.name ?? '')
  const [providerId, setProviderId] = useState(source?.provider ?? '')
  const [config, setConfig] = useState<Record<string, string>>(() =>
    configToStrings(source?.config),
  )
  const [enabled, setEnabled] = useState(source?.enabled ?? true)
  const [error, setError] = useState<string | null>(null)

  const provider: HeaderProvider | undefined = sourceProviders.find(
    (p) => p.id === providerId,
  )
  const schema = provider?.source_schema
  const providerItems = sourceProviders.map((p) => ({ value: p.id, label: p.name }))
  const credentialNames = (credentialsQuery.data?.items ?? []).map((c) => c.name)
  const pending = createSource.isPending || updateSource.isPending

  function validate(): string | null {
    if (!name.trim()) return 'Name is required'
    if (!provider || !schema) return 'Provider is required'
    for (const key of schema.required ?? []) {
      if (!(config[key] ?? '').trim())
        return `${schema.properties[key]?.title ?? key} is required`
    }
    return null
  }

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    const problem = validate()
    setError(problem)
    if (problem || !provider || !schema) return

    // Only the fields the schema knows, trimmed; blank optional ones are
    // left out so the provider's own default applies.
    const body: Record<string, string> = {}
    for (const key of Object.keys(schema.properties)) {
      const value = (config[key] ?? '').trim()
      if (value) body[key] = value
    }

    const onSuccess = () => {
      toast.success(isEdit ? 'Credential source updated' : 'Credential source created')
      onDone()
    }
    const onError = (err: unknown) => {
      const message =
        err instanceof Error ? err.message : 'Failed to save credential source'
      setError(message)
      toast.error(message)
    }
    if (source) {
      updateSource.mutate(
        { id: source.id, input: { name: name.trim(), config: body, enabled } },
        { onSuccess, onError },
      )
    } else {
      createSource.mutate(
        { name: name.trim(), provider: provider.id, config: body, enabled },
        { onSuccess, onError },
      )
    }
  }

  return (
    <form onSubmit={handleSubmit} className="flex flex-col gap-4">
      <DialogHeader>
        <DialogTitle>
          {isEdit ? `Edit ${source.name}` : 'Add credential source'}
        </DialogTitle>
        <DialogDescription>
          An outside credential service the gateway asks for header secrets. A source
          holds settings only; the token it authenticates with is a credential.
        </DialogDescription>
      </DialogHeader>

      <div className="flex flex-col gap-4">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="source-name" className="text-xs text-text-muted">
            Name
          </Label>
          <Input
            id="source-name"
            placeholder="e.g. vault-a"
            className="font-mono"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>

        <div className="flex flex-col gap-1.5">
          <Label className="text-xs text-text-muted">Provider</Label>
          <Select
            items={providerItems}
            value={providerId || null}
            disabled={isEdit}
            onValueChange={(value) => {
              setProviderId(value ?? '')
              setConfig({})
            }}
          >
            <SelectTrigger className="w-full" aria-label="Provider">
              <SelectValue placeholder="Select a provider…" />
            </SelectTrigger>
            <SelectContent>
              {providerItems.map((item) => (
                <SelectItem key={item.value} value={item.value}>
                  {item.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          {!providersQuery.isLoading && providerItems.length === 0 ? (
            <p className="text-xs text-text-subtle">
              No provider on this gateway uses credential sources. Enable one in the
              gateway config first.
            </p>
          ) : null}
        </div>

        {schema
          ? Object.entries(schema.properties).map(([key, prop]) => {
              const required = schema.required?.includes(key)
              const label = `${prop.description ?? key}${required ? ' *' : ''}`
              const inputId = `source-config-${key}`
              if (key === AUTH_CREDENTIAL_FIELD) {
                const current = config[key] ?? ''
                const names =
                  !current || credentialNames.includes(current)
                    ? credentialNames
                    : [current, ...credentialNames]
                const items = names.map((n) => ({ value: n, label: n }))
                return (
                  <div key={key} className="flex flex-col gap-1.5">
                    <Label className="text-xs text-text-muted">{label}</Label>
                    <Select
                      items={items}
                      value={current || null}
                      onValueChange={(value) =>
                        setConfig((c) => ({ ...c, [key]: value ?? '' }))
                      }
                    >
                      <SelectTrigger
                        className="w-full"
                        aria-label="Authentication credential"
                      >
                        <SelectValue placeholder="Select a credential…" />
                      </SelectTrigger>
                      <SelectContent>
                        {items.map((item) => (
                          <SelectItem key={item.value} value={item.value}>
                            {item.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    {!credentialsQuery.isLoading && credentialNames.length === 0 ? (
                      <p className="text-xs text-text-subtle">
                        No credentials yet. Add one on the Credentials tab first.
                      </p>
                    ) : null}
                  </div>
                )
              }
              return (
                <div key={key} className="flex flex-col gap-1.5">
                  <Label htmlFor={inputId} className="text-xs text-text-muted">
                    {label}
                  </Label>
                  <Input
                    id={inputId}
                    className="font-mono"
                    value={config[key] ?? ''}
                    onChange={(e) => setConfig((c) => ({ ...c, [key]: e.target.value }))}
                  />
                </div>
              )
            })
          : null}

        <div className="flex items-center justify-between gap-3">
          <Label htmlFor="source-enabled" className="text-sm text-foreground">
            Enabled
          </Label>
          <Switch id="source-enabled" checked={enabled} onCheckedChange={setEnabled} />
        </div>
      </div>

      {error ? <p className="text-xs font-medium text-destructive">{error}</p> : null}

      <DialogFooter>
        <Button type="button" variant="outline" onClick={onDone}>
          Cancel
        </Button>
        <Button type="submit" disabled={pending}>
          {pending ? 'Saving…' : isEdit ? 'Save' : 'Add source'}
        </Button>
      </DialogFooter>
    </form>
  )
}
