import { useCallback, useMemo, type ReactNode } from 'react'
import { useNavigate } from 'react-router-dom'
import { EmptyState } from '@/components/app/EmptyState'
import { ChartSkeleton } from '@/components/app/OverviewSkeletons'
import { TextAnimate } from '@/components/app/TextAnimate'
import {
  BalancedSankey,
  type PositionedLink,
  type PositionedNode,
  type SankeyLink as BalancedLink,
  type SankeyNode as BalancedNode,
} from '@/components/app/sankey'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { formatCompactNumber, periodLabel, type Period } from '@/lib/overview'
import { toBalancedData } from '@/lib/traffic-flow'
import {
  providerLabel,
  SANKEY_GATEWAY_TOOLS_KEY,
  SANKEY_OTHER_CONNECTORS_KEY,
  SANKEY_OTHER_MODELS_KEY,
  type SankeyNode,
  type TrafficFlow,
  type TrafficPath,
} from '@/lib/sankey'
import { colors } from '@/styles/tokens'
import { cn } from '@/lib/utils'

interface TrafficFlowCardProps {
  data: TrafficFlow | null | undefined
  period: Period
  /** The selected agent (`client_name`), `''` for all. */
  clientName: string
  onClientNameChange: (clientName: string) => void
  /** Shown when the request has settled and `data` came back `null` (analytics
   * off / endpoint 404s). */
  emptyLabel?: string
  /** Renders a `ChartSkeleton` instead of the chart/empty state while the
   * request is still in flight, matching every other Overview widget. */
  loading?: boolean
  /** Contextual help rendered next to the title -- normally a `<WidgetHelp />`. */
  help?: ReactNode
}

/** Ribbon colour by plane -- the same two hues the Traffic Distribution donut
 * already gives "LLM Agent" and "MCP Tool", so both cards read as one
 * system. Bars stay neutral; the ribbons carry the flow's identity, and
 * every bar is directly labelled. */
const PATH_COLOR: Record<TrafficPath, string> = {
  llm: colors.sev.medium,
  mcp: colors.brand.cyanTo,
}

const PATH_LEGEND: { key: TrafficPath; label: string }[] = [
  { key: 'llm', label: 'LLM path' },
  { key: 'mcp', label: 'MCP path' },
]

/** Neutral bar fill that follows the theme (light: mid grey, dark: light grey). */
const NODE_FILL = 'var(--text-muted)'

/** Geometry handed to the balanced renderer: thin bars, room on the left
 * for right-aligned agent labels and on the right for model + provider. */
const LAYOUT = {
  nodeWidth: 10,
  nodeGap: 14,
  minNodeHeight: 26,
  zeroNodeHeight: 26,
  leftMargin: 170,
  rightMargin: 240,
  labelGap: 10,
  labelPadding: 16,
}

/** The plane a link belongs to: a CLIENT -> PATH link is coloured by its
 * target path, a PATH -> MODEL/CONNECTOR link by its source path. */
function linkPath(
  source: SankeyNode | undefined,
  target: SankeyNode | undefined,
): TrafficPath | null {
  const key =
    source?.type === 'PATH' ? source.key : target?.type === 'PATH' ? target.key : null
  return key === 'llm' || key === 'mcp' ? key : null
}

/** Where a click on this node leads, or `null` when it is a synthetic
 * fold-in bucket (or the PATH column) with nothing to open. */
function nodeTarget(
  node: SankeyNode | undefined,
): 'agent' | 'model' | 'connector' | null {
  switch (node?.type) {
    case 'CLIENT':
      return 'agent'
    case 'MODEL':
      return node.key === SANKEY_OTHER_MODELS_KEY ? null : 'model'
    case 'CONNECTOR':
      return node.key === SANKEY_OTHER_CONNECTORS_KEY ||
        node.key === SANKEY_GATEWAY_TOOLS_KEY
        ? null
        : 'connector'
    default:
      return null
  }
}

