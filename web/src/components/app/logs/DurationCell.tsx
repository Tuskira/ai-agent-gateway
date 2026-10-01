import { Clock } from 'lucide-react'
import { SLOW_DURATION_MS } from '@/lib/logs'
import { formatReadableDuration } from '@/lib/duration'
import { formatMs } from '@/lib/overview'
import { cn } from '@/lib/utils'

interface DurationCellProps {
  ms: number
  className?: string
  /** Show "3m 53.8s" style text instead of whole milliseconds. The slow
   * threshold is the same either way. */
  readable?: boolean
}

/** Plain tabular-nums text under the threshold; an orange clock badge over
 * it — matches the mock's "Duration over 5000ms — slow, even when status is
 * 200" legend. */
export function DurationCell({ ms, className, readable = false }: DurationCellProps) {
  const slow = ms > SLOW_DURATION_MS
  const text = readable ? formatReadableDuration(ms) : formatMs(ms)

  if (!slow) {
    return (
      <span className={cn('tabular-nums text-text-muted', className)}>
        {text}
      </span>
    )
  }

  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 rounded-r-3 bg-sev-medium-bg px-2 py-1 text-[13px] font-semibold tabular-nums text-sev-medium-fg',
        className,
      )}
    >
      <Clock className="size-3" aria-hidden="true" />
      {text}
    </span>
  )
}
