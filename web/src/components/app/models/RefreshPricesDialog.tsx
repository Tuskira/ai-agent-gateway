import { useMemo, useState } from 'react'
import { toast } from 'sonner'
import { useApplyCatalogPrices, useCredentialsList, usePreviewCatalogPrices } from '@/lib/queries'
import {
  defaultCredentialName,
  type ApplyPricesItemInput,
  type CatalogProvider,
  type PreviewPricesInput,
  type PreviewPricesResult,
} from '@/lib/model-catalog'
import type { ModelPrice } from '@/lib/models'
import { formatCurrency } from '@/lib/overview'
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
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

interface RefreshPricesDialogProps {
  /** `null` closes the dialog. */
  provider: CatalogProvider | null
  onOpenChange: (open: boolean) => void
}

/** Outer shell: only mounts the body while a provider is set, keyed by its
 * id so switching providers resets the form -- same pattern as
 * `ConnectProviderDialog`. */
export function RefreshPricesDialog({ provider, onOpenChange }: RefreshPricesDialogProps) {
  return (
    <Dialog open={provider !== null} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[calc(100vh-3rem)] w-full flex-col overflow-hidden sm:max-w-2xl">
        {provider ? (
          <RefreshPricesBody
            key={provider.id}
            provider={provider}
            onDone={() => onOpenChange(false)}
            onCancel={() => onOpenChange(false)}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  )
}

type CredentialMode = 'existing' | 'new'

function priceText(p: ModelPrice | null): string {
  return p ? `${formatCurrency(p.input)} / ${formatCurrency(p.output)}` : '—'
}

interface RefreshPricesBodyProps {
  provider: CatalogProvider
  onDone: () => void
  onCancel: () => void
}

function RefreshPricesBody({ provider, onDone, onCancel }: RefreshPricesBodyProps) {
  // Nebius's pricing feed is public; every other registered source (today,
  // only together) needs a key -- see docs/models.md#refreshing-catalog-prices.
  const needsKey = provider.slug !== 'nebius'

  const credentialsQuery = useCredentialsList()
  const credentials = credentialsQuery.data?.items ?? []
  const preferredCredentialName = defaultCredentialName(provider.slug)
  const preferredCredential = credentials.find((c) => c.name === preferredCredentialName)

  const [modeOverride, setModeOverride] = useState<CredentialMode | null>(null)
  const [existingCredentialOverride, setExistingCredentialOverride] = useState<string | null>(
    null,
  )
  const mode: CredentialMode = modeOverride ?? (preferredCredential ? 'existing' : 'new')
  const existingCredentialName = existingCredentialOverride ?? preferredCredential?.name ?? ''
  const [apiKey, setApiKey] = useState('')

  const [preview, setPreview] = useState<PreviewPricesResult | null>(null)
  const [updateTenantModels, setUpdateTenantModels] = useState(true)

  const previewMutation = usePreviewCatalogPrices(provider.id)
  const applyMutation = useApplyCatalogPrices(provider.id)

  function handleFetch() {
    const input: PreviewPricesInput = {}
    if (needsKey) {
      if (mode === 'existing') {
        if (!existingCredentialName) {
          toast.error('Choose a credential first.')
          return
        }
        input.credential = existingCredentialName
      } else {
        if (!apiKey.trim()) {
          toast.error('Enter an API key first.')
          return
        }
        input.api_key = apiKey.trim()
      }
    }
    previewMutation.mutate(input, {
      onSuccess: setPreview,
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to fetch prices')
      },
    })
  }

  const changedItems = useMemo(() => preview?.items.filter((i) => i.changed) ?? [], [preview])

  function handleApply() {
    if (!preview || changedItems.length === 0) return
    const items: ApplyPricesItemInput[] = changedItems.map((i) => ({
      catalog_model_id: i.catalog_model_id,
      price: i.new_price,
    }))
    applyMutation.mutate(
      { items, update_tenant_models: updateTenantModels },
      {
        onSuccess: (result) => {
          toast.success(
            `Updated ${result.catalog_updated} catalog price${result.catalog_updated === 1 ? '' : 's'}` +
              (updateTenantModels
                ? `, ${result.tenant_updated} tenant model${result.tenant_updated === 1 ? '' : 's'}`
                : ''),
          )
          onDone()
        },
        onError: (err) => {
          toast.error(err instanceof Error ? err.message : 'Failed to apply prices')
        },
      },
    )
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <DialogHeader>
        <DialogTitle>Refresh prices — {provider.display_name}</DialogTitle>
        <DialogDescription>
          Fetches current prices from {provider.display_name}&apos;s own pricing API and shows
          the diff before anything is written.
        </DialogDescription>
      </DialogHeader>

      <div className="-mx-1 flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-1 py-4">
        {!preview ? (
          <div className="flex flex-col gap-3">
            {needsKey ? (
              <>
                <div className="flex gap-1.5">
                  <Button
                    type="button"
                    size="sm"
                    variant={mode === 'existing' ? 'default' : 'outline'}
                    disabled={credentials.length === 0}
                    onClick={() => setModeOverride('existing')}
                  >
                    Use existing credential
                  </Button>
                  <Button
                    type="button"
                    size="sm"
                    variant={mode === 'new' ? 'default' : 'outline'}
                    onClick={() => setModeOverride('new')}
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
                  <ApiKeyInput
                    aria-label={`${provider.display_name} API key`}
                    value={apiKey}
                    onChange={setApiKey}
                    helperText="Used only for this fetch -- never stored."
                  />
                )}
              </>
            ) : (
              <p className="text-sm text-text-subtle">
                {provider.display_name}&apos;s pricing feed is public — no key needed.
              </p>
            )}
          </div>
        ) : (
          <>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Model</TableHead>
                  <TableHead>Current (in/out)</TableHead>
                  <TableHead>New (in/out)</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {preview.items.map((item) => (
                  <TableRow
                    key={item.catalog_model_id}
                    className={item.changed ? 'bg-sev-medium-bg/40' : undefined}
                  >
                    <TableCell className="font-mono text-xs">{item.model_id}</TableCell>
                    <TableCell className="tabular-nums text-text-muted">
                      {priceText(item.current_price)}
                    </TableCell>
                    <TableCell className="tabular-nums font-medium text-foreground">
                      {item.new_price ? (
                        priceText(item.new_price)
                      ) : (
                        <span className="font-normal text-text-subtle">not found</span>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            {preview.unmatched_provider_models > 0 ? (
              <p className="text-xs text-text-subtle">
                {preview.unmatched_provider_models} model
                {preview.unmatched_provider_models === 1 ? '' : 's'} priced by{' '}
                {provider.display_name} {preview.unmatched_provider_models === 1 ? 'is' : 'are'}{' '}
                not in the catalog yet.
              </p>
            ) : null}
            <div className="flex items-center gap-2">
              <Checkbox
                id="refresh-prices-update-tenant-models"
                checked={updateTenantModels}
                onCheckedChange={(value) => setUpdateTenantModels(value === true)}
              />
              <Label
                htmlFor="refresh-prices-update-tenant-models"
                className="text-sm font-normal text-foreground"
              >
                Also update tenant models that still use the catalog price
              </Label>
            </div>
          </>
        )}
      </div>

      <DialogFooter>
        <Button type="button" variant="outline" onClick={onCancel}>
          Cancel
        </Button>
        {!preview ? (
          <Button type="button" disabled={previewMutation.isPending} onClick={handleFetch}>
            {previewMutation.isPending ? 'Fetching…' : 'Fetch prices'}
          </Button>
        ) : (
          <Button
            type="button"
            disabled={applyMutation.isPending || changedItems.length === 0}
            onClick={handleApply}
          >
            {applyMutation.isPending
              ? 'Applying…'
              : `Apply ${changedItems.length} change${changedItems.length === 1 ? '' : 's'}`}
          </Button>
        )}
      </DialogFooter>
    </div>
  )
}