/** A bar's label block. The renderer hands each label a 16px band centred on
 * its bar; a two-line block (model + provider) is centred on that band so it
 * grows evenly above and below the bar's midline. */
function NodeLabel({
  node,
  meta,
}: {
  node: PositionedNode
  meta: SankeyNode | undefined
}) {
  const type = meta?.type
  const left = type === 'CLIENT'
  const sublabel = type === 'MODEL' ? providerLabel(meta?.sublabel) : ''
  return (
    <div className="relative h-4" style={{ width: node.labelWidth }}>
      <div
        className={cn(
          'absolute top-1/2 flex -translate-y-1/2 flex-col leading-4',
          left ? 'right-0 items-end text-right' : 'left-0 items-start',
        )}
        style={{ maxWidth: node.labelWidth }}
      >
        <div className="flex max-w-full items-baseline gap-1 whitespace-nowrap">
          <span
            className={cn(
              'min-w-0 truncate text-[12.5px] font-semibold text-foreground',
              type === 'CONNECTOR' && 'font-mono text-[12px]',
            )}
            title={node.label}
          >
            {node.label}
          </span>
          <span className="shrink-0 text-[12px] tabular-nums text-text-subtle">
            {formatCompactNumber(node.value)}
          </span>
        </div>
        {sublabel ? (
          <span className="max-w-full truncate text-[11px] text-text-subtle">
            {sublabel}
          </span>
        ) : null}
      </div>
    </div>
  )
}

/**
 * "Agent traffic flow" Overview card: renders `GET
 * /api/v1/analytics/traffic-flow` as a balanced CLIENT -> PATH -> MODEL |
 * CONNECTOR Sankey (see `@/components/app/sankey` and
 * docs/observability.md#agent-traffic-flow). Follows the page's period
 * filter; the agent select narrows the graph to one client family.
 */
