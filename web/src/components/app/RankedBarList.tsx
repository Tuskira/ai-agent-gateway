import { EmptyState } from '@/components/app/EmptyState'
import { MiniBar } from '@/components/app/MiniBar'
import { ScrollList } from '@/components/app/ScrollList'
import { chartColor, pctOfMax } from '@/lib/overview'
import { cn } from '@/lib/utils'

export interface RankedBarItem {
  key: string
  label: string
  value: number
  displayValue: string
  /** Optional small status dot rendered before the label (e.g. connector health). */
  dotClassName?: string
}

interface RankedBarListProps {
  items: RankedBarItem[] | null
  /**
   * `inline` puts label, bar and count on one line, in columns shared by
   * every inline list on the page. `stacked` puts the label and count above
   * a full-width bar — the taller rows used in the hero cards.
   */
  variant?: 'inline' | 'stacked'
  labelClassName?: string
  emptyLabel?: string
}

const rowHoverClassName =
  '-mx-2 rounded-r-3 px-2 transition-colors duration-200 hover:bg-bg-subtle'

function RowLabel({ item, className }: { item: RankedBarItem; className?: string }) {
  return (
    <span className={cn('flex min-w-0 items-center gap-1.5', className)}>
      {item.dotClassName ? (
        <span
          className={cn('size-[7px] shrink-0 rounded-full', item.dotClassName)}
          aria-hidden="true"
        />
      ) : null}
      <span className="truncate">{item.label}</span>
    </span>
  )
}

/**
 * The shared "name — bar — count" row pattern used by Top agents /
 * profiles, Requests by client, MCP Tools, and Top connectors. Fills the
 * height it is given and scrolls inside it, so a long list never stretches
 * its card. Each row's bar takes the next chart color. Renders the mock's
 * exact empty caption when `items` is `null` or empty.
 */
export function RankedBarList({
  items,
  variant = 'inline',
  labelClassName = 'text-[13px] text-text-muted',
  emptyLabel,
}: RankedBarListProps) {
  if (!items || items.length === 0) {
    return (
      <div className="flex min-h-[96px] flex-1 items-center justify-center py-4">
        <EmptyState label={emptyLabel} />
      </div>
    )
  }

  const max = Math.max(...items.map((i) => i.value))

  return (
    <ScrollList className="-mx-2 min-h-0 flex-1">
      <div
        className={cn(
          'bar-list flex flex-col px-2',
          variant === 'stacked' ? 'gap-2.5' : 'gap-1',
        )}
      >
        {items.map((item, i) =>
          variant === 'stacked' ? (
            <div
              key={item.key}
              className={cn('flex flex-col gap-1.5 py-1.5', rowHoverClassName)}
            >
              <div className="flex items-center justify-between gap-3 text-[13px] leading-5">
                <RowLabel item={item} className={labelClassName} />
                <span className="shrink-0 font-semibold tabular-nums text-foreground">
                  {item.displayValue}
                </span>
              </div>
              <div className="h-2">
                <MiniBar
                  pct={pctOfMax(item.value, max)}
                  color={chartColor(i)}
                  heightPx={8}
                />
              </div>
            </div>
          ) : (
            <div
              key={item.key}
              className={cn(
                'grid grid-cols-[minmax(0,2fr)_minmax(0,3fr)_3rem] items-center gap-3 py-1.5 text-[13px] leading-5',
                rowHoverClassName,
              )}
            >
              <RowLabel item={item} className={labelClassName} />
              <div className="h-2">
                <MiniBar
                  pct={pctOfMax(item.value, max)}
                  color={chartColor(i)}
                  heightPx={8}
                />
              </div>
              <span className="text-right font-semibold tabular-nums text-foreground">
                {item.displayValue}
              </span>
            </div>
          ),
        )}
      </div>
    </ScrollList>
  )
}
