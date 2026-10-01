import { useMemo, useState } from 'react'
import { Database, Lock, Pencil, Plus, Trash2, Unplug } from 'lucide-react'
import { Link } from 'react-router-dom'
import { toast } from 'sonner'
import { ApiError } from '@/lib/api'
import {
  useDeleteCredential,
  useDeleteModel,
  useModelCatalogSummary,
  useModelsRegistryList,
  useModelsSummary,
} from '@/lib/queries'
import { mergeModelRows, type MergedModelRow, type RegisteredModel } from '@/lib/models'
import { mergeModelCatalogRows, type CatalogProvider } from '@/lib/model-catalog'
import { formatCompactNumber, formatCurrency } from '@/lib/overview'
import { StatCard } from '@/components/app/StatCard'
import { EmptyState } from '@/components/app/EmptyState'
import {
  DataTable,
  DataTablePage,
  type ColumnDef,
  type RowAction,
} from '@/components/app/data-table'
import { ConfirmDialog } from '@/components/app/ConfirmDialog'
import { StatusPill } from '@/components/app/StatusPill'
import { lifecycleLabel, lifecycleOutline, lifecycleTone } from '@/lib/lifecycle'
import { ModelFormDialog } from '@/components/app/models/ModelFormDialog'
import { ConnectProviderDialog } from '@/components/app/models/ConnectProviderDialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'

/** Shown on the "Discovered"/"Available" pills only -- the label alone
 * doesn't say why an unregistered name has traffic, or what "available"
 * means here. */
function statusTitle(status: MergedModelRow['status']): string | undefined {
  if (status === 'discovered') {
    return 'Seen in traffic but not registered. Calls pass straight through to the vendor.'
  }
  if (status === 'available') {
    return "A Model Catalog model you haven't set up yet. Set it up to make it callable."
  }
  return undefined
}

/** Full-page state shown when the model registry (`GET /api/v1/models`)
 * 403s — the signed-in principal is an agent key, not an admin. Mirrors
 * `ApiKeysPage`'s own admin-required box. */
function AdminRequiredNotice() {
  return (
    <div className="flex flex-col items-center gap-3 rounded-r-5 border border-border bg-card px-6 py-14 text-center">
      <span className="flex size-[52px] items-center justify-center rounded-r-5 bg-bg-muted text-text-muted">
        <Lock className="size-[22px]" aria-hidden="true" />
      </span>
      <div className="text-base font-semibold text-foreground">Admin access required</div>
      <p className="max-w-[46ch] text-[13.5px] text-text-subtle">
        These settings need an admin key. You&apos;re signed in with an agent key.
      </p>
    </div>
  )
}

/** Full-page state shown when there's nothing to show (no registered
 * models, no observed traffic) AND the reason is that ClickHouse isn't
 * enabled — the registry itself has no rows to fall back on either, so a
 * plain "no data yet" table would hide the actionable reason. Once the
 * registry has rows, or traffic starts flowing, the normal table +
 * merged rows take over instead of this. */
function AnalyticsOffNotice() {
  return (
    <div className="flex flex-col items-center gap-3 rounded-r-5 border border-border bg-card px-6 py-14 text-center">
      <span className="flex size-[52px] items-center justify-center rounded-r-5 bg-sev-medium-bg text-sev-medium-fg">
        <Database className="size-[22px]" aria-hidden="true" />
      </span>
      <div className="text-base font-semibold text-foreground">Analytics are turned off</div>
      <p className="max-w-[52ch] text-[13.5px] text-text-subtle">
        Analytics require ClickHouse. You&apos;re running on Postgres only — enable
        ClickHouse to see logs and usage.
      </p>
      <Link to="/docs" className="text-[13px] font-semibold text-text-link">
        View docs →
      </Link>
    </div>
  )
}

