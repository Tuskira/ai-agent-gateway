import { useReducedMotion } from 'motion/react'
import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  type TooltipContentProps,
  XAxis,
  YAxis,
} from 'recharts'
import { ColorSwatch } from '@/components/app/ColorSwatch'
import { EmptyState } from '@/components/app/EmptyState'
import { formatCompactNumber, type TrafficPoint } from '@/lib/overview'
import { colors } from '@/styles/tokens'

interface TrafficAreaChartProps {
  points: TrafficPoint[] | null
  emptyLabel?: string
}

const LLM_COLOR = colors.sev.medium
const MCP_COLOR = colors.brand.cyanTo

function ChartTooltip({ active, payload, label }: TooltipContentProps) {
  if (!active || !payload || payload.length === 0) return null
  return (
    <div className="rounded-r-2 border border-border bg-bg-elevated px-3 py-2 text-xs shadow-2">
      <div className="mb-1.5 font-medium text-text-muted">{label}</div>
      <div className="flex flex-col gap-1">
        {payload.map((entry) => (
          <div key={String(entry.dataKey)} className="flex items-center gap-1.5">
            <ColorSwatch
              color={(entry.color as string) ?? 'var(--text-subtle)'}
              size={8}
            />
            <span className="text-text-muted">{entry.name}</span>
            <span className="ml-auto font-semibold tabular-nums text-foreground">
              {formatCompactNumber(Number(entry.value ?? 0))}
            </span>
          </div>
        ))}
      </div>
    </div>
  )
}

/** The full-width "Traffic Over Time" area chart. Renders a muted empty
 * area with a centered caption when `points` is `null`. */
export function TrafficAreaChart({ points, emptyLabel }: TrafficAreaChartProps) {
  const reduceMotion = useReducedMotion()
  const hasData = !!points && points.length > 0

  if (!hasData) {
    return (
      <div className="flex h-[220px] items-center justify-center rounded-r-3 border border-dashed border-border bg-bg-subtle">
        <EmptyState label={emptyLabel} />
      </div>
    )
  }

  return (
    <div className="h-[220px] w-full">
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={points} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
          <defs>
            <linearGradient id="overview-llm-fill" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={LLM_COLOR} stopOpacity={0.25} />
              <stop offset="100%" stopColor={LLM_COLOR} stopOpacity={0.02} />
            </linearGradient>
            <linearGradient id="overview-mcp-fill" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={MCP_COLOR} stopOpacity={0.25} />
              <stop offset="100%" stopColor={MCP_COLOR} stopOpacity={0.02} />
            </linearGradient>
          </defs>
          <CartesianGrid vertical={false} strokeDasharray="3 3" stroke="var(--border)" />
          <XAxis
            dataKey="label"
            tickLine={false}
            axisLine={false}
            tick={{ fontSize: 11, fill: 'var(--text-subtle)' }}
            minTickGap={24}
          />
          <YAxis
            tickLine={false}
            axisLine={false}
            width={36}
            tick={{ fontSize: 11, fill: 'var(--text-subtle)' }}
          />
          <Tooltip content={ChartTooltip} cursor={{ stroke: 'var(--border-strong)' }} />
          <Area
            type="monotone"
            dataKey="llmCalls"
            name="LLM calls"
            stroke={LLM_COLOR}
            strokeWidth={2}
            fill="url(#overview-llm-fill)"
            isAnimationActive={!reduceMotion}
            animationDuration={900}
            animationEasing="ease-out"
          />
          <Area
            type="monotone"
            dataKey="mcpCalls"
            name="MCP calls"
            stroke={MCP_COLOR}
            strokeWidth={2}
            fill="url(#overview-mcp-fill)"
            isAnimationActive={!reduceMotion}
            animationDuration={900}
            animationEasing="ease-out"
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  )
}
