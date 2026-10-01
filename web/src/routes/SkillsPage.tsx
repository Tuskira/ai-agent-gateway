import { useMemo, useState } from 'react'
import { Database, Plus, Trash2 } from 'lucide-react'
import { Link } from 'react-router-dom'
import { toast } from 'sonner'
import {
  useDeleteSkill,
  useSkillsList,
  useSkillsSummary,
  useSkillsUsage,
} from '@/lib/queries'
import type { Skill } from '@/lib/skills'
import type { TimeRange } from '@/lib/overview'
import {
  LIFECYCLE_STATES,
  mergeSkillRows,
  type LifecycleState,
  type SkillLifecycleRow,
} from '@/lib/lifecycle'
import { formatCompactNumber } from '@/lib/overview'
import { formatDate } from '@/lib/utils'
import { StatCard } from '@/components/app/StatCard'
import { EmptyState } from '@/components/app/EmptyState'
import {
  DataTable,
  DataTablePage,
  type ColumnDef,
  type RowAction,
} from '@/components/app/data-table'
import { ConfirmDialog } from '@/components/app/ConfirmDialog'
import {
  AnalyticsOffNote,
  LifecyclePill,
  RangeSelect,
  StateFilter,
} from '@/components/app/LifecycleBits'
import { SkillFormDialog } from '@/components/app/skills/SkillFormDialog'
import { SkillDetailSheet } from '@/components/app/skills/SkillDetailSheet'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'

/** Full-page state shown when there's nothing to show (no skills or
 * commands registered) AND the reason is that ClickHouse isn't enabled --
 * mirrors `ModelsPage`'s own `AnalyticsOffNotice`. Once the registry has
 * rows, the normal table takes over instead, usage columns simply
 * reading 0/—/— until traffic starts flowing. */
function AnalyticsOffNotice() {
  return (
    <div className="flex flex-col items-center gap-3 rounded-r-5 border border-border bg-card px-6 py-14 text-center">
      <span className="flex size-[52px] items-center justify-center rounded-r-5 bg-sev-medium-bg text-sev-medium-fg">
        <Database className="size-[22px]" aria-hidden="true" />
      </span>
      <div className="text-base font-semibold text-foreground">Analytics are turned off</div>
      <p className="max-w-[52ch] text-[13.5px] text-text-subtle">
        Analytics require ClickHouse. You&apos;re running on Postgres only — enable
        ClickHouse to see usage.
      </p>
      <Link to="/docs" className="text-[13px] font-semibold text-text-link">
        View docs →
      </Link>
    </div>
  )
}

