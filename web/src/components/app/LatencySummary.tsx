import { Clock } from 'lucide-react'
import { EmptyState } from '@/components/app/EmptyState'
import { NumberTicker } from '@/components/app/NumberTicker'
import { ScrollList } from '@/components/app/ScrollList'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { SLOW_DURATION_MS } from '@/lib/logs'
import { formatDuration, formatTimes, pctOfMax, type SlowCall } from '@/lib/overview'
import { cn } from '@/lib/utils'

interface LatencySummaryProps {
  latency: { medianMs: number; p95Ms: number; slowest: SlowCall[] } | null
  loading: boolean
  /** Caption for the list when there is nothing to show. */
  emptyLabel: string
}

const BAR_HEIGHT = 8
/** The p95 tick stands a little taller than the bar it crosses. */
const TICK_OVERHANG = 2

/** Marks a duration over the threshold the log pages also call slow. */
function SlowMark() {
  return (
    <>
      <Clock className="size-3 shrink-0 text-sev-medium" aria-hidden="true" />
      <span className="sr-only">Slow</span>
    </>
  )
}

function Stat({
  label,
  value,
  loading,
  caption,
}: {
  label: string
  value: number | null
  loading: boolean
  caption?: string | null
}) {
  return (
    <div role="group" aria-label={label} className="min-w-0">
      <div className="text-xs leading-4 text-text-subtle" aria-hidden="true">
        {label}
      </div>
      <div className="flex h-7 items-baseline gap-1.5">
        {loading ? (
          <Skeleton className="h-5 w-16 self-center" />
        ) : value === null ? (
          <span className="text-xl font-bold text-foreground">—</span>
        ) : (
          <>
            <span className="flex items-center gap-1 text-xl leading-7 font-bold text-foreground">
              <NumberTicker value={value} format={formatDuration} />
              {value > SLOW_DURATION_MS ? <SlowMark /> : null}
            </span>
            {caption ? (
              <span className="truncate text-[11.5px] leading-4 text-text-subtle">
                {caption}
              </span>
            ) : null}
          </>
        )}
      </div>
    </div>
  )
}

/**
 * One call's duration as a bar against the slowest call, crossed by a tick at
 * p95. Drawn as SVG with percentage attributes, like `MiniBar`, which keeps
 * per-row geometry out of the `style` prop.
 */
function DurationBar({
  pct,
  p95Pct,
  slow,
}: {
  pct: number
  p95Pct: number | null
  slow: boolean
}) {
  const top = TICK_OVERHANG
  return (
    <svg
      className="block w-full overflow-visible"
      height={BAR_HEIGHT + TICK_OVERHANG * 2}
      role="presentation"
    >
      <rect
        x={0}
        y={top}
        width="100%"
        height={BAR_HEIGHT}
        rx={BAR_HEIGHT / 2}
        className="fill-[var(--bg-muted)]"
      />
      <rect
        x={0}
        y={top}
        width={`${pct}%`}
        height={BAR_HEIGHT}
        rx={BAR_HEIGHT / 2}
        className={cn('bar-fill', slow ? 'fill-sev-medium' : 'fill-primary')}
      />
      {p95Pct !== null ? (
        <>
          {/* A ring in the surface colour keeps the tick legible where it
              crosses the fill. */}
          <line
            x1={`${p95Pct}%`}
            x2={`${p95Pct}%`}
            y1={0}
            y2={BAR_HEIGHT + TICK_OVERHANG * 2}
            strokeWidth={4.5}
            className="stroke-card"
          />
          <line
            x1={`${p95Pct}%`}
            x2={`${p95Pct}%`}
            y1={0}
            y2={BAR_HEIGHT + TICK_OVERHANG * 2}
            strokeWidth={1.5}
            className="stroke-foreground"
          />
        </>
      ) : null}
    </svg>
  )
}

