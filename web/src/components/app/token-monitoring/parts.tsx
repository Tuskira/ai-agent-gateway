import type { ReactNode } from 'react'
import { LayoutGrid, List } from 'lucide-react'
import { StatusPill, type PillTone } from '@/components/app/StatusPill'
import { SegmentedControl } from '@/components/app/SegmentedControl'
import { formatDeltaPct } from '@/lib/overview'
import {
  tokenSplit,
  type CallerRole,
  type CostView,
  type TokenUsage,
} from '@/lib/token-monitoring'
import { cn } from '@/lib/utils'

const COST_VIEWS: { value: CostView; label: string; icon: ReactNode }[] = [
  {
    value: 'cards',
    label: 'Cards',
    icon: <LayoutGrid className="size-4" aria-hidden="true" />,
  },
  { value: 'list', label: 'List', icon: <List className="size-4" aria-hidden="true" /> },
]

/** Cost by model as cards or as a sortable table. */
export function CostViewSwitch({
  value,
  onChange,
}: {
  value: CostView
  onChange: (v: CostView) => void
}) {
  return (
    <SegmentedControl
      label="Cost by model view"
      options={COST_VIEWS}
      value={value}
      onChange={onChange}
    />
  )
}

const ROLE_TONE: Record<CallerRole, PillTone> = {
  agent: 'info',
  admin: 'accent',
  interceptor: 'positive',
  other: 'neutral',
}

export function RolePill({ role }: { role: CallerRole }) {
  return <StatusPill tone={ROLE_TONE[role]} label={role} />
}

/** "▲ 12.5%" / "▼ 3%" vs the previous period; "New" when there was no
 * previous usage but there is now; "—" when there is neither. */
export function DeltaText({ delta, tokens }: { delta: number | null; tokens: number }) {
  if (delta === null) {
    return (
      <span className="font-semibold text-text-muted">{tokens > 0 ? 'New' : '—'}</span>
    )
  }
  return (
    <span
      className={cn(
        'font-semibold',
        delta >= 0 ? 'text-status-resolved' : 'text-sev-high',
      )}
    >
      {delta >= 0 ? '▲' : '▼'} {formatDeltaPct(delta)}
    </span>
  )
}

const SPLIT_COLORS: Record<string, string> = {
  in: 'var(--chart-1)',
  out: 'var(--chart-2)',
  cr: 'var(--chart-3)',
  cw: 'var(--chart-4)',
}

/** Input / output / cache read / cache write as one stacked bar plus chips.
 * An SVG so segment widths need no inline style. */
export function TokenSplit({ usage }: { usage: TokenUsage }) {
  const parts = tokenSplit(usage)
  const total = parts.reduce((s, p) => s + p.value, 0)
  let x = 0
  return (
    <div className="flex flex-col gap-2">
      <svg
        viewBox="0 0 100 4"
        preserveAspectRatio="none"
        className="h-1.5 w-full rounded-r-pill bg-bg-muted"
        aria-hidden="true"
      >
        {total > 0
          ? parts.map((p) => {
              const w = (p.value / total) * 100
              const rect = (
                <rect
                  key={p.key}
                  x={x}
                  y={0}
                  width={w}
                  height={4}
                  fill={SPLIT_COLORS[p.key]}
                />
              )
              x += w
              return rect
            })
          : null}
      </svg>
      <div className="flex flex-wrap gap-1.5">
        {parts.map((p) => (
          <span
            key={p.key}
            className={cn(
              'inline-flex items-center gap-1 rounded-r-pill bg-bg-subtle px-2 py-0.5 text-[11px]',
              p.value === 0 ? 'text-text-subtle' : 'text-foreground',
            )}
          >
            <svg viewBox="0 0 6 6" className="size-1.5" aria-hidden="true">
              <circle cx="3" cy="3" r="3" fill={SPLIT_COLORS[p.key]} />
            </svg>
            {p.name} <span className="font-semibold tabular-nums">{p.label}</span>
          </span>
        ))}
      </div>
    </div>
  )
}

/** A page section of its own (Cost by model, Callers, Sessions): a tinted
 * panel with a larger heading, so it reads apart from the widget cards. */
export function Section({
  title,
  badge,
  help,
  action,
  children,
}: {
  title: string
  badge?: ReactNode
  help?: ReactNode
  action?: ReactNode
  children: ReactNode
}) {
  return (
    <section
      aria-label={title}
      className="flex flex-col gap-4 rounded-r-6 border border-border bg-muted/30 p-5"
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <h2 className="text-[18px] leading-7 font-semibold tracking-[-0.01em] text-foreground">
            {title}
          </h2>
          {badge}
          {help}
        </div>
        {action}
      </div>
      {children}
    </section>
  )
}
