import { Skeleton } from '@/components/ui/skeleton'
import { cn } from '@/lib/utils'

/*
 * Loading placeholders for the Overview widgets. Each one matches the footprint of the content it stands in
 * for, so nothing shifts when the numbers arrive. Every size is a static
 * class: Tailwind has to see the full string, and nothing here is dynamic.
 */

const ROW_WIDTHS = ['w-[62%]', 'w-[48%]', 'w-[70%]', 'w-[40%]', 'w-[55%]'] as const

const COLUMN_HEIGHTS = [
  'h-[30%]',
  'h-[47%]',
  'h-[64%]',
  'h-[81%]',
  'h-[38%]',
  'h-[55%]',
  'h-[72%]',
  'h-[89%]',
  'h-[46%]',
  'h-[63%]',
  'h-[80%]',
  'h-[37%]',
  'h-[54%]',
  'h-[71%]',
  'h-[88%]',
  'h-[45%]',
] as const

/** A track with a slowly turning ring, plus legend rows beside it where
 * the card has room for them. */
export function DonutSkeleton({ sizeClassName }: { sizeClassName: string }) {
  return (
    <div
      className="flex flex-1 items-center justify-center gap-6"
      role="status"
      aria-label="Loading"
    >
      <div className={cn('relative shrink-0', sizeClassName)}>
        <div className="absolute inset-0 rounded-full border-[18px] border-bg-muted" />
        <div className="absolute inset-0 rounded-full border-[18px] border-transparent border-t-border-strong motion-safe:animate-[spin_4s_linear_infinite]" />
        <div className="absolute inset-0 flex flex-col items-center justify-center gap-2">
          <Skeleton className="h-6 w-14" />
          <Skeleton className="h-3 w-10" />
        </div>
      </div>
      <div className="hidden w-full max-w-[200px] min-w-[130px] flex-col gap-3.5 @min-[380px]:flex">
        <Skeleton className="h-3.5 w-full" />
        <Skeleton className="h-3.5 w-4/5" />
      </div>
    </div>
  )
}

/** Label-over-bar rows, for the ranked lists and LLM Usage. */
export function BarListSkeleton({ rows = 4 }: { rows?: number }) {
  return (
    <div className="flex flex-col gap-4" role="status" aria-label="Loading">
      {ROW_WIDTHS.slice(0, rows).map((width, i) => (
        <div key={i} className="flex flex-col gap-2">
          <div className="flex items-center justify-between">
            <Skeleton className={cn('h-3.5', width)} />
            <Skeleton className="h-3.5 w-8" />
          </div>
          <Skeleton className="h-2 w-full rounded-full" />
        </div>
      ))}
    </div>
  )
}

/** Columns of uneven height along a baseline, for Traffic Over Time. */
export function ChartSkeleton() {
  return (
    <div
      className="flex h-[220px] items-end gap-2 border-b border-border pb-px"
      role="status"
      aria-label="Loading"
    >
      {COLUMN_HEIGHTS.map((height, i) => (
        <Skeleton key={i} className={cn('flex-1 rounded-b-none', height)} />
      ))}
    </div>
  )
}
