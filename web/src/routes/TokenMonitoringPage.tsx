import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Search } from 'lucide-react'
import type { ColumnDef } from '@tanstack/react-table'
import { ColorSwatch } from '@/components/app/ColorSwatch'
import { DashboardCard } from '@/components/app/DashboardCard'
import { DataTable } from '@/components/app/data-table'
import { Donut } from '@/components/app/Donut'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { RankedBarList } from '@/components/app/RankedBarList'
import { ScrollList } from '@/components/app/ScrollList'
import {
  CostViewSwitch,
  DeltaText,
  RolePill,
  Section,
  TokenSplit,
} from '@/components/app/token-monitoring/parts'
import {
  AnalyticsOffNotice,
  TotalsTiles,
} from '@/components/app/token-monitoring/TotalsTiles'
import { TokenAreaChart } from '@/components/app/token-monitoring/TokenAreaChart'
import {
  chartColor,
  formatCompactNumber,
  periodParams,
  periodQuery,
} from '@/lib/overview'
import { useGridColumns } from '@/hooks/use-grid-columns'
import { usePeriod } from '@/hooks/use-period'
import { WidgetHelp } from '@/components/app/WidgetHelp'
import { MONITORING_HELP } from '@/lib/token-monitoring-help'
import { PeriodFilter } from '@/components/app/PeriodFilter'
import { useTokenMonitoring } from '@/lib/queries'
import {
  callerBars,
  callerLabel,
  formatShare,
  formatTokenCost,
  modelBars,
  NO_USAGE_LABEL,
  ROLE_LABEL,
  type CallerTokens,
  type CostView,
  type ModelTokens,
  type TokenMonitoring,
} from '@/lib/token-monitoring'
import { colors } from '@/styles/tokens'

/** A fixed height at every width that fits exactly five list rows
 * (RankedBarList stacked rows: 46px + 10px gap) under the card header; the
 * rest scrolls inside the card (RankedBarList's ScrollList). */
const CARD_CLASS = 'h-[348px]'

/** Cost cards fill this many complete rows of the grid (3 columns -> 6 cards,
 * 4 -> 8, 5 -> 10); "Show all" opens every model in a dialog. */
const COST_ROWS = 2

/** A Cost by model row: the model and its colour in the token-ranked lists. */
interface CostRow {
  m: ModelTokens
  colorIndex: number
}

const ROLE_COLOR = {
  agent: colors.brand.cyanTo,
  admin: colors.sev.medium,
  interceptor: colors.status.resolved,
  other: colors.status.suppressed,
} as const

function ModelCostCard({
  m,
  totalCost,
  index,
  onOpen,
}: {
  m: ModelTokens
  totalCost: number
  index: number
  onOpen: () => void
}) {
  return (
    <button
      type="button"
      aria-label={`Open ${m.model}`}
      onClick={onOpen}
      className="dash-card flex cursor-pointer flex-col gap-3 rounded-r-5 border border-border bg-card p-4 text-left transition-colors hover:border-border-strong"
    >
      <div className="flex items-start justify-between gap-2">
        <span className="flex min-w-0 items-center gap-2">
          <ColorSwatch color={chartColor(index)} />
          <span className="truncate font-mono text-[13px] font-semibold text-foreground">
            {m.model}
          </span>
        </span>
        <span className="shrink-0 text-xs text-text-subtle">
          {formatCompactNumber(m.calls)} calls
        </span>
      </div>
      <div className="flex items-baseline justify-between gap-2">
        <span className="text-xl font-bold tabular-nums text-foreground">
          {formatTokenCost(m.cost_usd)}
        </span>
        <span className="text-xs tabular-nums text-text-subtle">
          {m.cost_usd !== null
            ? formatShare(m.cost_usd, totalCost)
            : `${m.unpriced_calls} unpriced`}
        </span>
      </div>
      <TokenSplit usage={m} />
      <div className="flex items-center justify-between text-xs text-text-subtle">
        <span>
          <span className="font-semibold text-foreground">
            {formatCompactNumber(m.tokens)}
          </span>{' '}
          tokens
        </span>
        <DeltaText delta={m.delta_pct} tokens={m.tokens} />
      </div>
    </button>
  )
}

