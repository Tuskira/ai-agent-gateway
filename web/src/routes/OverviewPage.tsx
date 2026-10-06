import { useState } from 'react'
import { Link } from 'react-router-dom'
import {
  ArrowRight,
  Bot,
  CircleCheck,
  CircleDollarSign,
  Coins,
  Wrench,
  type LucideIcon,
} from 'lucide-react'
import { useAuth } from '@/auth/AuthContext'
import {
  useTrafficFlow,
  useConnectors,
  useConnectorToolCounts,
  useHealth,
  useOverviewMetrics,
  useProfiles,
  useProfileToolCounts,
} from '@/lib/queries'
import {
  chartColor,
  formatCompactNumber,
  formatCurrency,
  formatDeltaPct,
  formatPercent,
  MAX_TOOL_COUNT_REQUESTS,
  pctOfMax,
  periodLabel,
  periodQuery,
  type ConnectorStatus,
  type CountKpi,
} from '@/lib/overview'
import { colors } from '@/styles/tokens'
import { cn } from '@/lib/utils'
import { DashboardCard } from '@/components/app/DashboardCard'
import { KpiCard } from '@/components/app/KpiCard'
import { LatencySummary } from '@/components/app/LatencySummary'
import { NumberTicker } from '@/components/app/NumberTicker'
import {
  BarListSkeleton,
  ChartSkeleton,
  DonutSkeleton,
} from '@/components/app/OverviewSkeletons'
import { EmptyState } from '@/components/app/EmptyState'
import { ColorSwatch } from '@/components/app/ColorSwatch'
import { Donut } from '@/components/app/Donut'
import { MiniBar } from '@/components/app/MiniBar'
import { RankedBarList, type RankedBarItem } from '@/components/app/RankedBarList'
import { ScrollList } from '@/components/app/ScrollList'
import { TrafficAreaChart } from '@/components/app/TrafficAreaChart'
import { WidgetHelp } from '@/components/app/WidgetHelp'
import { KPI_HELP, WIDGET_HELP } from '@/lib/overview-help'
import { Skeleton } from '@/components/ui/skeleton'
import { PeriodFilter } from '@/components/app/PeriodFilter'
import { usePeriod } from '@/hooks/use-period'
import { TrafficFlowCard } from '@/components/app/TrafficFlowCard'
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/tooltip'

const USAGE_DETAILS_PATH = '/llm-logs'
const CLICKHOUSE_EMPTY_LABEL = 'No data yet · enable the ClickHouse sink'

const COST_ESTIMATE_SUBTITLE = 'estimate · rate card'
const COST_ESTIMATE_TOOLTIP =
  'Costs are computed from a bundled list-price table and may differ from your invoice. Override with llm_proxy.pricing_file.'

/* Card heights. Fixed from `lg` up so every card in a row is the same size
 * and long lists scroll inside their card; below that a card is as tall as
 * its content. */
const heroCardClass = 'min-h-[300px] lg:h-[384px]'
const rowCardClass = 'min-h-[240px] lg:h-[300px]'

const detailsLinkClass =
  'inline-flex items-center gap-1 text-xs font-semibold text-text-link hover:underline'
const listRowHoverClass =
  '-mx-2 rounded-r-3 px-2 transition-colors duration-200 hover:bg-bg-subtle'
/* A donut sits beside its legend when the card is wide enough for both,
 * and above it otherwise. */
const heroDonutBodyClass =
  'flex-1 items-center justify-center gap-6 @min-[380px]:flex-row'
const rowDonutBodyClass =
  'flex-1 items-center justify-center gap-4 @min-[290px]:flex-row @min-[340px]:gap-6'

function connectorDotClassName(status: ConnectorStatus): string {
  if (status === 'healthy') return 'bg-status-resolved'
  if (status === 'unhealthy') return 'bg-sev-high'
  return 'bg-border-strong'
}

function statusCodeColor(label: string): string {
  if (label === '200') return colors.status.resolved
  if (label === '204') return 'var(--primary)'
  return colors.sev.high
}

