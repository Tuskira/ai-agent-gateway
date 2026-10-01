import type { ReactNode } from 'react'
import { TextAnimate } from '@/components/app/TextAnimate'
import { cn } from '@/lib/utils'

interface DashboardCardProps {
  title: string
  /** Contextual help rendered next to the title — normally a `<WidgetHelp />`. */
  help?: ReactNode
  /** Right-aligned header content: a link, a legend, a caption. */
  action?: ReactNode
  children: ReactNode
  /** Sizing for the card itself — the caller sets the height. */
  className?: string
  bodyClassName?: string
}

/**
 * Shared shell for the Overview widgets: a hover-lit card with an animated
 * title, an optional info hint, an optional header action and a body that fills whatever height
 * the card is given. Every widget uses it, so headers line up across a row
 * and each body starts at the same offset.
 */
export function DashboardCard({
  title,
  help,
  action,
  children,
  className,
  bodyClassName,
}: DashboardCardProps) {
  return (
    <section
      className={cn(
        'dash-card @container flex min-h-0 flex-col rounded-r-5 border border-transparent bg-card px-5 py-[18px] text-card-foreground',
        className,
      )}
    >
      <header className="mb-4 flex min-h-6 items-center justify-between gap-3">
        <div className="flex shrink-0 items-center gap-1">
          <h2 className="text-base leading-6 font-semibold tracking-[-0.01em] text-foreground">
            <TextAnimate className="whitespace-nowrap">{title}</TextAnimate>
          </h2>
          {help}
        </div>
        {action ? <div className="min-w-0 truncate">{action}</div> : null}
      </header>
      <div className={cn('flex min-h-0 flex-1 flex-col', bodyClassName)}>{children}</div>
    </section>
  )
}
