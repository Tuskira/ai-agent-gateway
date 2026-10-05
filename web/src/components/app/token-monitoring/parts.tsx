import { StatusPill, type PillTone } from '@/components/app/StatusPill'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { formatDeltaPct } from '@/lib/overview'
import {
  MONITORING_WINDOWS,
  tokenSplit,
  type CallerRole,
  type MonitoringWindow,
  type TokenUsage,
} from '@/lib/token-monitoring'
import { cn } from '@/lib/utils'

/** Today / Last 7 Days / Last 30 Days (UTC): the shared ToggleGroup,
 * styled as a segmented control; one window is always selected. */
export function WindowSwitch({
  value,
  onChange,
}: {
  value: MonitoringWindow
  onChange: (w: MonitoringWindow) => void
}) {
  return (
    <ToggleGroup
      aria-label="Time window"
      value={[value]}
      onValueChange={(v) => v[0] && onChange(v[0] as MonitoringWindow)}
      className="gap-0 rounded-r-4 border border-border bg-card p-0.5"
    >
      {MONITORING_WINDOWS.map((w) => (
        <ToggleGroupItem
          key={w.value}
          value={w.value}
          className="h-8 rounded-r-3 px-3 text-[13px] text-text-muted transition-colors hover:bg-transparent hover:text-foreground aria-pressed:bg-primary aria-pressed:text-primary-foreground"
        >
          {w.label}
        </ToggleGroupItem>
      ))}
    </ToggleGroup>
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