interface KpiTile {
  key: keyof typeof KPI_HELP
  label: string
  icon: LucideIcon
  chipClassName: string
  /** `null` until the analytics endpoint has answered with real numbers. */
  value: number | null
  format: (n: number) => string
  deltaText?: string
  deltaDirection?: 'up' | 'down'
  sub?: string
}

export default function OverviewPage() {
  const [period, setPeriod] = usePeriod('24h')
  const [flowClientName, setFlowClientName] = useState('')

  const { principal } = useAuth()
  const { data: health, isLoading: healthLoading } = useHealth()
  const metricsQuery = useOverviewMetrics(period)
  const metrics = metricsQuery.data ?? null
  const metricsLoading = metricsQuery.isLoading
  const flowQuery = useTrafficFlow(period, flowClientName)

  const connectorsQuery = useConnectors()
  const profilesQuery = useProfiles()
  const connectors = connectorsQuery.data?.items ?? []
  const profiles = profilesQuery.data?.items ?? []
  const connectorToolCounts = useConnectorToolCounts(connectors)
  const profileToolCounts = useProfileToolCounts(profiles)

  const healthLabel = health ? (health.status === 'ok' ? 'healthy' : health.status) : null
  const tenantShort = principal ? principal.tenant_id.slice(0, 8) : '—'
  const vsPrevious =
    period.range === 'custom' ? 'vs previous period' : `vs previous ${periodLabel(period).replace('Last ', '')}`
  const showSetupStrip =
    connectorsQuery.data !== undefined && connectorsQuery.data.total === 0

  /* ---- KPI tiles ---- */
  const kpis = metrics?.kpis
  const countKpi = (kpi: CountKpi | undefined) => ({
    value: kpi ? kpi.value : null,
    deltaText: kpi ? formatDeltaPct(kpi.delta.pct) : undefined,
    deltaDirection: kpi?.delta.direction,
    sub: kpi ? vsPrevious : undefined,
  })
  const kpiTiles: KpiTile[] = [
    {
      key: 'llm',
      label: 'LLM Agent Calls',
      icon: Bot,
      chipClassName: 'bg-sev-medium/15 text-sev-medium',
      format: formatCompactNumber,
      ...countKpi(kpis?.llmAgentCalls),
    },
    {
      key: 'mcp',
      label: 'MCP Tool Calls',
      icon: Wrench,
      chipClassName: 'bg-brand-cyan-to/15 text-brand-cyan-to',
      format: formatCompactNumber,
      ...countKpi(kpis?.mcpToolCalls),
    },
    {
      key: 'tokens',
      label: 'Total Tokens',
      icon: Coins,
      chipClassName: 'bg-primary/15 text-primary',
      format: formatCompactNumber,
      ...countKpi(kpis?.totalTokens),
    },
    {
      key: 'cost',
      label: 'Total Cost',
      icon: CircleDollarSign,
      chipClassName: 'bg-brand-tusk/20 text-sev-medium-fg',
      format: formatCurrency,
      ...countKpi(kpis?.totalCost),
    },
    {
      key: 'success',
      label: 'Success rate',
      icon: CircleCheck,
      chipClassName: 'bg-status-resolved/15 text-status-resolved',
      format: formatPercent,
      value: kpis ? kpis.successRate.value : null,
      sub: kpis?.successRate.sub,
    },
  ]

  /* ---- LLM Usage ---- */
  const maxLlmCalls = metrics ? Math.max(...metrics.llmUsage.map((r) => r.calls), 1) : 1

  // costEstimated/pricingSource are new, optional fields on the overview
  // payload (an older gateway build may not send them yet) -- absence is
  // treated the same as `true`, since every cost figure this page has ever
  // shown has come from the rate card, never a provider invoice.
  const showCostEstimateNote = metrics ? metrics.costEstimated !== false : false

  /* ---- Traffic Distribution donut ---- */
  const trafficTotal = metrics ? metrics.traffic.llmCalls + metrics.traffic.mcpCalls : 0
  const trafficSlices = metrics
    ? [
        {
          key: 'llm',
          label: 'LLM Agent',
          value: metrics.traffic.llmCalls,
          color: colors.sev.medium,
        },
        {
          key: 'mcp',
          label: 'MCP Tool',
          value: metrics.traffic.mcpCalls,
          color: colors.brand.cyanTo,
        },
      ]
    : null

  /* ---- Top agents / profiles: metrics when present, else real profiles ---- */
  const topAgentsItems: RankedBarItem[] | null = metrics
    ? metrics.topAgents.map((a, i) => ({
        key: `${i}-${a.name}`,
        label: a.name,
        value: a.count,
        displayValue: formatCompactNumber(a.count),
      }))
    : profilesQuery.data
      ? profiles.slice(0, MAX_TOOL_COUNT_REQUESTS).map((p, i) => {
          const count = profileToolCounts[i]?.data ?? 0
          return { key: p.id, label: p.name, value: count, displayValue: String(count) }
        })
      : null

  /* ---- Health · status codes donut ---- */
  const healthSlices = metrics
    ? metrics.statusCodes.map((s) => ({
        key: s.label,
        label: s.label,
        value: s.pct,
        color: statusCodeColor(s.label),
      }))
    : null

  /* ---- Requests by client / MCP Tools: metrics only, no real substitute ---- */
  const requestsByClientItems: RankedBarItem[] | null = metrics
    ? metrics.requestsByClient.map((c, i) => ({
        key: `${i}-${c.name}`,
        label: c.name,
        value: c.count,
        displayValue: formatCompactNumber(c.count),
      }))
    : null

  const mcpToolsItems: RankedBarItem[] | null = metrics
    ? metrics.mcpTools.map((t, i) => ({
        key: `${i}-${t.name}`,
        label: t.name,
        value: t.count,
        displayValue: formatCompactNumber(t.count),
      }))
    : null

  /* ---- Top connectors: metrics when present, else real connectors ---- */
  const topConnectorsItems: RankedBarItem[] | null = metrics
    ? metrics.topConnectors.map((c, i) => ({
        key: `${i}-${c.name}`,
        label: c.name,
        value: c.count,
        displayValue: formatCompactNumber(c.count),
      }))
    : connectorsQuery.data
      ? connectors.slice(0, MAX_TOOL_COUNT_REQUESTS).map((c, i) => {
          const count = connectorToolCounts[i]?.data ?? 0
          return {
            key: c.id,
            label: c.name,
            value: count,
            displayValue: String(count),
            dotClassName: connectorDotClassName(c.status),
          }
        })
      : null

  const detailsLink = (
    <Link to={USAGE_DETAILS_PATH} className={detailsLinkClass}>
      View details
      <ArrowRight className="size-3.5" aria-hidden="true" />
    </Link>
  )

  return (
    <div className="flex flex-col gap-4">
      {/* Header */}
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
            AI Agent Observability
          </h1>
          <p className="text-[13px] text-text-subtle">
            Visibility Dashboard
            {!healthLoading && health ? (
              <>
                {' '}
                · v{health.version} ·{' '}
                <span className="font-medium text-status-resolved">{healthLabel}</span>
              </>
            ) : null}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <PeriodFilter value={period} onChange={setPeriod} />
          <span className="inline-flex h-9 items-center gap-1.5 rounded-r-4 border border-border bg-card px-3 text-[13px] font-semibold text-foreground">
            <span
              className="size-[7px] rounded-full bg-status-resolved"
              aria-hidden="true"
            />
            {tenantShort}
          </span>
        </div>
      </div>

      {showSetupStrip ? (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-r-5 border border-dashed border-border bg-bg-subtle px-4 py-3 text-sm">
          <span className="text-text-muted">No MCPs yet</span>
          <Link
            to="/connectors"
            className="inline-flex items-center gap-1 font-semibold text-text-link"
          >
            Add one
            <ArrowRight className="size-3.5" aria-hidden="true" />
          </Link>
        </div>
      ) : null}

      {/* KPI row */}
      <div className="grid grid-cols-[repeat(auto-fit,minmax(200px,1fr))] gap-4">
        {kpiTiles.map((k) => (
          <KpiCard
            key={k.key}
            title={k.label}
            icon={k.icon}
            chipClassName={k.chipClassName}
            help={
              <WidgetHelp
                title={k.label}
                content={KPI_HELP[k.key]}
                triggerClassName="size-5"
              />
            }
            titleNote={
              k.key === 'cost' && showCostEstimateNote ? (
                <TooltipProvider>
                  <Tooltip>
                    <TooltipTrigger
                      render={
                        <span className="cursor-default text-text-subtle underline decoration-border-strong decoration-dotted underline-offset-2" />
                      }
                    >
                      {COST_ESTIMATE_SUBTITLE}
                    </TooltipTrigger>
                    <TooltipContent className="max-w-[240px] text-center">
                      {COST_ESTIMATE_TOOLTIP}
                    </TooltipContent>
                  </Tooltip>
                </TooltipProvider>
              ) : null
            }
            value={
              metricsLoading ? (
                <Skeleton className="h-7 w-24" />
              ) : k.value === null ? (
                '—'
              ) : (
                <NumberTicker value={k.value} format={k.format} />
              )
            }
            footer={
              metricsLoading ? (
                <Skeleton className="h-3 w-28" />
              ) : (
                <>
                  {k.deltaText ? (
                    <span
                      className={cn(
                        'font-semibold',
                        k.deltaDirection === 'up'
                          ? 'text-status-resolved'
                          : 'text-sev-high',
                      )}
                    >
                      {k.deltaDirection === 'up' ? '▲' : '▼'} {k.deltaText}
                    </span>
                  ) : null}{' '}
                  {k.sub}
                </>
              )
            }
            footerAction={
              k.key === 'cost' ? (
                <Link
                  to={USAGE_DETAILS_PATH}
                  aria-label="View cost details"
                  className={detailsLinkClass}
                >
                  <span className="hidden @min-[230px]:inline">Details</span>
                  <ArrowRight className="size-3.5" aria-hidden="true" />
                </Link>
              ) : k.key === 'tokens' ? (
                <Link
                  to={`/token-monitoring?${periodQuery(period)}`}
                  aria-label="View token monitoring"
                  className={detailsLinkClass}
                >
                  <span className="hidden @min-[230px]:inline">Details</span>
                  <ArrowRight className="size-3.5" aria-hidden="true" />
                </Link>
              ) : null
            }
          />
        ))}
      </div>

      {/* Full width: agent -> path -> model | connector Sankey */}
      <TrafficFlowCard
        data={flowQuery.data}
        period={period}
        clientName={flowClientName}
        onClientNameChange={setFlowClientName}
        emptyLabel={CLICKHOUSE_EMPTY_LABEL}
        loading={flowQuery.isLoading}
        help={<WidgetHelp title="Agent traffic flow" content={WIDGET_HELP.trafficFlow} />}
      />

      {/* Hero row: LLM Usage / Traffic Distribution / Top agents / profiles */}
      <div className="grid grid-cols-[repeat(auto-fit,minmax(min(100%,340px),1fr))] gap-4">
        <DashboardCard
          title="LLM Usage"
          help={<WidgetHelp title="LLM Usage" content={WIDGET_HELP.llmUsage} />}
          action={detailsLink}
          className={heroCardClass}
        >
          {metricsLoading ? (
            <BarListSkeleton />
          ) : metrics ? (
            <>
              <div className="mb-2 flex justify-between text-[10.5px] font-semibold tracking-[.08em] text-text-subtle uppercase">
                <span>Model</span>
                <span>Cost (est.)</span>
              </div>
              <ScrollList className="-mx-2 min-h-0 flex-1">
                <div className="bar-list flex flex-col gap-1 px-2">
                  {metrics.llmUsage.map((row, i) => (
                    <div
                      key={row.model}
                      className={cn('flex flex-col gap-1.5 py-1', listRowHoverClass)}
                    >
                      <div className="flex justify-between gap-3 text-[13px] leading-5">
                        <span className="min-w-0 truncate font-mono text-foreground">
                          {row.model}
                        </span>
                        <span className="shrink-0 font-semibold tabular-nums text-foreground">
                          {formatCompactNumber(row.calls)}
                        </span>
                      </div>
                      <div className="h-2">
                        <MiniBar
                          pct={pctOfMax(row.calls, maxLlmCalls)}
                          color={chartColor(i)}
                          heightPx={8}
                        />
                      </div>
                      <div className="flex justify-between gap-3 text-[11.5px] leading-4 text-text-subtle">
                        <span>{formatCompactNumber(row.tokens)} tokens</span>
                        <span className="tabular-nums">{formatCurrency(row.cost)}</span>
                      </div>
                    </div>
                  ))}
                </div>
              </ScrollList>
            </>
          ) : (
            <div className="flex flex-1 items-center justify-center">
              <EmptyState label={CLICKHOUSE_EMPTY_LABEL} />
            </div>
          )}
        </DashboardCard>

        <DashboardCard
          title="Traffic Distribution"
          help={
            <WidgetHelp
              title="Traffic Distribution"
              content={WIDGET_HELP.trafficDistribution}
            />
          }
          className={heroCardClass}
          bodyClassName={heroDonutBodyClass}
        >
          {metricsLoading ? (
            <DonutSkeleton sizeClassName="size-[190px] @min-[440px]:size-[220px]" />
          ) : (
            <>
              <Donut
                sizeClassName="size-[190px] @min-[440px]:size-[220px]"
                slices={trafficSlices}
                centerValue={
                  <NumberTicker value={trafficTotal} format={formatCompactNumber} />
                }
                centerLabel="Total"
                emptyLabel={CLICKHOUSE_EMPTY_LABEL}
              />
              {metrics && trafficSlices ? (
                <div className="flex w-full max-w-[240px] min-w-[170px] flex-col gap-1 text-sm">
                  {trafficSlices.map((s) => (
                    <div
                      key={s.key}
                      className={cn(
                        'grid grid-cols-[auto_minmax(0,1fr)_auto_3.25rem] items-center gap-2 py-1.5',
                        listRowHoverClass,
                      )}
                    >
                      <ColorSwatch color={s.color} />
                      <span className="truncate text-foreground">{s.label}</span>
                      <span className="font-semibold tabular-nums text-foreground">
                        {formatCompactNumber(s.value)}
                      </span>
                      <span className="text-right text-xs tabular-nums text-text-subtle">
                        {formatPercent((s.value / Math.max(trafficTotal, 1)) * 100)}
                      </span>
                    </div>
                  ))}
                </div>
              ) : null}
            </>
          )}
        </DashboardCard>

        <DashboardCard
          title="Top agents / profiles"
          help={
            <WidgetHelp title="Top agents / profiles" content={WIDGET_HELP.topAgents} />
          }
          className={heroCardClass}
          action={
            <span className="text-[11.5px] text-text-subtle">
              {metrics ? (
                <>
                  from <code className="font-mono">X-Agent-Profile-Name</code>
                </>
              ) : (
                'profiles · cached tool count'
              )}
            </span>
          }
        >
          {metricsLoading ? (
            <BarListSkeleton rows={5} />
          ) : (
            <RankedBarList
              items={topAgentsItems}
              variant="stacked"
              emptyLabel={metrics ? CLICKHOUSE_EMPTY_LABEL : 'No profiles yet'}
            />
          )}
        </DashboardCard>
      </div>

      {/* Row B: Health · status codes / Latency / Requests by client */}
      <div className="grid grid-cols-[repeat(auto-fit,minmax(min(100%,340px),1fr))] gap-4">
        <DashboardCard
          title="Health · status codes"
          help={<WidgetHelp title="Health · status codes" content={WIDGET_HELP.health} />}
          className={rowCardClass}
          bodyClassName={rowDonutBodyClass}
        >
          {metricsLoading ? (
            <DonutSkeleton sizeClassName="size-[140px] @min-[340px]:size-[184px]" />
          ) : (
            <>
              <Donut
                sizeClassName="size-[140px] @min-[340px]:size-[184px]"
                slices={healthSlices}
                centerValue={
                  <NumberTicker value={metrics?.successPct ?? 0} format={formatPercent} />
                }
                centerLabel="success"
                centerValueClassName="text-xl font-bold @min-[340px]:text-[26px]"
                centerLabelClassName="text-[11px] text-text-subtle"
                emptyLabel={CLICKHOUSE_EMPTY_LABEL}
              />
              {metrics && healthSlices ? (
                <div className="flex w-full max-w-[200px] min-w-[130px] flex-col gap-1 text-[13px]">
                  {healthSlices.map((s) => (
                    <div
                      key={s.key}
                      className={cn(
                        'grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-2 py-1.5',
                        listRowHoverClass,
                      )}
                    >
                      <ColorSwatch color={s.color} />
                      <span className="truncate font-mono text-[12.5px] text-foreground">
                        {s.label}
                      </span>
                      <span className="text-right font-semibold tabular-nums text-foreground">
                        {formatPercent(s.value)}
                      </span>
                    </div>
                  ))}
                </div>
              ) : null}
            </>
          )}
        </DashboardCard>

        <DashboardCard
          title="Latency"
          help={<WidgetHelp title="Latency" content={WIDGET_HELP.latency} />}
          className={rowCardClass}
          bodyClassName="gap-3"
        >
          <LatencySummary
            latency={metrics?.latency ?? null}
            loading={metricsLoading}
            emptyLabel={CLICKHOUSE_EMPTY_LABEL}
          />
        </DashboardCard>

        <DashboardCard
          title="Requests by client"
          help={
            <WidgetHelp
              title="Requests by client"
              content={WIDGET_HELP.requestsByClient}
            />
          }
          className={rowCardClass}
        >
          {metricsLoading ? (
            <BarListSkeleton />
          ) : (
            <RankedBarList
              items={requestsByClientItems}
              emptyLabel={CLICKHOUSE_EMPTY_LABEL}
            />
          )}
        </DashboardCard>
      </div>

      {/* Row C: MCP Tools / Top connectors */}
      <div className="grid grid-cols-[repeat(auto-fit,minmax(min(100%,340px),1fr))] gap-4">
        <DashboardCard
          title="MCP Tools"
          help={<WidgetHelp title="MCP Tools" content={WIDGET_HELP.mcpTools} />}
          className={rowCardClass}
        >
          {metricsLoading ? (
            <BarListSkeleton />
          ) : (
            <RankedBarList
              items={mcpToolsItems}
              labelClassName="font-mono text-[12px] text-text-muted"
              emptyLabel={CLICKHOUSE_EMPTY_LABEL}
            />
          )}
        </DashboardCard>

        <DashboardCard
          title="Top MCPs"
          help={<WidgetHelp title="Top MCPs" content={WIDGET_HELP.topConnectors} />}
          className={rowCardClass}
        >
          {metricsLoading ? (
            <BarListSkeleton />
          ) : (
            <RankedBarList
              items={topConnectorsItems}
              emptyLabel={metrics ? CLICKHOUSE_EMPTY_LABEL : 'No MCPs yet'}
            />
          )}
        </DashboardCard>
      </div>

      {/* Full width: Traffic Over Time */}
      <DashboardCard
        title="Traffic Over Time"
        help={
          <WidgetHelp title="Traffic Over Time" content={WIDGET_HELP.trafficOverTime} />
        }
        action={
          <div className="flex gap-4 text-xs text-text-muted">
            <span className="flex items-center gap-1.5">
              <ColorSwatch color={colors.sev.medium} />
              LLM calls
            </span>
            <span className="flex items-center gap-1.5">
              <ColorSwatch color={colors.brand.cyanTo} />
              MCP calls
            </span>
          </div>
        }
      >
        {metricsLoading ? (
          <ChartSkeleton />
        ) : (
          <TrafficAreaChart
            points={metrics?.trafficOverTime ?? null}
            emptyLabel={CLICKHOUSE_EMPTY_LABEL}
          />
        )}
      </DashboardCard>
    </div>
  )
}
