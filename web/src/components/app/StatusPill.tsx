import { cn } from '@/lib/utils'

export type PillTone = 'positive' | 'negative' | 'neutral' | 'info' | 'accent'

const toneClasses: Record<PillTone, string> = {
  positive: 'bg-status-resolved-bg text-status-resolved',
  negative: 'bg-sev-high-bg text-sev-high-fg',
  neutral: 'bg-bg-muted text-text-muted',
  info: 'bg-status-open-bg text-status-open',
  // Distinct from "info" (registered) — a call to action, not a state the
  // gateway is already managing. Uses the design system's existing
  // "deferred" status token (cyan), unused elsewhere in this pill today.
  accent: 'bg-status-deferred-bg text-status-deferred',
}

const outlineClasses: Record<PillTone, string> = {
  positive: 'border border-dashed border-status-resolved text-status-resolved',
  negative: 'border border-dashed border-sev-high text-sev-high-fg',
  neutral: 'border border-dashed border-border-strong text-text-muted',
  info: 'border border-dashed border-status-open text-status-open',
  accent: 'border border-dashed border-status-deferred text-status-deferred',
}

const dotClasses: Record<PillTone, string> = {
  positive: 'bg-status-resolved',
  negative: 'bg-sev-high',
  neutral: 'bg-border-strong',
  info: 'bg-status-open',
  accent: 'bg-status-deferred',
}

interface StatusPillProps {
  tone: PillTone
  label: string
  className?: string
  /** Native tooltip, e.g. what the status means. */
  title?: string
  /** Dashed outline instead of a filled pill, for rows that are not (yet)
   * this tenant's own (Discovered, Available). */
  outline?: boolean
}

/** Small dot + label pill for status columns (connector health, API key
 * active/revoked, cache freshness, …). */
export function StatusPill({ tone, label, className, title, outline }: StatusPillProps) {
  return (
    <span
      title={title}
      className={cn(
        'inline-flex h-5 w-fit items-center gap-1.5 rounded-r-pill px-2 text-[11px] font-semibold whitespace-nowrap',
        outline ? outlineClasses[tone] : toneClasses[tone],
        className,
      )}
    >
      <span
        className={cn('size-1.5 shrink-0 rounded-full', dotClasses[tone])}
        aria-hidden="true"
      />
      {label}
    </span>
  )
}