export default function ModelsPage() {
  const registryQuery = useModelsRegistryList()
  const summaryQuery = useModelsSummary()
  const catalogQuery = useModelCatalogSummary()
  const deleteModel = useDeleteModel()
  const deleteCredential = useDeleteCredential()

  const [createOpen, setCreateOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<RegisteredModel | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<RegisteredModel | null>(null)
  // Which currently-orphaned credentials (see `credentialUsage` below) the
  // user has opted, via checkbox, to delete along with the model. Reset
  // whenever the confirm dialog opens for a (possibly different) model.
  const [deleteOrphanCreds, setDeleteOrphanCreds] = useState<Set<string>>(new Set())
  const [connectTarget, setConnectTarget] = useState<{
    provider: CatalogProvider
    modelId: string
  } | null>(null)

  const isLoading = registryQuery.isLoading || summaryQuery.isLoading
  const isForbidden =
    registryQuery.error instanceof ApiError && registryQuery.error.status === 403
  // fetchModelsSummary swallows every failure (404 = no ClickHouse sink,
  // same contract as fetchOverviewMetrics) into `null` -- see its doc
  // comment for why that's the right default here.
  const analyticsUnavailable = !summaryQuery.isLoading && summaryQuery.data === null

  const registryItems = registryQuery.data?.items
  const usageModels = summaryQuery.data?.models
  const baseRows = useMemo(
    () => mergeModelRows(registryItems ?? [], usageModels ?? []),
    [registryItems, usageModels],
  )
  // fetchModelCatalogSafe (behind useModelCatalogSummary) resolves to null
  // on any failure -- a principal without model.read, or the catalog being
  // unreachable, just means no "Available" rows, not a broken page.
  const rows = useMemo(
    () => mergeModelCatalogRows(baseRows, catalogQuery.data),
    [baseRows, catalogQuery.data],
  )

  function openConnect(setup: { providerId: string; modelId: string } | undefined) {
    if (!setup) return
    const provider = catalogQuery.data?.providers.find((p) => p.id === setup.providerId)
    if (!provider) return
    setConnectTarget({ provider, modelId: setup.modelId })
  }

  function openDeleteConfirm(model: RegisteredModel | null) {
    setDeleteTarget(model)
    setDeleteOrphanCreds(new Set())
  }

  /** Every credential `deleteTarget`'s targets reference, and how many
   * OTHER registered models still use each one -- so the confirm dialog
   * can tell "still used elsewhere" (informational) apart from "about to
   * be orphaned" (offers a delete-it-too checkbox, default off). See
   * ADDENDUM 1 item 5, MODEL-CATALOG-CONTRACT.md. */
  const credentialUsage = useMemo(() => {
    if (!deleteTarget) return []
    const names = [
      ...new Set(
        deleteTarget.targets.map((t) => t.credential).filter((c): c is string => Boolean(c)),
      ),
    ]
    return names.map((name) => ({
      name,
      otherModels: (registryItems ?? []).filter(
        (m) => m.id !== deleteTarget.id && m.targets.some((t) => t.credential === name),
      ).length,
    }))
  }, [deleteTarget, registryItems])

  function toggleOrphanCred(name: string, checked: boolean) {
    setDeleteOrphanCreds((prev) => {
      const next = new Set(prev)
      if (checked) next.add(name)
      else next.delete(name)
      return next
    })
  }

  function handleDelete() {
    if (!deleteTarget) return
    const orphansToDelete = deleteOrphanCreds
    deleteModel.mutate(deleteTarget.id, {
      onSuccess: () => {
        toast.success('Model deleted')
        for (const name of orphansToDelete) {
          deleteCredential.mutate(name, {
            onError: (err) => {
              toast.error(
                `Couldn't delete credential "${name}": ${
                  err instanceof Error ? err.message : 'request failed'
                }`,
              )
            },
          })
        }
        openDeleteConfirm(null)
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to delete model')
      },
    })
  }

  const columns: ColumnDef<MergedModelRow>[] = [
    {
      id: 'name',
      accessorKey: 'name',
      header: 'Name',
      size: 240,
      cell: ({ row }) => (
        <div className="flex items-center gap-2">
          <span className="truncate font-mono text-[13px] font-semibold text-foreground">
            {row.original.name}
          </span>
          {row.original.registered?.scope === 'platform' ? (
            <Badge variant="outline" className="shrink-0 text-[10px] uppercase">
              platform
            </Badge>
          ) : null}
        </div>
      ),
    },
    {
      id: 'provider',
      accessorKey: 'provider',
      header: 'Provider',
      cell: ({ row }) => (
        <span className="text-text-muted">{row.original.provider || '—'}</span>
      ),
    },
    {
      id: 'calls',
      accessorKey: 'calls',
      header: 'Calls (7d)',
      size: 110,
      cell: ({ row }) => (
        <span className="tabular-nums">{formatCompactNumber(row.original.calls)}</span>
      ),
    },
    {
      id: 'tokens',
      accessorKey: 'tokens',
      header: 'Tokens (7d)',
      size: 120,
      cell: ({ row }) => (
        <span className="tabular-nums">{formatCompactNumber(row.original.tokens)}</span>
      ),
    },
    {
      id: 'cost',
      accessorFn: (r) => r.costUsd ?? undefined,
      header: 'Cost (7d)',
      size: 110,
      sortUndefined: 'last',
      cell: ({ row }) => (
        <span className="text-text-muted tabular-nums">
          {row.original.costUsd === null ? '—' : formatCurrency(row.original.costUsd)}
        </span>
      ),
    },
    {
      id: 'used_by',
      accessorKey: 'usedBy',
      header: 'Used by',
      size: 110,
      cell: ({ row }) => {
        const usedBy = row.original.usedBy
        return (
          <span className="text-text-muted">
            {usedBy > 0 ? `${usedBy} ${usedBy === 1 ? 'key' : 'keys'}` : '—'}
          </span>
        )
      },
    },
    {
      id: 'status',
      accessorKey: 'status',
      header: 'Status',
      size: 130,
      cell: ({ row }) => (
        <StatusPill
          tone={lifecycleTone(row.original.status)}
          label={lifecycleLabel(row.original.status)}
          title={statusTitle(row.original.status)}
          outline={lifecycleOutline(row.original.status)}
        />
      ),
    },
  ]

  // Platform-scope rows and traffic-only "discovered" rows (an
  // unregistered name the gateway is still passing through, `r.registered
  // === null`) have nothing editable here — see mergeModelRows and the
  // contract's scope rules. A discovered row has no registry record to
  // edit or delete, so it intentionally keeps no row actions at all (no
  // actions menu renders — see DataTableRowActions).
  const isReadOnly = (r: MergedModelRow) =>
    !r.registered || r.registered.scope === 'platform'

  const rowActions: RowAction<MergedModelRow>[] = [
    {
      label: 'Set up',
      icon: <Unplug aria-hidden="true" />,
      // "available" rows always carry catalogSetup (mergeModelCatalogRows);
      // a "discovered" row carries one only when its name/model id matched
      // a catalog model. Every other status never does.
      hidden: (r) => !r.catalogSetup,
      onClick: (r) => openConnect(r.catalogSetup),
    },
    {
      label: 'Edit',
      icon: <Pencil aria-hidden="true" />,
      hidden: isReadOnly,
      onClick: (r) => setEditTarget(r.registered),
    },
    {
      label: 'Delete',
      icon: <Trash2 aria-hidden="true" />,
      variant: 'destructive',
      separatorBefore: true,
      hidden: isReadOnly,
      onClick: (r) => openDeleteConfirm(r.registered),
    },
  ]

  const header = (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div>
        <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
          Models{!isLoading ? ` (${rows.length})` : ''}
        </h1>
        <p className="text-[13px] text-text-subtle">LLMs available through the gateway.</p>
      </div>
      {!isForbidden ? (
        <Button onClick={() => setCreateOpen(true)}>
          <Plus className="size-4" aria-hidden="true" />
          Add Model
        </Button>
      ) : null}
    </div>
  )

  if (!isLoading && isForbidden) {
    return (
      <div className="flex flex-col gap-4">
        {header}
        <AdminRequiredNotice />
      </div>
    )
  }

  if (!isLoading && rows.length === 0 && analyticsUnavailable) {
    return (
      <div className="flex flex-col gap-4">
        {header}
        <AnalyticsOffNotice />
      </div>
    )
  }

  return (
    <DataTablePage>
      {header}

      <div className="grid grid-cols-[repeat(auto-fit,minmax(200px,1fr))] gap-4">
        <StatCard dotClassName="bg-primary" title="Total Models">
          <span className="text-[26px] leading-tight font-bold text-foreground">
            {rows.length}
          </span>
        </StatCard>
        <StatCard dotClassName="bg-brand-cyan-to" title="Highest traffic (7d)">
          {summaryQuery.data?.highest_traffic ? (
            <>
              <span className="truncate font-mono text-[15px] font-semibold text-foreground">
                {summaryQuery.data.highest_traffic.name}
              </span>
              <span className="text-xs text-text-subtle">
                {formatCompactNumber(summaryQuery.data.highest_traffic.tokens)} tokens
              </span>
            </>
          ) : (
            <EmptyState label="No data yet" className="mx-0 text-left" />
          )}
        </StatCard>
      </div>

      <DataTable
        storageKey="models"
        columns={columns}
        data={rows}
        getRowId={(r) => r.key}
        getRowLabel={(r) => r.name}
        rowActions={rowActions}
        loading={isLoading}
        error={registryQuery.isError && !isForbidden && "Couldn't load models."}
        emptyTitle="No models seen yet"
        emptyDescription="Models appear here as agents route calls through the gateway."
      />
      <p className="px-1 text-xs text-text-subtle">
        Observed from gateway traffic over the last 7 days.
      </p>

      <ModelFormDialog open={createOpen} onOpenChange={setCreateOpen} />
      <ModelFormDialog
        open={editTarget !== null}
        onOpenChange={(open) => !open && setEditTarget(null)}
        model={editTarget}
      />
      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && openDeleteConfirm(null)}
        title={`Delete "${deleteTarget?.name}"?`}
        description={
          // ConfirmDialog's description renders inside Base UI's
          // AlertDialog.Description, which is a <p> -- only phrasing
          // content (span, not div/p) is valid nested inside it, so every
          // "block" below is a <span> laid out with `display: block`/
          // `flex` via Tailwind rather than an actual block element.
          <span className="flex flex-col gap-2 text-left">
            <span className="block">
              Agents calling this name fall back to the gateway's normal passthrough
              behavior. This can&apos;t be undone.
            </span>
            {credentialUsage.map(({ name, otherModels }) =>
              otherModels > 0 ? (
                <span key={name} className="block text-text-subtle">
                  The key <span className="font-mono text-foreground">{name}</span> is
                  still used by {otherModels} other model{otherModels === 1 ? '' : 's'}.
                </span>
              ) : (
                <span
                  key={name}
                  className="flex items-start gap-2 rounded-r-3 border border-sev-medium/40 bg-sev-medium-bg px-2.5 py-2 text-sev-medium-fg"
                >
                  <Checkbox
                    className="mt-0.5"
                    aria-label={`Delete credential ${name} too`}
                    checked={deleteOrphanCreds.has(name)}
                    onCheckedChange={(checked) => toggleOrphanCred(name, checked === true)}
                  />
                  <span>
                    <span className="font-mono">{name}</span> is no longer used by any
                    model — delete it too?
                  </span>
                </span>
              ),
            )}
          </span>
        }
        confirmLabel="Delete"
        destructive
        isLoading={deleteModel.isPending}
        onConfirm={handleDelete}
      />
      <ConnectProviderDialog
        provider={connectTarget?.provider ?? null}
        preselectModelId={connectTarget?.modelId}
        onOpenChange={(open) => !open && setConnectTarget(null)}
      />
    </DataTablePage>
  )
}
