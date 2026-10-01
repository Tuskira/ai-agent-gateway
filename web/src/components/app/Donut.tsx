import type { ReactNode } from 'react'
import { useReducedMotion } from 'motion/react'
import { Cell, Pie, PieChart, ResponsiveContainer } from 'recharts'
import { EmptyState } from '@/components/app/EmptyState'
import { cn } from '@/lib/utils'

export interface DonutSlice {
  key: string
  label: string
  value: number
  /** Hex/CSS color, passed straight to recharts' `<Cell fill>`. */
  color: string
}

interface DonutProps {
  /** Static Tailwind size classes, e.g. `"size-[190px]"`. The ring is drawn
   * to fill whatever size these give it, so they may vary with the card's
   * width (`"size-[140px] @min-[340px]:size-[184px]"`). */
  sizeClassName: string
  slices: DonutSlice[] | null
  centerValue: ReactNode
  centerLabel: string
  centerValueClassName?: string
  centerLabelClassName?: string
  emptyLabel?: string
}

/* Ring radii as a share of the available radius, so the ring scales with
 * the element: 18px thick at 184px across. */
const OUTER_RADIUS = '96%'
const INNER_RADIUS = '76%'
const NO_MARGIN = { top: 0, right: 0, bottom: 0, left: 0 }

/** The Traffic Distribution / Health · status codes donut. Renders a
 * muted empty ring with a centered caption when `slices` is `null`. */
export function Donut({
  sizeClassName,
  slices,
  centerValue,
  centerLabel,
  centerValueClassName = 'text-[32px] font-bold',
  centerLabelClassName = 'text-[11.5px] text-text-subtle',
  emptyLabel,
}: DonutProps) {
  const reduceMotion = useReducedMotion()
  const hasData = !!slices && slices.length > 0 && slices.some((s) => s.value > 0)

  return (
    <div className={cn('relative shrink-0', sizeClassName)}>
      {hasData ? (
        <ResponsiveContainer width="100%" height="100%">
          <PieChart margin={NO_MARGIN}>
            <Pie
              data={slices}
              dataKey="value"
              nameKey="label"
              cx="50%"
              cy="50%"
              innerRadius={INNER_RADIUS}
              outerRadius={OUTER_RADIUS}
              startAngle={90}
              endAngle={-270}
              stroke="var(--bg-elevated)"
              strokeWidth={2}
              isAnimationActive={!reduceMotion}
              animationDuration={800}
              animationEasing="ease-out"
            >
              {slices.map((s) => (
                <Cell
                  key={s.key}
                  fill={s.color}
                  className="transition-opacity hover:opacity-85"
                />
              ))}
            </Pie>
          </PieChart>
        </ResponsiveContainer>
      ) : (
        <div
          className={cn(
            'flex size-full items-center justify-center rounded-full border-[18px] border-bg-muted p-3',
          )}
        >
          <EmptyState label={emptyLabel} />
        </div>
      )}
      {hasData ? (
        <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center leading-[1.1]">
          <span className={centerValueClassName}>{centerValue}</span>
          <span className={centerLabelClassName}>{centerLabel}</span>
        </div>
      ) : null}
    </div>
  )
}