export default function TokenMonitoringPage() {
  const [period, setPeriod] = usePeriod('24h')
  const [allModels, setAllModels] = useState(false)
  // One choice for the page and the dialog, so the dialog opens the same way.
  const [costView, setCostView] = useState<CostView>('cards')
  const [modelQuery, setModelQuery] = useState('')
  const [costGridRef, costColumns] = useGridColumns(3)
  const topModels = costColumns * COST_ROWS
  const query = useTokenMonitoring(period)
  const navigate = useNavigate()
  const data: TokenMonitoring | null | undefined = query.data
  const loading = query.isLoading
  const totals = data?.totals
  const totalCost = data?.by_model.reduce((s, m) => s + (m.cost_usd ?? 0), 0) ?? 0
  // by_model arrives sorted by tokens; the cost cards rank by cost (unpriced
  // last) and keep each model's colour from the token-ranked lists.
  const byCost = (data?.by_model ?? [])
    .map((m, colorIndex) => ({ m, colorIndex }))
    .sort((a, b) => (b.m.cost_usd ?? -1) - (a.m.cost_usd ?? -1))

  const roleSlices = useMemo(
    () =>
      data
        ? (data.by_role ?? [])
            .filter((r) => r.tokens > 0)
            .map((r) => ({
              key: r.role,
              label: ROLE_LABEL[r.role],
              value: r.tokens,
              color: ROLE_COLOR[r.role],
            }))
        : null,
    [data],
  )

  const columns = useMemo<ColumnDef<CallerTokens>[]>(
    () => [
      {
        id: 'caller',
        header: 'Caller',
        accessorFn: callerLabel,
        cell: ({ row }) => (
          <span className="flex flex-col">
            <span className="font-medium text-foreground">
              {callerLabel(row.original)}
            </span>
            {row.original.key_id ? (
              <span className="font-mono text-[11px] text-text-subtle">
                {row.original.key_id}
              </span>
            ) : null}
          </span>
        ),
      },
      {
        id: 'role',
        header: 'Role',
        accessorKey: 'role',
        cell: ({ row }) => <RolePill role={row.original.role} />,
      },
      {
        id: 'tokens',
        header: 'Tokens',
        accessorKey: 'tokens',
        cell: ({ row }) => formatCompactNumber(row.original.tokens),
      },
      {
        id: 'share',
        header: 'Share',
        accessorFn: (c) => c.tokens,
        cell: ({ row }) => formatShare(row.original.tokens, totals?.tokens ?? 0),
      },
      {
        id: 'cost',
        header: 'Cost',
        accessorFn: (c) => c.cost_usd ?? -1,
        cell: ({ row }) => formatTokenCost(row.original.cost_usd),
      },
      { id: 'calls', header: 'Calls', accessorKey: 'calls' },
      { id: 'models', header: 'Models', accessorKey: 'models' },
      {
        id: 'delta',
        header: 'Change',
        accessorFn: (c) => c.delta_pct ?? 0,
        cell: ({ row }) => (
          <DeltaText delta={row.original.delta_pct} tokens={row.original.tokens} />
        ),
      },
    ],
    [totals?.tokens],
  )

  const openModel = (model: string) =>
    navigate(
      `/token-monitoring/model?${new URLSearchParams({ model, ...periodParams(period) }).toString()}`,
    )
  const costCards = (items: typeof byCost, ref?: (el: HTMLElement | null) => void) => (
    <div
      ref={ref}
      className="grid grid-cols-[repeat(auto-fill,minmax(min(100%,300px),1fr))] gap-4"
    >
      {items.map(({ m, colorIndex }) => (
        <ModelCostCard
          key={m.model}
          m={m}
          index={colorIndex}
          totalCost={totalCost}
          onOpen={() => openModel(m.model)}
        />
      ))}
    </div>
  )
  const modelColumns = useMemo<ColumnDef<CostRow>[]>(
    () => [
      {
        id: 'model',
        header: 'Model',
        accessorFn: (r) => r.m.model,
        cell: ({ row }) => (
          <span className="flex min-w-0 items-center gap-2">
            <ColorSwatch color={chartColor(row.original.colorIndex)} />
            <span className="truncate font-mono text-[13px] font-medium text-foreground">
              {row.original.m.model}
            </span>
          </span>
        ),
      },
      {
        id: 'cost',
        header: 'Cost',
        accessorFn: (r) => r.m.cost_usd ?? -1,
        cell: ({ row }) => formatTokenCost(row.original.m.cost_usd),
      },
      {
        id: 'share',
        header: 'Share',
        accessorFn: (r) => r.m.cost_usd ?? -1,
        cell: ({ row }) =>
          row.original.m.cost_usd !== null
            ? formatShare(row.original.m.cost_usd, totalCost)
            : `${row.original.m.unpriced_calls} unpriced`,
      },
      { id: 'calls', header: 'Calls', accessorFn: (r) => r.m.calls },
      ...(
        [
          ['tokens', 'Tokens'],
          ['prompt_tokens', 'Input'],
          ['completion_tokens', 'Output'],
          ['cache_read_tokens', 'Cache read'],
          ['cache_write_tokens', 'Cache write'],
        ] as const
      ).map(([key, header]): ColumnDef<CostRow> => ({
        id: key,
        header,
        accessorFn: (r) => r.m[key],
        cell: ({ row }) => formatCompactNumber(row.original.m[key]),
      })),
      {
        id: 'delta',
        header: 'Change',
        accessorFn: (r) => r.m.delta_pct ?? 0,
        cell: ({ row }) => (
          <DeltaText delta={row.original.m.delta_pct} tokens={row.original.m.tokens} />
        ),
      },
    ],
    [totalCost],
  )
  // fill: grow into a bounded parent (the dialog) instead of a fixed height.
  const modelTable = (rows: CostRow[], fill = false) => (
    <DataTable
      storageKey="token-monitoring-models"
      columns={modelColumns}
      data={rows}
      getRowId={(r) => r.m.model}
      getRowLabel={(r) => r.m.model}
      onRowClick={(r) => openModel(r.m.model)}
      defaultSorting={[{ field: 'cost', sort: 'desc' }]}
      height={fill ? undefined : '420px'}
      fillHeight={fill}
      loading={loading}
      emptyTitle={NO_USAGE_LABEL}
    />
  )
  const search = modelQuery.trim().toLowerCase()
  const matching = search
    ? byCost.filter(({ m }) => m.model.toLowerCase().includes(search))
    : byCost

  const openKey = (c: CallerTokens) => {
    if (c.key_id)
      navigate(
        `/token-monitoring/keys/${encodeURIComponent(c.key_id)}?${periodQuery(period)}`,
      )
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
            Token Monitoring
          </h1>
          <p className="text-[13px] text-text-subtle">
            Token usage and cost by model and caller. Times in UTC.
          </p>
        </div>
        <PeriodFilter value={period} onChange={setPeriod} />
      </div>

      {data === null ? (
        <AnalyticsOffNotice />
      ) : (
        <>
          <TotalsTiles totals={totals} loading={loading} />

          <div className="grid grid-cols-[repeat(auto-fit,minmax(min(100%,340px),1fr))] gap-4">
            <section aria-label="By caller role">
              <DashboardCard
                title="By caller role"
                help={
                  <WidgetHelp title="By caller role" content={MONITORING_HELP.byRole} />
                }
                className={CARD_CLASS}
                bodyClassName="flex flex-col items-center justify-center gap-3"
              >
                <Donut
                  sizeClassName="size-[140px]"
                  centerValueClassName="text-[19px] font-bold tabular-nums"
                  slices={roleSlices && roleSlices.length > 0 ? roleSlices : null}
                  centerValue={formatCompactNumber(totals?.tokens ?? 0)}
                  centerLabel="Tokens"
                  emptyLabel={NO_USAGE_LABEL}
                />
                {data ? (
                  // Fits as-is for the usual roles; with more rows than the
                  // card holds, the list scrolls instead of overflowing it.
                  <ScrollList className="min-h-0 w-full max-w-[280px] border-t border-border pt-2">
                    <div className="flex flex-col gap-0.5 text-sm">
                      {(data.by_role ?? []).map((r) => (
                        <div
                          key={r.role}
                          className="grid grid-cols-[auto_minmax(0,1fr)_auto_3.25rem] items-center gap-2 py-0.5"
                        >
                          <ColorSwatch color={ROLE_COLOR[r.role]} />
                          <span className="text-foreground">{ROLE_LABEL[r.role]}</span>
                          <span className="font-semibold tabular-nums">
                            {formatCompactNumber(r.tokens)}
                          </span>
                          <span className="text-right text-xs tabular-nums text-text-subtle">
                            {formatShare(r.tokens, totals?.tokens ?? 0)}
                          </span>
                        </div>
                      ))}
                    </div>
                  </ScrollList>
                ) : null}
              </DashboardCard>
            </section>
            <DashboardCard
              title="By model"
              help={<WidgetHelp title="By model" content={MONITORING_HELP.byModel} />}
              className={CARD_CLASS}
            >
              <RankedBarList
                variant="stacked"
                emptyLabel={NO_USAGE_LABEL}
                items={data ? modelBars(data.by_model) : null}
              />
            </DashboardCard>
            <DashboardCard
              title="By caller"
              help={<WidgetHelp title="By caller" content={MONITORING_HELP.byCaller} />}
              className={CARD_CLASS}
            >
              <RankedBarList
                variant="stacked"
                emptyLabel={NO_USAGE_LABEL}
                items={data ? callerBars(data.by_key) : null}
              />
            </DashboardCard>
          </div>

          <DashboardCard
            title="Usage over time (UTC)"
            help={
              <WidgetHelp
                title="Usage over time (UTC)"
                content={MONITORING_HELP.usageOverTime}
              />
            }
          >
            <TokenAreaChart
              points={data?.burn ?? null}
              granularity={data?.granularity ?? 'hour'}
              emptyLabel={NO_USAGE_LABEL}
            />
          </DashboardCard>

          <Section
            title="Cost by model"
            badge={
              <span className="rounded-r-pill bg-bg-muted px-2 py-0.5 text-xs font-medium text-text-muted">
                {data?.by_model.length ?? 0} models
              </span>
            }
            help={
              <WidgetHelp title="Cost by model" content={MONITORING_HELP.costByModel} />
            }
            action={<CostViewSwitch value={costView} onChange={setCostView} />}
          >
            {data && data.by_model.length === 0 ? (
              <p className="rounded-r-5 border border-dashed border-border bg-bg-subtle px-4 py-6 text-center text-sm text-text-subtle">
                {NO_USAGE_LABEL}
              </p>
            ) : costView === 'list' ? (
              modelTable(byCost)
            ) : (
              <>
                {costCards(byCost.slice(0, topModels), costGridRef)}
                {data && data.by_model.length > topModels && (
                  <button
                    type="button"
                    onClick={() => setAllModels(true)}
                    className="self-center text-[13px] font-semibold text-text-link"
                  >
                    Show all {data.by_model.length} models
                  </button>
                )}
              </>
            )}
            <Dialog
              open={allModels}
              onOpenChange={(open) => {
                setAllModels(open)
                if (!open) setModelQuery('')
              }}
            >
              <DialogContent className="flex h-[calc(100vh-3rem)] w-full flex-col overflow-hidden sm:max-w-6xl">
                <DialogHeader>
                  <DialogTitle>Cost by model</DialogTitle>
                  <DialogDescription>
                    {search
                      ? `${matching.length} of ${byCost.length} models`
                      : `${byCost.length} models, most expensive first.`}
                  </DialogDescription>
                </DialogHeader>
                <div className="flex flex-wrap items-center gap-3">
                  <label className="flex h-9 min-w-[220px] flex-1 items-center gap-2 rounded-r-4 border border-border bg-background px-3 text-text-subtle">
                    <Search className="size-4 shrink-0" aria-hidden="true" />
                    <Input
                      type="search"
                      aria-label="Search models"
                      value={modelQuery}
                      onChange={(e) => setModelQuery(e.target.value)}
                      placeholder="Search models…"
                      className="h-auto flex-1 border-0 bg-transparent p-0 text-sm shadow-none focus-visible:ring-0"
                    />
                  </label>
                  <CostViewSwitch value={costView} onChange={setCostView} />
                </div>
                {/* A fixed height, so switching cards and list (or searching)
                    never resizes the dialog. */}
                <div className="-mx-1 flex min-h-0 flex-1 flex-col overflow-y-auto px-1 pb-1">
                  {matching.length === 0 ? (
                    <p className="px-4 py-6 text-center text-sm text-text-subtle">
                      No models match “{modelQuery.trim()}”.
                    </p>
                  ) : costView === 'list' ? (
                    modelTable(matching, true)
                  ) : (
                    costCards(matching)
                  )}
                </div>
              </DialogContent>
            </Dialog>
          </Section>

          <Section
            title="Callers"
            help={<WidgetHelp title="Callers" content={MONITORING_HELP.callers} />}
          >
            <DataTable
              storageKey="token-monitoring-callers"
              height="420px"
              columns={columns}
              data={data?.by_key ?? []}
              getRowId={(c) => c.key_id || 'none'}
              getRowLabel={callerLabel}
              onRowClick={openKey}
              loading={loading}
              error={query.isError && "Couldn't load token monitoring."}
              emptyTitle={NO_USAGE_LABEL}
              emptyDescription="Calls through the LLM plane, and ingested calls, appear here per API key."
            />
          </Section>
        </>
      )}
    </div>
  )
}