export default function SkillsPage() {
  const [range, setRange] = useState<TimeRange>('7d')
  const [stateFilter, setStateFilter] = useState<LifecycleState | 'all'>('all')
  const skillsQuery = useSkillsList()
  const summaryQuery = useSkillsSummary(range)
  const usageQuery = useSkillsUsage(range)
  const deleteSkill = useDeleteSkill()

  const [createOpen, setCreateOpen] = useState(false)
  const [registerName, setRegisterName] = useState<string | null>(null)
  const [detailTarget, setDetailTarget] = useState<Skill | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<Skill | null>(null)

  const items = useMemo(() => skillsQuery.data?.items ?? [], [skillsQuery.data])
  const totalCommands = useMemo(
    () => items.filter((s) => s.kind === 'command').length,
    [items],
  )
  // fetchSkillsUsage / fetchSkillsSummary swallow every failure (404 = no
  // ClickHouse sink) into `null`. With analytics off there is no traffic and
  // no Discovered rows: states fall back to Registered / Disabled.
  const usageLoading = usageQuery.isLoading || summaryQuery.isLoading
  const analyticsUnavailable = !usageLoading && usageQuery.data === null
  const allRows = useMemo(
    () => mergeSkillRows(items, usageQuery.data ?? null, summaryQuery.data?.skills ?? null),
    [items, usageQuery.data, summaryQuery.data],
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
  const mostUsed = useMemo(() => {
    let best: SkillLifecycleRow | null = null
    for (const r of allRows) if (r.calls > 0 && (!best || r.calls > best.calls)) best = r
    return best
  }, [allRows])

  function handleDelete() {
    if (!deleteTarget) return
    deleteSkill.mutate(deleteTarget.id, {
      onSuccess: () => {
        toast.success(deleteTarget.kind === 'command' ? 'Command deleted' : 'Skill deleted')
        setDeleteTarget(null)
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to delete')
      },
    })
  }

  const columns: ColumnDef<SkillLifecycleRow>[] = [
    {
      id: 'name',
      accessorKey: 'name',
      header: 'Name',
      size: 220,
      cell: ({ row }) => (
        <span className="font-mono text-[13px] font-semibold text-foreground">
          {row.original.name}
        </span>
      ),
    },
    {
      id: 'state',
      accessorFn: (r) => r.state,
      header: 'Status',
      size: 150,
      cell: ({ row }) => <LifecyclePill state={row.original.state} />,
    },
    {
      id: 'action',
      header: 'Action',
      size: 110,
      enableSorting: false,
      cell: ({ row }) =>
        row.original.state === 'discovered' ? (
          <Button
            size="xs"
            variant="outline"
            className="shrink-0 whitespace-nowrap"
            aria-label={`Register ${row.original.name}`}
            onClick={(e) => {
              e.stopPropagation()
              setRegisterName(row.original.name)
            }}
          >
            Register
          </Button>
        ) : null,
    },
    {
      id: 'kind',
      accessorFn: (r) => r.skill?.kind ?? 'skill',
      header: 'Kind',
      size: 110,
      cell: ({ row }) => (
        <Badge variant="outline" className="text-[10px] uppercase">
          {row.original.skill?.kind ?? 'skill'}
        </Badge>
      ),
    },
    {
      id: 'description',
      accessorFn: (r) => r.skill?.description ?? '',
      header: 'Description',
      size: 320,
      cell: ({ row }) => (
        <span className="text-text-muted">{row.original.skill?.description || '—'}</span>
      ),
    },
    {
      id: 'version',
      accessorFn: (r) => r.skill?.latest_version ?? 0,
      header: 'Version',
      size: 100,
      cell: ({ row }) => (
        <span className="text-text-muted tabular-nums">
          {row.original.skill ? `v${row.original.skill.latest_version}` : '—'}
        </span>
      ),
    },
    {
      id: 'calls',
      accessorFn: (r) => r.calls,
      header: `Calls (${range})`,
      size: 110,
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
        const usedBy = row.original.used_by
        return (
          <span className="text-text-muted">
            {usedBy > 0 ? `${usedBy} ${usedBy === 1 ? 'key' : 'keys'}` : '—'}
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
      id: 'scope',
      accessorFn: (r) => r.skill?.scope ?? '',
      header: 'Scope',
      size: 110,
      cell: ({ row }) =>
        row.original.skill === null ? (
          <span className="text-text-subtle">—</span>
        ) : row.original.skill.scope === 'platform' ? (
          <Badge variant="outline" className="text-[10px] uppercase">
            platform
          </Badge>
        ) : (
          <span className="text-text-subtle">Tenant</span>
        ),
    },
    {
      id: 'updated_at',
      accessorFn: (r) => r.skill?.updated_at ?? '',
      header: 'Updated',
      meta: { tooltip: false },
      cell: ({ row }) => (
        <span className="text-text-muted">
          {row.original.skill ? formatDate(row.original.skill.updated_at) : '—'}
        </span>
      ),
    },
  ]

  // Platform skills are managed outside this tenant; a Discovered name has no
  // row to delete, only to register.
  const rowActions: RowAction<SkillLifecycleRow>[] = [
    {
      label: 'Register',
      icon: <Plus aria-hidden="true" />,
      hidden: (r) => r.state !== 'discovered',
      onClick: (r) => setRegisterName(r.name),
    },
    {
      label: 'Delete',
      icon: <Trash2 aria-hidden="true" />,
      variant: 'destructive',
      hidden: (r) => r.skill === null || r.skill.scope === 'platform',
      onClick: (r) => r.skill && setDeleteTarget(r.skill),
    },
  ]

  const isLoading = skillsQuery.isLoading || usageLoading

  const header = (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div>
        <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
          Skills{!skillsQuery.isLoading ? ` (${items.length})` : ''}
        </h1>
        <p className="text-[13px] text-text-subtle">
          Text-only instructions and prompt templates agents can load through a profile.
        </p>
      </div>
      <div className="flex items-center gap-2">
        <RangeSelect value={range} onChange={setRange} />
        <Button onClick={() => setCreateOpen(true)}>
          <Plus className="size-4" aria-hidden="true" />
          Add skill
        </Button>
      </div>
    </div>
  )

  if (!isLoading && items.length === 0 && analyticsUnavailable) {
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
        <StatCard dotClassName="bg-primary" title="Total skills">
          <span className="text-[26px] leading-tight font-bold text-foreground">
            {items.length}
          </span>
        </StatCard>
        <StatCard dotClassName="bg-brand-cyan-to" title="Commands">
          <span className="text-[26px] leading-tight font-bold text-foreground">
            {totalCommands}
          </span>
        </StatCard>
        <StatCard dotClassName="bg-sev-medium" title={`Most used (${range})`}>
          {mostUsed ? (
            <>
              <span className="truncate font-mono text-[15px] font-semibold text-foreground">
                {mostUsed.name}
              </span>
              <span className="text-xs text-text-subtle">
                {formatCompactNumber(mostUsed.calls)} calls
              </span>
            </>
          ) : (
            <EmptyState label="No data yet" className="mx-0 text-left" />
          )}
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
        storageKey="skills"
        storageVersion={3}
        columns={columns}
        data={rows}
        getRowId={(r) => r.key}
        getRowLabel={(r) => r.name}
        rowActions={rowActions}
        onRowClick={(r) => (r.skill ? setDetailTarget(r.skill) : setRegisterName(r.name))}
        loading={isLoading}
        error={skillsQuery.isError && "Couldn't load skills."}
        emptyTitle="No skills yet"
        emptyDescription="Add a skill or command, then attach it to a profile to make it available to agents."
      />
      {analyticsUnavailable ? (
        <AnalyticsOffNote />
      ) : (
        <p className="px-1 text-xs text-text-subtle">
          Usage observed from gateway traffic over the selected range. Discovered names were
          used by a model but are not registered.
        </p>
      )}

      <SkillFormDialog open={createOpen} onOpenChange={setCreateOpen} />
      <SkillFormDialog
        open={registerName !== null}
        onOpenChange={(open) => !open && setRegisterName(null)}
        initialName={registerName ?? ''}
      />
      <SkillDetailSheet
        skill={detailTarget}
        onOpenChange={(open) => !open && setDetailTarget(null)}
      />
      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        title={`Delete "${deleteTarget?.name}"?`}
        description="Any profile this is attached to loses access to it immediately. This can't be undone."
        confirmLabel="Delete"
        destructive
        isLoading={deleteSkill.isPending}
        onConfirm={handleDelete}
      />
    </DataTablePage>
  )
}
