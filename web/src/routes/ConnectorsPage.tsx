import { useMemo, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Eye, Pencil, Plug, Plus, Power, RefreshCw, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { checkConnectorHealth, type Connector } from '@/lib/connectors'
import {
  useAllConnectorTools,
  useConnectorsList,
  useDeleteConnector,
  useMcpCatalog,
  useMcpsUsage,
  useSetConnectorEnabled,
} from '@/lib/queries'
import type { CatalogEntry } from '@/lib/mcpCatalog'
import { periodLabel } from '@/lib/overview'
import { PeriodFilter } from '@/components/app/PeriodFilter'
import { usePeriod } from '@/hooks/use-period'
import {
  LIFECYCLE_STATES,
  mergeMcpRows,
  type LifecycleState,
  type McpLifecycleRow,
} from '@/lib/lifecycle'
import { formatCompactNumber } from '@/lib/overview'
import { formatDate } from '@/lib/utils'
import {
  AnalyticsOffNote,
  LifecyclePill,
  StateFilter,
  ViaGatewayBadge,
} from '@/components/app/LifecycleBits'
import { CatalogAddDialog } from '@/components/app/connectors/CatalogAddDialog'
import { StatCard } from '@/components/app/StatCard'
import {
  DataTable,
  DataTablePage,
  type ColumnDef,
  type RowAction,
} from '@/components/app/data-table'
import { ConfirmDialog } from '@/components/app/ConfirmDialog'
import { StatusPill } from '@/components/app/StatusPill'
import { ConnectorFormDialog } from '@/components/app/connectors/ConnectorFormDialog'
import { ConnectorDetailDialog } from '@/components/app/connectors/ConnectorDetailDialog'
import { Button } from '@/components/ui/button'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { CatalogTab } from '@/components/app/connectors/CatalogTab'
import { useAuthOptional } from '@/auth/AuthContext'

function statusTone(status: string): 'positive' | 'negative' | 'neutral' {
  if (status === 'healthy') return 'positive'
  if (status === 'unhealthy') return 'negative'
  return 'neutral'
}

export default function ConnectorsPage() {
  const connectorsQuery = useConnectorsList()
  const canManageCatalog = !!useAuthOptional()?.principal?.roles.includes('admin')
  const deleteConnector = useDeleteConnector()
  const connectors = useMemo(() => connectorsQuery.data?.items ?? [], [connectorsQuery.data])
  const toolsQueries = useAllConnectorTools(connectors)

  const [tab, setTab] = useState<'mcps' | 'catalog'>('mcps')
  const [period, setPeriod] = usePeriod('7d')
  // Column and tile labels: "7d", or the custom dates.
  const range = period.range === 'custom' ? periodLabel(period) : period.range
  const [stateFilter, setStateFilter] = useState<LifecycleState | 'all'>('all')
  const usageQuery = useMcpsUsage(period)
  const hasDiscovered = (usageQuery.data ?? []).some((u) => !u.registered_connector_slug)
  const catalogQuery = useMcpCatalog(hasDiscovered)
  const setEnabled = useSetConnectorEnabled()
  const [createOpen, setCreateOpen] = useState(false)
  // Register a Discovered server: from its catalog entry, or as a custom MCP
  // with the observed name prefilled.
  const [addFromCatalog, setAddFromCatalog] = useState<CatalogEntry | null>(null)
  const [registerName, setRegisterName] = useState<string | null>(null)
  const [editTarget, setEditTarget] = useState<Connector | null>(null)
  const [viewTarget, setViewTarget] = useState<Connector | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<Connector | null>(null)

  const queryClient = useQueryClient()
  const healthCheck = useMutation({
    mutationFn: (id: string) => checkConnectorHealth(id),
    onSuccess: (result, id) => {
      const connector = connectors.find((c) => c.id === id)
      if (result === null) {
        toast.message('Health check not available yet on this gateway.')
      } else {
        toast.success(`${connector?.name ?? id}: ${result.status}`)
      }
      // The probe persists the connector's status; refetch so the table's
      // status pill reflects it without a reload.
      void queryClient.invalidateQueries({ queryKey: ['connectors'] })
    },
    onError: (err) => {
      toast.error(err instanceof Error ? err.message : 'Health check failed')
    },
  })

  const usageLoading = usageQuery.isLoading
  const analyticsUnavailable = !usageLoading && usageQuery.data === null
  const catalogEntries = useMemo(() => catalogQuery.data?.items ?? [], [catalogQuery.data])
  const allRows = useMemo(
    () => mergeMcpRows(connectors, usageQuery.data ?? null, catalogEntries),
    [connectors, usageQuery.data, catalogEntries],
  )
  const stateCounts = useMemo(() => {
    const counts: Partial<Record<LifecycleState, number>> = {}
    for (const r of allRows) counts[r.state] = (counts[r.state] ?? 0) + 1
    return counts
  }, [allRows])
  const rows = useMemo(
    () => (stateFilter === 'all' ? allRows : allRows.filter((r) => r.state === stateFilter)),
    [allRows, stateFilter],
  )
  const stateByConnectorId = useMemo(() => {
    const m = new Map<string, LifecycleState>()
    for (const r of allRows) if (r.connector) m.set(r.connector.id, r.state)
    return m
  }, [allRows])

  const total = connectorsQuery.data?.total ?? 0
  const healthyCount = connectors.filter((c) => c.status === 'healthy').length
  const totalTools = toolsQueries.reduce((sum, q) => sum + (q.data?.total ?? 0), 0)
  const toolCountByConnector = new Map(
    connectors.map((c, i) => [c.id, toolsQueries[i]?.data?.total]),
  )

  function toggleEnabled(connector: Connector, enabled: boolean) {
    setEnabled.mutate(
      { connector, enabled },
      {
        onSuccess: () => toast.success(`${connector.name} ${enabled ? 'enabled' : 'disabled'}`),
        onError: (err) =>
          toast.error(err instanceof Error ? err.message : 'Failed to update MCP'),
      },
    )
  }

  function handleDelete() {
    if (!deleteTarget) return
    deleteConnector.mutate(deleteTarget.id, {
      onSuccess: () => {
        toast.success('MCP deleted')
        setDeleteTarget(null)
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to delete MCP')
      },
    })
  }

  function registerDiscovered(r: McpLifecycleRow) {
    if (r.catalogMatch) setAddFromCatalog(r.catalogMatch)
    else setRegisterName(r.name)
  }

  const columns: ColumnDef<McpLifecycleRow>[] = [
    {
      id: 'name',
      accessorKey: 'name',
      header: 'Name',
      size: 190,
      cell: ({ row }) => {
        const c = row.original.connector
        if (!c) {
          return (
            <div>
              <div className="truncate font-mono text-[13px] font-semibold text-foreground">
                {row.original.name}
              </div>
              <div className="truncate text-[11.5px] text-text-subtle">
                {row.original.tools.length} tools seen
              </div>
            </div>
          )
        }
        const toolCount = toolCountByConnector.get(c.id)
        return (
          <div>
            <div className="truncate text-[13.5px] font-semibold text-foreground">
              {c.name}
            </div>
            <div className="truncate text-[11.5px] text-text-subtle">
              {c.slug}
              {toolCount !== undefined ? ` · ${toolCount} tools` : ''}
            </div>
          </div>
        )
      },
    },
    {
      id: 'state',
      accessorFn: (r) => r.state,
      header: 'State',
      size: 250,
      cell: ({ row }) => (
        <div className="flex items-center gap-2">
          <LifecyclePill state={row.original.state} />
          <ViaGatewayBadge viaGateway={row.original.via_gateway} direct={row.original.direct} />
        </div>
      ),
    },
    {
      id: 'action',
      header: 'Action',
      size: 150,
      enableSorting: false,
      cell: ({ row }) =>
        row.original.state === 'discovered' ? (
          <Button
            size="xs"
            variant="outline"
            className="shrink-0 whitespace-nowrap"
            aria-label={`${row.original.catalogMatch ? 'Add from catalog' : 'Add MCP'}: ${row.original.name}`}
            onClick={(e) => {
              e.stopPropagation()
              registerDiscovered(row.original)
            }}
          >
            {row.original.catalogMatch ? 'Add from catalog' : 'Add MCP'}
          </Button>
        ) : null,
    },
    {
      id: 'endpoint',
      accessorFn: (r) => r.connector?.endpoint ?? '',
      header: 'Endpoint',
      size: 170,
      cell: ({ row }) => (
        <span
          className="block truncate font-mono text-xs text-text-muted"
          title={row.original.connector?.endpoint ?? undefined}
        >
          {row.original.connector?.endpoint ?? '—'}
        </span>
      ),
    },
    {
      id: 'status',
      accessorFn: (r) => r.connector?.status ?? '',
      header: 'Health',
      size: 110,
      cell: ({ row }) =>
        row.original.connector ? (
          <StatusPill
            tone={statusTone(row.original.connector.status)}
            label={row.original.connector.status}
          />
        ) : (
          <span className="text-text-subtle">—</span>
        ),
    },
    {
      id: 'calls',
      accessorFn: (r) => r.calls,
      header: `Calls (${range})`,
      size: 100,
      cell: ({ row }) => (
        <span className="tabular-nums">{formatCompactNumber(row.original.calls)}</span>
      ),
    },
    {
      id: 'used_by',
      accessorFn: (r) => r.used_by,
      header: 'Used by',
      size: 110,
      cell: ({ row }) => {
        const n = row.original.used_by
        return (
          <span className="text-text-muted">
            {n > 0 ? `${n} ${n === 1 ? 'key' : 'keys'}` : '—'}
          </span>
        )
      },
    },
    {
      id: 'last_seen',
      accessorFn: (r) => r.last_seen ?? '',
      header: 'Last seen',
      meta: { tooltip: false },
      cell: ({ row }) => (
        <span className="text-text-muted">{formatDate(row.original.last_seen ?? undefined)}</span>
      ),
    },
    {
      id: 'timeout_ms',
      accessorFn: (r) => r.connector?.timeout_ms ?? 0,
      header: 'Timeout',
      size: 110,
      cell: ({ row }) => (
        <span className="text-text-muted tabular-nums">
          {row.original.connector ? `${row.original.connector.timeout_ms}ms` : '—'}
        </span>
      ),
    },
    {
      id: 'created_at',
      accessorFn: (r) => r.connector?.created_at ?? '',
      header: 'Created',
      meta: { tooltip: false },
      cell: ({ row }) => (
        <span className="text-text-muted">
          {row.original.connector ? formatDate(row.original.connector.created_at) : '—'}
        </span>
      ),
    },
  ]

  const isConnector = (r: McpLifecycleRow) => r.connector !== null
  const rowActions: RowAction<McpLifecycleRow>[] = [
    {
      label: 'Add from catalog',
      icon: <Plug aria-hidden="true" />,
      hidden: (r) => r.state !== 'discovered' || !r.catalogMatch,
      onClick: registerDiscovered,
    },
    {
      label: 'Add MCP',
      icon: <Plus aria-hidden="true" />,
      hidden: (r) => r.state !== 'discovered' || !!r.catalogMatch,
      onClick: registerDiscovered,
    },
    {
      label: 'View',
      icon: <Eye aria-hidden="true" />,
      hidden: (r) => !isConnector(r),
      onClick: (r) => setViewTarget(r.connector),
    },
    {
      label: 'Edit',
      icon: <Pencil aria-hidden="true" />,
      hidden: (r) => !isConnector(r),
      onClick: (r) => setEditTarget(r.connector),
    },
    {
      label: 'Disable',
      icon: <Power aria-hidden="true" />,
      hidden: (r) => !r.connector || r.state === 'disabled',
      disabled: () => setEnabled.isPending,
      onClick: (r) => r.connector && toggleEnabled(r.connector, false),
    },
    {
      label: 'Enable',
      icon: <Power aria-hidden="true" />,
      hidden: (r) => r.state !== 'disabled',
      disabled: () => setEnabled.isPending,
      onClick: (r) => r.connector && toggleEnabled(r.connector, true),
    },
    {
      label: 'Health check',
      icon: <RefreshCw aria-hidden="true" />,
      hidden: (r) => !isConnector(r),
      disabled: () => healthCheck.isPending,
      onClick: (r) => r.connector && healthCheck.mutate(r.connector.id),
    },
    {
      label: 'Delete',
      icon: <Trash2 aria-hidden="true" />,
      variant: 'destructive',
      separatorBefore: true,
      hidden: (r) => !isConnector(r),
      onClick: (r) => setDeleteTarget(r.connector),
    },
  ]

  return (
    <DataTablePage>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
            MCPs ({total})
          </h1>
          <p className="text-[13px] text-text-subtle">
            MCP tool servers the gateway fronts. Agents only see tools granted through a
            profile.
          </p>
        </div>
        <div className="flex items-center gap-2">
          {tab === 'mcps' ? <PeriodFilter value={period} onChange={setPeriod} /> : null}
          <Button onClick={() => setCreateOpen(true)}>
            <Plus className="size-4" aria-hidden="true" />
            Add MCP
          </Button>
        </div>
      </div>

      <Tabs value={tab} onValueChange={(v) => setTab(v as 'mcps' | 'catalog')}>
        <TabsList variant="line">
          <TabsTrigger value="mcps">Registered</TabsTrigger>
          <TabsTrigger value="catalog">Catalog</TabsTrigger>
        </TabsList>
      </Tabs>

      {tab === 'catalog' ? (
        <CatalogTab
          canManage={canManageCatalog}
          onAdded={() => setTab('mcps')}
          stateByConnectorId={stateByConnectorId}
        />
      ) : (
        <>
          <div className="grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-4">
            <StatCard dotClassName="bg-primary" title="Total MCPs">
              <span className="text-[26px] leading-tight font-bold text-foreground">
                {total}
              </span>
            </StatCard>
            <StatCard dotClassName="bg-status-resolved" title="Healthy">
              <span className="text-[26px] leading-tight font-bold text-foreground">
                {healthyCount}
              </span>
            </StatCard>
            <StatCard dotClassName="bg-brand-cyan-to" title="Total Tools">
              <span className="text-[26px] leading-tight font-bold text-foreground">
                {totalTools}
              </span>
            </StatCard>
          </div>

          <div className="flex flex-wrap items-center gap-2">
            <StateFilter
              value={stateFilter}
              onChange={setStateFilter}
              states={LIFECYCLE_STATES.filter(
                (st) => st !== 'available' && (!analyticsUnavailable || st !== 'discovered'),
              )}
              counts={stateCounts}
            />
          </div>

          <DataTable
            storageKey="connectors"
            storageVersion={3}
            columns={columns}
            data={rows}
            getRowId={(r) => r.key}
            getRowLabel={(r) => r.name}
            rowActions={rowActions}
            onRowClick={(r) => (r.connector ? setViewTarget(r.connector) : registerDiscovered(r))}
            loading={connectorsQuery.isLoading}
            error={connectorsQuery.isError && "Couldn't load MCPs."}
            emptyTitle="No MCPs yet"
            emptyDescription="Connect an MCP server to give your agents tools."
          />
          {analyticsUnavailable ? (
            <AnalyticsOffNote />
          ) : (
            <p className="px-1 text-xs text-text-subtle">
              Usage observed from gateway traffic over the selected range. Discovered servers
              were used by a model but are not registered. State and health are separate.
            </p>
          )}
        </>
      )}

      <ConnectorFormDialog open={createOpen} onOpenChange={setCreateOpen} />
      <ConnectorFormDialog
        open={registerName !== null}
        onOpenChange={(open) => !open && setRegisterName(null)}
        initialName={registerName ?? ''}
      />
      <CatalogAddDialog
        entry={addFromCatalog}
        onOpenChange={(open) => !open && setAddFromCatalog(null)}
      />
      <ConnectorFormDialog
        open={editTarget !== null}
        onOpenChange={(open) => !open && setEditTarget(null)}
        connector={editTarget}
      />
      <ConnectorDetailDialog
        connector={viewTarget}
        onOpenChange={(open) => !open && setViewTarget(null)}
        onEdit={(c) => {
          setViewTarget(null)
          setEditTarget(c)
        }}
      />
      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        title={`Delete "${deleteTarget?.name}"?`}
        description="Profiles granting this MCP's tools will lose access. This can't be undone."
        confirmLabel="Delete"
        destructive
        isLoading={deleteConnector.isPending}
        onConfirm={handleDelete}
      />
    </DataTablePage>
  )
}
