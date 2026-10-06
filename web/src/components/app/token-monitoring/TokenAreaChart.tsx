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
import { EmptyState } from '@/components/app/EmptyState'
import { formatCompactNumber } from '@/lib/overview'
import { bucketLabel, type TokenBucket } from '@/lib/token-monitoring'
import { colors } from '@/styles/tokens'

const COLOR = colors.sev.medium

function ChartTooltip({ active, payload, label }: TooltipContentProps) {
  if (!active || !payload || payload.length === 0) return null
  return (
    <div className="rounded-r-2 border border-border bg-bg-elevated px-3 py-2 text-xs shadow-2">
      <div className="mb-1 font-medium text-text-muted">{label} UTC</div>
      <div className="font-semibold tabular-nums text-foreground">
        {formatCompactNumber(Number(payload[0]?.value ?? 0))} tokens
      </div>
    </div>
  )
}

/** Tokens per UTC hour (Today) or day (7d / 30d). */
export function TokenAreaChart({
  points,
  granularity,
  emptyLabel,
}: {
  points: TokenBucket[] | null
  granularity: 'hour' | 'day'
  emptyLabel?: string
}) {
  const reduceMotion = useReducedMotion()
  if (!points || points.every((p) => p.tokens === 0)) {
    return (
      <div className="flex h-[220px] items-center justify-center rounded-r-3 border border-dashed border-border bg-bg-subtle">
        <EmptyState label={emptyLabel} />
      </div>
    )
  }
  const data = points.map((p) => ({
    label: bucketLabel(p.bucket, granularity),
    tokens: p.tokens,
  }))
  return (
    <div className="h-[220px] w-full">
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={data} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
          <defs>
            <linearGradient id="token-monitoring-fill" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={COLOR} stopOpacity={0.25} />
              <stop offset="100%" stopColor={COLOR} stopOpacity={0.02} />
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
            width={60}
            tick={{ fontSize: 11, fill: 'var(--text-subtle)' }}
            tickFormatter={(v: number) => formatCompactNumber(v)}
          />
          <Tooltip content={ChartTooltip} cursor={{ stroke: 'var(--border-strong)' }} />
          <Area
            type="monotone"
            dataKey="tokens"
            name="Tokens"
            stroke={COLOR}
            strokeWidth={2}
            fill="url(#token-monitoring-fill)"
            isAnimationActive={!reduceMotion}
            animationDuration={900}
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  )
}
