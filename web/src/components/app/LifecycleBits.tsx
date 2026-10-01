import { Calendar } from 'lucide-react'
import { StatusPill } from '@/components/app/StatusPill'
import { TIME_RANGES, type TimeRange } from '@/lib/overview'
import {
  LIFECYCLE_STATES,
  lifecycleLabel,
  lifecycleOutline,
  lifecycleTitle,
  lifecycleTone,
  type LifecycleState,
} from '@/lib/lifecycle'

/** The state pill: label, tone, dashed outline and hover text all come from
 * `lib/lifecycle.ts`. */
export function LifecyclePill({ state }: { state: LifecycleState }) {
  return (
    <StatusPill
      tone={lifecycleTone(state)}
      label={lifecycleLabel(state)}
      title={lifecycleTitle(state)}
      outline={lifecycleOutline(state)}
    />
  )
}

/** Time-range picker, styled like the Overview page's. */
export function RangeSelect({
  value,
  onChange,
}: {
  value: TimeRange
  onChange: (range: TimeRange) => void
}) {
  return (
    <label className="flex h-9 items-center gap-2 rounded-r-4 border border-border bg-card py-0 pr-1 pl-2.5 text-text-subtle transition-colors hover:border-border-strong">
      <Calendar className="size-[15px]" aria-hidden="true" />
      <select
        aria-label="Time range"
        value={value}
        onChange={(e) => onChange(e.target.value as TimeRange)}
        className="h-8 cursor-pointer border-0 bg-transparent text-[13px] font-medium text-foreground outline-none"
      >
        {TIME_RANGES.map((r) => (
          <option key={r.value} value={r.value}>
            {r.label}
          </option>
        ))}
      </select>
    </label>
  )
}

/** Filter by lifecycle state. `states` limits the options to ones that can
 * occur on the page (Discovered is hidden with analytics off). */
export function StateFilter({
  value,
  onChange,
  states = LIFECYCLE_STATES,
  counts,
}: {
  value: LifecycleState | 'all'
  onChange: (state: LifecycleState | 'all') => void
  states?: LifecycleState[]
  counts?: Partial<Record<LifecycleState, number>>
}) {
  return (
    <label className="flex h-9 items-center gap-2 rounded-r-4 border border-border bg-card py-0 pr-1 pl-2.5 text-[13px] text-text-subtle transition-colors hover:border-border-strong">
      State
      <select
        aria-label="Filter by state"
        value={value}
        onChange={(e) => onChange(e.target.value as LifecycleState | 'all')}
        className="h-8 cursor-pointer border-0 bg-transparent text-[13px] font-medium text-foreground outline-none"
      >
        <option value="all">All</option>
        {states.map((s) => (
          <option key={s} value={s}>
            {lifecycleLabel(s)}
            {counts ? ` (${counts[s] ?? 0})` : ''}
          </option>
        ))}
      </select>
    </label>
  )
}

/** One-line note shown next to a table when analytics (ClickHouse) is off:
 * states fall back to registry-only and Discovered rows are hidden. */
export function AnalyticsOffNote() {
  return (
    <p className="px-1 text-xs text-text-subtle" data-testid="analytics-off-note">
      Analytics are turned off. They require ClickHouse, so states show Registered, Disabled
      and Available only and Discovered rows are hidden.
    </p>
  )
}

export const VIA_GATEWAY_TITLE = "Calls reached this MCP through the gateway's MCP plane"

/** Secondary badge next to the state pill: how traffic reached the server.
 * Renders nothing without gateway traffic; "via gateway + direct" when both. */
export function ViaGatewayBadge({ viaGateway, direct }: { viaGateway: boolean; direct: boolean }) {
  if (!viaGateway) return null
  return (
    <span
      data-testid="via-gateway-badge"
      title={VIA_GATEWAY_TITLE}
      className="inline-flex h-5 w-fit items-center rounded-r-pill border border-border bg-card px-2 text-[11px] font-medium whitespace-nowrap text-text-muted"
    >
      {direct ? 'via gateway + direct' : 'via gateway'}
    </span>
  )
}
