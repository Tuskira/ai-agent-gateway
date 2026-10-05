import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
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
import { RankedBarList } from '@/components/app/RankedBarList'
import {
  DeltaText,
  RolePill,
  TokenSplit,
  WindowSwitch,
} from '@/components/app/token-monitoring/parts'
import {
  AnalyticsOffNotice,
  TotalsTiles,
} from '@/components/app/token-monitoring/TotalsTiles'
import { TokenAreaChart } from '@/components/app/token-monitoring/TokenAreaChart'
import { chartColor, formatCompactNumber } from '@/lib/overview'
import { useMonitoringWindow } from '@/hooks/use-monitoring-window'
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
  type ModelTokens,
  type TokenMonitoring,
} from '@/lib/token-monitoring'
import { colors } from '@/styles/tokens'

/** A fixed height at every width that fits exactly five list rows
 * (RankedBarList stacked rows: 46px + 10px gap) under the card header; the
 * rest scrolls inside the card (RankedBarList's ScrollList). */
const CARD_CLASS = 'h-[348px]'

/** Cost cards on the page; "Show all" opens every model in a dialog. */
const TOP_MODELS = 6

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
      className="dash-card flex flex-col gap-3 rounded-r-5 border border-border bg-card p-4 text-left transition-colors hover:border-border-strong"
    >
      <div className="flex items-start justify-between gap-2">
        <span className="flex min-w-0 items-center gap-2">
          <ColorSwatch color={chartColor(index)} />
          <span className="truncate font-mono text-[13px] font-semibold text-foreground">
            {m.model}
          </span>
        </span>
        <span className="shrink-0 text-xs text-text-subtle">
          {m.calls.toLocaleString()} calls
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
  const [window, setWindow] = useMonitoringWindow()
  const [allModels, setAllModels] = useState(false)
  const query = useTokenMonitoring(window)
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
      `/token-monitoring/model?${new URLSearchParams({ model, window }).toString()}`,
    )
  const costCards = (items: typeof byCost) => (
    <div className="grid grid-cols-[repeat(auto-fill,minmax(min(100%,300px),1fr))] gap-4">
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
  const openKey = (c: CallerTokens) => {
    if (c.key_id)
      navigate(`/token-monitoring/keys/${encodeURIComponent(c.key_id)}?window=${window}`)
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
        <WindowSwitch value={window} onChange={setWindow} />
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
                  <div className="flex w-full max-w-[280px] flex-col gap-0.5 border-t border-border pt-2 text-sm">
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
                ) : null}
              </DashboardCard>
            </section>
            <DashboardCard title="By model" className={CARD_CLASS}>
              <RankedBarList
                variant="stacked"
                emptyLabel={NO_USAGE_LABEL}
                items={data ? modelBars(data.by_model) : null}
              />
            </DashboardCard>
            <DashboardCard title="By caller" className={CARD_CLASS}>
              <RankedBarList
                variant="stacked"
                emptyLabel={NO_USAGE_LABEL}
                items={data ? callerBars(data.by_key) : null}
              />
            </DashboardCard>
          </div>

          <DashboardCard title="Usage over time (UTC)">
            <TokenAreaChart
              points={data?.burn ?? null}
              granularity={data?.granularity ?? 'hour'}
              emptyLabel={NO_USAGE_LABEL}
            />
          </DashboardCard>

          <section aria-label="Cost by model" className="flex flex-col gap-3">
            <h2 className="text-[15px] font-semibold text-foreground">
              Cost by model{' '}
              <span className="ml-1 rounded-r-pill bg-bg-muted px-2 py-0.5 text-xs font-medium text-text-muted">
                {data?.by_model.length ?? 0} models
              </span>
            </h2>
            {data && data.by_model.length === 0 ? (
              <p className="rounded-r-5 border border-dashed border-border bg-bg-subtle px-4 py-6 text-center text-sm text-text-subtle">
                {NO_USAGE_LABEL}
              </p>
            ) : (
              <>
                {costCards(byCost.slice(0, TOP_MODELS))}
                {data && data.by_model.length > TOP_MODELS && (
                  <button
                    type="button"
                    onClick={() => setAllModels(true)}
                    className="self-center text-[13px] font-semibold text-text-link"
                  >
                    Show all {data.by_model.length} models
                  </button>
                )}
                <Dialog open={allModels} onOpenChange={setAllModels}>
                  <DialogContent className="flex max-h-[calc(100vh-3rem)] w-full flex-col overflow-hidden sm:max-w-4xl">
                    <DialogHeader>
                      <DialogTitle>Cost by model</DialogTitle>
                      <DialogDescription>
                        {data?.by_model.length ?? 0} models, most expensive first.
                      </DialogDescription>
                    </DialogHeader>
                    <div className="-mx-1 min-h-0 flex-1 overflow-y-auto px-1 pb-1">
                      {costCards(byCost)}
                    </div>
                  </DialogContent>
                </Dialog>
              </>
            )}
          </section>

          <section aria-label="Callers" className="flex flex-col gap-3">
            <h2 className="text-[15px] font-semibold text-foreground">Callers</h2>
            <DataTable
              storageKey="token-monitoring-callers"
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
          </section>
        </>
      )}
    </div>
  )
}
