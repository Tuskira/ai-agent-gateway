import { useState } from 'react'
import { DollarSign, ExternalLink, Lock, Pencil, Plus, ShieldAlert, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { useAuth } from '@/auth/AuthContext'
import { ApiError } from '@/lib/api'
import { useAuthConfig, useDeleteCatalogModel, useDeleteCatalogProvider, useModelCatalogList } from '@/lib/queries'
import {
  getCatalogModelUsage,
  supportsPriceRefresh,
  usageCountFromError,
  type CatalogModel,
  type CatalogProvider,
} from '@/lib/model-catalog'
import { formatCurrency } from '@/lib/overview'
import {
  DataTable,
  DataTablePage,
  type ColumnDef,
  type RowAction,
} from '@/components/app/data-table'
import { ConfirmDialog } from '@/components/app/ConfirmDialog'
import { StatusPill } from '@/components/app/StatusPill'
import { CatalogProviderFormDialog } from '@/components/app/models/CatalogProviderFormDialog'
import { CatalogModelFormDialog } from '@/components/app/models/CatalogModelFormDialog'
import { RefreshPricesDialog } from '@/components/app/models/RefreshPricesDialog'
import { Button } from '@/components/ui/button'

/** True when this principal may write to the catalog. Mirrors the
 * contract's permission rule (`MODEL-CATALOG-CONTRACT.md`, "Permissions"):
 * an admin API key always can; an admin console session only when the
 * gateway runs single-tenant (multi-tenant keeps catalog writes API-key-
 * admin only). Purely a UI convenience — the API enforces this for real,
 * and every mutation here still surfaces its own 403 inline. */
function useCanManageCatalog(): boolean {
  const { principal } = useAuth()
  const authConfigQuery = useAuthConfig()
  const isAdmin = principal?.roles.includes('admin') ?? false
  if (!isAdmin) return false
  if (principal?.kind === 'user') return authConfigQuery.data?.single_tenant === true
  return true
}

function AdminRequiredNotice() {
  return (
    <div className="flex flex-col items-center gap-3 rounded-r-5 border border-border bg-card px-6 py-14 text-center">
      <span className="flex size-[52px] items-center justify-center rounded-r-5 bg-bg-muted text-text-muted">
        <Lock className="size-[22px]" aria-hidden="true" />
      </span>
      <div className="text-base font-semibold text-foreground">Admin access required</div>
      <p className="max-w-[46ch] text-[13.5px] text-text-subtle">
        Viewing the model catalog needs an account with model read access.
      </p>
    </div>
  )
}

function ReadOnlyBanner() {
  return (
    <div className="flex items-center gap-3 rounded-r-4 border border-sev-medium/40 bg-sev-medium-bg px-3.5 py-3 text-[13px] text-sev-medium-fg">
      <ShieldAlert className="size-[18px] shrink-0" aria-hidden="true" />
      <span className="flex-1">
        Only platform admins can change the catalog. You can view it, but adding, editing and
        deleting are turned off.
      </span>
    </div>
  )
}

function capabilityText(value: boolean | null): string {
  if (value === true) return 'Yes'
  if (value === false) return 'No'
  return 'Unknown'
}

interface ProviderModelsPanelProps {
  provider: CatalogProvider
  canManage: boolean
  onAdd: (provider: CatalogProvider) => void
  onEdit: (provider: CatalogProvider, model: CatalogModel) => void
  onDelete: (provider: CatalogProvider, model: CatalogModel) => void
}

function ProviderModelsPanel({
  provider,
  canManage,
  onAdd,
  onEdit,
  onDelete,
}: ProviderModelsPanelProps) {
  const columns: ColumnDef<CatalogModel>[] = [
    {
      id: 'name',
      accessorKey: 'suggested_name',
      header: 'Name',
      size: 200,
      cell: ({ row }) => (
        <div className="flex flex-col">
          <span className="font-mono text-[12.5px] font-semibold text-foreground">
            {row.original.suggested_name}
          </span>
          <span className="text-[11px] text-text-subtle">{row.original.model_id}</span>
        </div>
      ),
    },
    {
      id: 'display_name',
      accessorKey: 'display_name',
      header: 'Display name',
      cell: ({ row }) => (
        <span className="text-text-muted">{row.original.display_name || '—'}</span>
      ),
    },
    {
      id: 'price',
      accessorFn: (m) => m.price?.input ?? undefined,
      header: 'Price (in/out)',
      sortUndefined: 'last',
      cell: ({ row }) => {
        const price = row.original.price
        return (
          <span className="tabular-nums text-text-muted">
            {price ? `${formatCurrency(price.input)} / ${formatCurrency(price.output)}` : '—'}
          </span>
        )
      },
    },
    {
      id: 'capabilities',
      header: 'Capabilities',
      enableSorting: false,
      cell: ({ row }) => {
        const c = row.original.capabilities
        return (
          <div className="flex flex-col gap-0.5">
            <span className="text-[11.5px] text-text-subtle">
              Tools: {capabilityText(c.tools)} · Vision: {capabilityText(c.vision)} · Streaming:{' '}
              {capabilityText(c.streaming)}
            </span>
            {row.original.notes ? (
              <span
                className="max-w-[32ch] truncate text-[11px] text-text-muted"
                title={row.original.notes}
              >
                {row.original.notes}
              </span>
            ) : null}
          </div>
        )
      },
    },
    {
      id: 'status',
      header: 'Status',
      size: 170,
      enableSorting: false,
      cell: ({ row }) =>
        row.original.tenant_model_id ? (
          <StatusPill
            tone="info"
            label="Connected"
            title={`Registered as ${row.original.tenant_model_name}`}
          />
        ) : (
          <StatusPill tone="neutral" label="Not connected" />
        ),
    },
    {
      id: 'enabled',
      accessorKey: 'enabled',
      header: 'Enabled',
      size: 90,
      cell: ({ row }) => (
        <StatusPill
          tone={row.original.enabled ? 'positive' : 'neutral'}
          label={row.original.enabled ? 'Yes' : 'No'}
        />
      ),
    },
  ]

  const rowActions: RowAction<CatalogModel>[] = [
    { label: 'Edit', icon: <Pencil aria-hidden="true" />, onClick: (m) => onEdit(provider, m) },
    {
      label: 'Delete',
      icon: <Trash2 aria-hidden="true" />,
      variant: 'destructive',
      separatorBefore: true,
      onClick: (m) => onDelete(provider, m),
    },
  ]

  return (
    <div className="flex flex-col gap-3 p-3">
      <div className="flex items-center justify-between">
        <span className="text-[13px] font-semibold text-foreground">
          Models ({provider.models.length})
        </span>
        {canManage ? (
          <Button type="button" variant="outline" size="sm" onClick={() => onAdd(provider)}>
            <Plus className="size-3.5" aria-hidden="true" />
            Add model
          </Button>
        ) : null}
      </div>
      <DataTable
        storageKey={`model-catalog-models-${provider.id}`}
        columns={columns}
        data={provider.models}
        getRowId={(m) => m.id}
        getRowLabel={(m) => m.suggested_name}
        rowActions={canManage ? rowActions : undefined}
        showToolbar={false}
        showTotalCount={false}
        enableColumnVisibility={false}
        enableColumnResize={false}
        emptyTitle="No models in this provider yet"
      />
    </div>
  )
}

export default function ModelCatalogPage() {
  const canManage = useCanManageCatalog()
  const catalogQuery = useModelCatalogList()
  const deleteProvider = useDeleteCatalogProvider()
  const deleteModel = useDeleteCatalogModel()

  const [createProviderOpen, setCreateProviderOpen] = useState(false)
  const [editProviderTarget, setEditProviderTarget] = useState<CatalogProvider | null>(null)
  const [deleteProviderTarget, setDeleteProviderTarget] = useState<CatalogProvider | null>(null)
  const [providerUsageConfirm, setProviderUsageConfirm] = useState<{
    provider: CatalogProvider
    count: number | null
  } | null>(null)

  const [refreshPricesProvider, setRefreshPricesProvider] = useState<CatalogProvider | null>(null)

  const [modelDialogProvider, setModelDialogProvider] = useState<CatalogProvider | null>(null)
  const [editModelTarget, setEditModelTarget] = useState<{
    provider: CatalogProvider
    model: CatalogModel
  } | null>(null)
  const [deleteModelTarget, setDeleteModelTarget] = useState<{
    provider: CatalogProvider
    model: CatalogModel
  } | null>(null)
  const [modelUsageConfirm, setModelUsageConfirm] = useState<{
    model: CatalogModel
    count: number | null
  } | null>(null)

  const isForbidden = catalogQuery.error instanceof ApiError && catalogQuery.error.status === 403
  const providers = catalogQuery.data?.providers ?? []

  function handleDeleteProvider() {
    const target = deleteProviderTarget
    if (!target) return
    deleteProvider.mutate(
      { id: target.id },
      {
        onSuccess: () => {
          toast.success('Provider deleted')
          setDeleteProviderTarget(null)
        },
        onError: (err) => {
          if (err instanceof ApiError && err.status === 409) {
            setDeleteProviderTarget(null)
            setProviderUsageConfirm({ provider: target, count: usageCountFromError(err) })
            return
          }
          toast.error(err instanceof Error ? err.message : 'Failed to delete provider')
        },
      },
    )
  }

  function handleForceDeleteProvider() {
    const target = providerUsageConfirm?.provider
    if (!target) return
    deleteProvider.mutate(
      { id: target.id, force: true },
      {
        onSuccess: () => {
          toast.success('Provider deleted')
          setProviderUsageConfirm(null)
        },
        onError: (err) => {
          toast.error(err instanceof Error ? err.message : 'Failed to delete provider')
        },
      },
    )
  }

  function handleDeleteModel() {
    const target = deleteModelTarget?.model
    if (!target) return
    deleteModel.mutate(
      { id: target.id },
      {
        onSuccess: () => {
          toast.success('Model deleted')
          setDeleteModelTarget(null)
        },
        onError: async (err) => {
          if (err instanceof ApiError && err.status === 409) {
            setDeleteModelTarget(null)
            let count = usageCountFromError(err)
            if (count === null) {
              try {
                count = (await getCatalogModelUsage(target.id)).tenant_models
              } catch {
                // leave count null -- the confirm dialog words it generically.
              }
            }
            setModelUsageConfirm({ model: target, count })
            return
          }
          toast.error(err instanceof Error ? err.message : 'Failed to delete model')
        },
      },
    )
  }

  function handleForceDeleteModel() {
    const target = modelUsageConfirm?.model
    if (!target) return
    deleteModel.mutate(
      { id: target.id, force: true },
      {
        onSuccess: () => {
          toast.success('Model deleted')
          setModelUsageConfirm(null)
        },
        onError: (err) => {
          toast.error(err instanceof Error ? err.message : 'Failed to delete model')
        },
      },
    )
  }

  const providerColumns: ColumnDef<CatalogProvider>[] = [
    {
      id: 'display_name',
      accessorKey: 'display_name',
      header: 'Provider',
      size: 220,
      cell: ({ row }) => (
        <div className="flex flex-col">
          <span className="font-semibold text-foreground">{row.original.display_name}</span>
          <span className="font-mono text-[11px] text-text-subtle">{row.original.slug}</span>
        </div>
      ),
    },
    {
      id: 'base_url',
      accessorKey: 'base_url',
      header: 'Base URL',
      cell: ({ row }) => (
        <span className="truncate font-mono text-[12px] text-text-muted">
          {row.original.base_url}
        </span>
      ),
    },
    {
      id: 'docs_url',
      header: 'Docs',
      enableSorting: false,
      size: 90,
      cell: ({ row }) =>
        row.original.docs_url ? (
          <a
            href={row.original.docs_url}
            target="_blank"
            rel="noreferrer"
            className="inline-flex items-center gap-1 text-text-link hover:underline"
          >
            <ExternalLink className="size-3.5" aria-hidden="true" />
          </a>
        ) : (
          <span className="text-text-subtle">—</span>
        ),
    },
    {
      id: 'models',
      accessorFn: (p) => p.models.length,
      header: 'Models',
      size: 90,
    },
    {
      id: 'enabled',
      accessorKey: 'enabled',
      header: 'Enabled',
      size: 100,
      cell: ({ row }) => (
        <StatusPill
          tone={row.original.enabled ? 'positive' : 'neutral'}
          label={row.original.enabled ? 'Yes' : 'No'}
        />
      ),
    },
  ]

  const providerRowActions: RowAction<CatalogProvider>[] = [
    { label: 'Edit', icon: <Pencil aria-hidden="true" />, onClick: setEditProviderTarget },
    {
      label: 'Refresh prices',
      icon: <DollarSign aria-hidden="true" />,
      onClick: setRefreshPricesProvider,
      // Only for a provider with a registered price source (nebius,
      // together) -- see internal/modelcatalog/prices.
      hidden: (p) => !supportsPriceRefresh(p.slug),
    },
    {
      label: 'Delete',
      icon: <Trash2 aria-hidden="true" />,
      variant: 'destructive',
      separatorBefore: true,
      onClick: setDeleteProviderTarget,
    },
  ]

  const header = (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div>
        <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
          Model catalog{!catalogQuery.isLoading ? ` (${providers.length})` : ''}
        </h1>
        <p className="text-[13px] text-text-subtle">
          Platform-level providers and models tenants can connect from the Models page.
        </p>
      </div>
      {canManage ? (
        <Button onClick={() => setCreateProviderOpen(true)}>
          <Plus className="size-4" aria-hidden="true" />
          Add provider
        </Button>
      ) : null}
    </div>
  )

  if (!catalogQuery.isLoading && isForbidden) {
    return (
      <div className="flex flex-col gap-4">
        {header}
        <AdminRequiredNotice />
      </div>
    )
  }

  return (
    <DataTablePage>
      {header}

      {!canManage ? <ReadOnlyBanner /> : null}

      <DataTable
        storageKey="model-catalog-providers"
        columns={providerColumns}
        data={providers}
        getRowId={(p) => p.id}
        getRowLabel={(p) => p.display_name}
        rowActions={canManage ? providerRowActions : undefined}
        loading={catalogQuery.isLoading}
        error={catalogQuery.isError && !isForbidden && "Couldn't load the model catalog."}
        emptyTitle="No providers yet"
        emptyDescription="Add a provider so tenants can connect it from the Models page."
        renderDetailPanel={(provider) => (
          <ProviderModelsPanel
            provider={provider}
            canManage={canManage}
            onAdd={setModelDialogProvider}
            onEdit={(provider, model) => setEditModelTarget({ provider, model })}
            onDelete={(provider, model) => setDeleteModelTarget({ provider, model })}
          />
        )}
      />

      <CatalogProviderFormDialog
        open={createProviderOpen}
        onOpenChange={setCreateProviderOpen}
      />
      <CatalogProviderFormDialog
        open={editProviderTarget !== null}
        onOpenChange={(open) => !open && setEditProviderTarget(null)}
        provider={editProviderTarget}
      />
      <ConfirmDialog
        open={deleteProviderTarget !== null}
        onOpenChange={(open) => !open && setDeleteProviderTarget(null)}
        title={`Delete "${deleteProviderTarget?.display_name}"?`}
        description="Removes every model under this provider from the catalog. Tenant models already connected from it keep working. This can't be undone."
        confirmLabel="Delete"
        destructive
        isLoading={deleteProvider.isPending}
        onConfirm={handleDeleteProvider}
      />
      <ConfirmDialog
        open={providerUsageConfirm !== null}
        onOpenChange={(open) => !open && setProviderUsageConfirm(null)}
        title={`"${providerUsageConfirm?.provider.display_name}" is in use`}
        description={
          providerUsageConfirm?.count !== null && providerUsageConfirm?.count !== undefined
            ? `Used by ${providerUsageConfirm.count} tenant model${providerUsageConfirm.count === 1 ? '' : 's'}. Delete anyway?`
            : 'One or more tenant models were created from this provider. Delete anyway?'
        }
        confirmLabel="Delete anyway"
        destructive
        isLoading={deleteProvider.isPending}
        onConfirm={handleForceDeleteProvider}
      />

      <RefreshPricesDialog
        provider={refreshPricesProvider}
        onOpenChange={(open) => !open && setRefreshPricesProvider(null)}
      />

      <CatalogModelFormDialog
        open={modelDialogProvider !== null}
        onOpenChange={(open) => !open && setModelDialogProvider(null)}
        providerId={modelDialogProvider?.id ?? ''}
        providerSlug={modelDialogProvider?.slug ?? ''}
      />
      <CatalogModelFormDialog
        open={editModelTarget !== null}
        onOpenChange={(open) => !open && setEditModelTarget(null)}
        providerId={editModelTarget?.provider.id ?? ''}
        providerSlug={editModelTarget?.provider.slug ?? ''}
        model={editModelTarget?.model}
      />
      <ConfirmDialog
        open={deleteModelTarget !== null}
        onOpenChange={(open) => !open && setDeleteModelTarget(null)}
        title={`Delete "${deleteModelTarget?.model.suggested_name}"?`}
        description="This can't be undone."
        confirmLabel="Delete"
        destructive
        isLoading={deleteModel.isPending}
        onConfirm={handleDeleteModel}
      />
      <ConfirmDialog
        open={modelUsageConfirm !== null}
        onOpenChange={(open) => !open && setModelUsageConfirm(null)}
        title={`"${modelUsageConfirm?.model.suggested_name}" is in use`}
        description={
          modelUsageConfirm?.count !== null && modelUsageConfirm?.count !== undefined
            ? `Used by ${modelUsageConfirm.count} tenant model${modelUsageConfirm.count === 1 ? '' : 's'}. Delete anyway?`
            : 'One or more tenant models were created from this catalog model. Delete anyway?'
        }
        confirmLabel="Delete anyway"
        destructive
        isLoading={deleteModel.isPending}
        onConfirm={handleForceDeleteModel}
      />
    </DataTablePage>
  )
}