function SlowestCalls({ calls, p95Ms }: { calls: SlowCall[]; p95Ms: number }) {
  const max = Math.max(...calls.map((c) => c.ms))
  // A p95 above every listed call has no place on a scale that ends at the
  // slowest of them.
  const p95Pct = max > 0 && p95Ms > 0 && p95Ms <= max ? (p95Ms / max) * 100 : null

  return (
    <TooltipProvider delay={200}>
      <ScrollList className="-mx-2 min-h-0 shrink">
        <ul aria-label="Slowest calls" className="bar-list flex flex-col px-2">
          {calls.map((call, i) => {
            const duration = formatDuration(call.ms)
            const times = formatTimes(call.ms, p95Ms)
            const slow = call.ms > SLOW_DURATION_MS
            return (
              <Tooltip key={`${call.name}-${i}`}>
                <TooltipTrigger
                  render={
                    <li
                      tabIndex={0}
                      aria-label={[call.name, duration, times ? `${times} p95` : null]
                        .filter(Boolean)
                        .join(', ')}
                      className="-mx-2 grid grid-cols-[minmax(0,5fr)_minmax(0,4fr)_4rem] items-center gap-3 rounded-r-3 px-2 py-1 outline-none transition-colors duration-200 hover:bg-bg-subtle focus-visible:ring-3 focus-visible:ring-ring/50"
                    />
                  }
                >
                  <span className="truncate font-mono text-xs leading-5 text-text-muted">
                    {call.name}
                  </span>
                  <DurationBar pct={pctOfMax(call.ms, max)} p95Pct={p95Pct} slow={slow} />
                  <span className="flex items-center justify-end gap-1 text-[12.5px] leading-5 font-semibold text-foreground tabular-nums">
                    {slow ? <SlowMark /> : null}
                    {duration}
                  </span>
                </TooltipTrigger>
                <TooltipContent
                  side="bottom"
                  align="start"
                  className="flex-col items-start gap-0.5"
                >
                  <span className="font-mono">{call.name}</span>
                  <span className="opacity-80">
                    {Math.round(call.ms).toLocaleString()} ms
                    {times ? ` · ${times} p95` : ''}
                  </span>
                </TooltipContent>
              </Tooltip>
            )
          })}
        </ul>
      </ScrollList>
    </TooltipProvider>
  )
}

/**
 * The body of the Overview's Latency card: the median and p95 with the gap
 * between them spelled out, then the slowest calls as bars on one scale.
 *
 * Colour carries one meaning here. A duration is marked, with an icon as well
 * as colour, only when it passes the threshold the log pages call slow;
 * everything else stays in ordinary ink so that mark is easy to find.
 */
export function LatencySummary({ latency, loading, emptyLabel }: LatencySummaryProps) {
  const hasCalls = latency !== null && latency.slowest.length > 0
  const times = latency ? formatTimes(latency.p95Ms, latency.medianMs) : null

  return (
    <>
      <div className="grid grid-cols-2 gap-3">
        <Stat label="Median" value={latency?.medianMs ?? null} loading={loading} />
        <Stat
          label="p95"
          value={latency?.p95Ms ?? null}
          loading={loading}
          caption={times ? `${times} median` : null}
        />
      </div>

      <div className="flex items-center justify-between gap-3 text-xs leading-4 text-text-subtle">
        <span>Slowest calls</span>
        {hasCalls ? (
          <span className="flex items-center gap-1.5" aria-hidden="true">
            <span className="h-3 w-[1.5px] bg-foreground" />
            p95
          </span>
        ) : null}
      </div>

      {loading ? (
        <div className="flex flex-col gap-3" role="status" aria-label="Loading">
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-full" />
        </div>
      ) : hasCalls ? (
        <SlowestCalls calls={latency.slowest} p95Ms={latency.p95Ms} />
      ) : (
        <div className="flex min-h-[80px] flex-1 items-center justify-center rounded-r-4 border border-border py-3">
          <EmptyState label={emptyLabel} />
        </div>
      )}
    </>
  )
}
