import type { ReactNode } from 'react'
import { ScrollArea } from '@base-ui/react/scroll-area'
import { cn } from '@/lib/utils'

interface ScrollListProps {
  children: ReactNode
  /** Sizing for the scroll container — it scrolls once it has a bounded
   * height (e.g. `min-h-0 flex-1` inside a fixed-height card). */
  className?: string
}

/**
 * Vertical scroll container for the dashboard's lists. The scrollbar stays
 * out of sight until the pointer is over the list or the list is being
 * scrolled, so a list that only just overflows doesn't carry a permanent
 * track down its edge.
 */
export function ScrollList({ children, className }: ScrollListProps) {
  return (
    <ScrollArea.Root className={cn('relative', className)}>
      <ScrollArea.Viewport className="size-full rounded-[inherit] outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50">
        {children}
      </ScrollArea.Viewport>
      <ScrollArea.Scrollbar
        orientation="vertical"
        className="flex w-2 touch-none p-px opacity-0 transition-opacity duration-200 select-none data-hovering:opacity-100 data-scrolling:opacity-100 motion-reduce:transition-none"
      >
        <ScrollArea.Thumb className="relative flex-1 rounded-full bg-border-strong" />
      </ScrollArea.Scrollbar>
    </ScrollArea.Root>
  )
}
