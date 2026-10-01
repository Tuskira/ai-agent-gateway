import type { ReactNode } from 'react'
import type { LucideIcon } from 'lucide-react'
import { TextAnimate } from '@/components/app/TextAnimate'
import { cn } from '@/lib/utils'

interface KpiCardProps {
  title: string
  icon: LucideIcon
  /** Static Tailwind classes tinting the icon chip, e.g.
   * `"bg-primary/15 text-primary"`. */
  chipClassName: string
  /** Contextual help rendered next to the title — normally a `<WidgetHelp />`. */
  help?: ReactNode
  /** Small caption shown after the title (e.g. the cost-estimate note). */
  titleNote?: ReactNode
  /** The headline value: a ticker, a placeholder dash, or a skeleton. */
  value: ReactNode
  /** Left side of the bottom row: delta and comparison text. */
  footer?: ReactNode
  /** Right side of the bottom row, e.g. a details link. */
  footerAction?: ReactNode
}

/**
 * Overview KPI tile. Every tile has the same three fixed-height rows —
 * title + icon chip, value, footer — so a row of tiles lines up whatever
 * each one has to say; the optional title note shares the chip's row rather
 * than adding one. On hover the value lifts, the chip lifts and scales, the
 * glyph tilts and the footer settles.
 */
export function KpiCard({
  title,
  icon: Icon,
  chipClassName,
  help,
  titleNote,
  value,
  footer,
  footerAction,
}: KpiCardProps) {
  return (
    <div className="dash-card group/kpi @container flex flex-col gap-2 rounded-r-5 border border-transparent bg-card p-5 text-card-foreground">
      <div className="flex h-10 items-center justify-between gap-3">
        <div className="flex min-w-0 flex-col">
          <div className="flex min-w-0 items-center gap-0.5">
            <div className="truncate text-[13px] leading-5 font-medium text-text-muted transition-colors duration-300 group-hover/kpi:text-foreground">
              <TextAnimate>{title}</TextAnimate>
            </div>
            {help}
          </div>
          {titleNote ? (
            <div className="truncate text-[11px] leading-4">{titleNote}</div>
          ) : null}
        </div>
        <span
          className={cn(
            'flex size-10 shrink-0 items-center justify-center rounded-[14px] transition-[transform,box-shadow] duration-300 ease-out',
            'motion-safe:group-hover/kpi:-translate-y-0.5 motion-safe:group-hover/kpi:scale-105',
            'group-hover/kpi:shadow-[inset_0_0_0_1px_color-mix(in_oklch,var(--border)_80%,transparent),0_8px_16px_-10px_color-mix(in_oklch,var(--foreground)_25%,transparent)]',
            chipClassName,
          )}
        >
          <Icon
            className="size-5 transition-transform duration-500 ease-out motion-safe:group-hover/kpi:scale-110 motion-safe:group-hover/kpi:rotate-6"
            aria-hidden="true"
          />
        </span>
      </div>
      <div className="flex h-9 items-center truncate text-[28px] leading-9 font-bold tracking-[-0.02em] tabular-nums text-foreground transition-transform duration-300 ease-out motion-safe:group-hover/kpi:-translate-y-0.5">
        {value}
      </div>
      <div className="flex min-h-5 items-center justify-between gap-2 text-xs leading-5 text-text-subtle transition-transform duration-300 ease-out motion-safe:group-hover/kpi:translate-y-0.5">
        <span className="min-w-0 truncate">{footer}</span>
        {footerAction ? <span className="shrink-0">{footerAction}</span> : null}
      </div>
    </div>
  )
}