export function TrafficFlowCard({
  data,
  period,
  clientName,
  onClientNameChange,
  emptyLabel = 'No data yet · enable the ClickHouse sink',
  loading = false,
  help,
}: TrafficFlowCardProps) {
  const navigate = useNavigate()
  const hasData = !!data && data.nodes.length > 0
  const rangeLabel =
    period.range === 'custom' ? periodLabel(period) : periodLabel(period).toLowerCase()

  const chart = useMemo(() => (hasData ? toBalancedData(data!) : null), [hasData, data])
  const meta = chart?.meta

  // The select lists the agents the window actually saw (`agents` is
  // computed server-side before the filter, so it stays complete while one
  // is picked). A selected agent that has since dropped out of the window
  // is kept as an option so the select never shows a value it lacks.
  const agentOptions = useMemo(() => {
    const agents = data?.agents ?? []
    if (clientName && !agents.some((a) => a.key === clientName)) {
      return [...agents, { key: clientName, label: clientName, value: 0 }]
    }
    return agents
  }, [data, clientName])

  const open = useCallback(
    (node: SankeyNode | undefined) => {
      switch (nodeTarget(node)) {
        case 'agent':
          onClientNameChange(clientName === node!.key ? '' : node!.key)
          break
        case 'model':
          navigate(`/llm-logs?model=${encodeURIComponent(node!.key)}`)
          break
        case 'connector':
          navigate('/connectors')
          break
      }
    },
    [clientName, navigate, onClientNameChange],
  )

  const handleNodeClick = useCallback(
    (n: BalancedNode) => open(meta?.get(n.key)),
    [meta, open],
  )
  // A ribbon opens whichever end has somewhere to go: the agent on a
  // CLIENT -> PATH ribbon, the model or connector on a PATH -> target one.
  const handleLinkClick = useCallback(
    (l: BalancedLink) => {
      const source = meta?.get(l.source)
      const target = meta?.get(l.target)
      open(nodeTarget(target) ? target : source)
    },
    [meta, open],
  )
  const isNodeClickable = useCallback(
    (n: PositionedNode) => nodeTarget(meta?.get(n.key)) !== null,
    [meta],
  )

  const getNodeColor = useCallback(() => NODE_FILL, [])
  const getLinkColors = useCallback(
    (l: PositionedLink) => {
      const path = linkPath(meta?.get(l.source), meta?.get(l.target))
      return path ? PATH_COLOR[path] : colors.slate['9']
    },
    [meta],
  )
  const renderNodeLabel = useCallback(
    (n: PositionedNode) => <NodeLabel node={n} meta={meta?.get(n.key)} />,
    [meta],
  )

  // Tall enough that the busiest column (the terminal models + connectors,
  // each at least `minNodeHeight` plus a gap) never squeezes.
  const terminalCount = chart
    ? chart.sankey.nodes.filter((n) => n.column === 'TARGET').length
    : 0
  const chartHeight = Math.max(340, 44 * terminalCount)

  return (
    <section className="dash-card @container flex min-h-0 flex-col rounded-r-5 border border-transparent bg-card px-5 py-[18px] text-card-foreground">
      <header className="mb-4 flex min-h-6 flex-wrap items-start justify-between gap-3">
        <div className="flex min-w-0 flex-col gap-0.5">
          <div className="flex items-center gap-2">
            <h2 className="flex items-baseline gap-1 text-base leading-6 font-semibold tracking-[-0.01em] text-foreground">
              <TextAnimate className="whitespace-nowrap">Agent traffic flow</TextAnimate>
            </h2>
            {help}
          </div>
          <p className="text-[12px] text-text-subtle">who called what · {rangeLabel}</p>
        </div>
        <div className="flex flex-wrap items-center gap-4">
          <ul className="flex items-center gap-4" aria-label="Legend">
            {PATH_LEGEND.map((p) => (
              <li
                key={p.key}
                className="flex items-center gap-1.5 text-[12px] text-text-muted"
              >
                <span
                  aria-hidden="true"
                  className="inline-block h-2.5 w-4 rounded-[3px]"
                  style={{ backgroundColor: PATH_COLOR[p.key], opacity: 0.55 }}
                />
                {p.label}
              </li>
            ))}
          </ul>
          <label className="flex items-center gap-2 text-[12px] text-text-muted">
            Agent
            <NativeSelect
              size="sm"
              aria-label="Agent"
              value={clientName}
              onChange={(e) => onClientNameChange(e.target.value)}
            >
              <NativeSelectOption value="">All agents</NativeSelectOption>
              {agentOptions.map((a) => (
                <NativeSelectOption key={a.key} value={a.key}>
                  {a.label}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </label>
        </div>
      </header>

      {loading ? (
        <ChartSkeleton />
      ) : chart ? (
        <div className="w-full py-2">
          <BalancedSankey
            data={chart.sankey}
            height={chartHeight}
            layout={LAYOUT}
            showHeaders={false}
            getNodeColor={getNodeColor}
            getLinkColors={getLinkColors}
            renderNodeLabel={renderNodeLabel}
            formatValue={formatCompactNumber}
            linkOpacity={0.38}
            linkHoverOpacity={0.75}
            linkDimOpacity={0.1}
            onNodeClick={handleNodeClick}
            onLinkClick={handleLinkClick}
            isNodeClickable={isNodeClickable}
            tooltipClassName="rounded-r-2 border border-border bg-bg-elevated px-3 py-2 text-xs shadow-2"
            ariaLabel="Agent traffic flow"
          />
        </div>
      ) : (
        <div className="flex h-[220px] items-center justify-center rounded-r-3 border border-dashed border-border bg-bg-subtle">
          <EmptyState
            label={
              data
                ? clientName
                  ? 'No traffic from this agent in this range'
                  : 'No traffic in this range yet'
                : emptyLabel
            }
          />
        </div>
      )}

      {chart ? (
        <p className="mt-3 text-[12px] text-text-subtle">
          Band width is proportional to call volume. Click an agent to filter, a model to
          open its calls, or an MCP to open its details.
        </p>
      ) : null}
    </section>
  )
}
