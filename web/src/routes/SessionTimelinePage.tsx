import { useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Clock, X } from 'lucide-react'
import { useAccessLogDetail, useConnectors, useLlmLogDetail, useSessionTimeline } from '@/lib/queries'
import { useAuth } from '@/auth/AuthContext'
import { AccessLogDetailDrawer } from '@/components/app/logs/AccessLogDetailDrawer'
import { LlmLogDetailDrawer } from '@/components/app/logs/LlmLogDetailDrawer'
import {
  readStoredTimelineOrder,
  storeTimelineOrder,
  type TimelineEvent,
  type TimelineOrder,
  type TimelineStatus,
} from '@/lib/session-timeline'
import { formatPercent } from '@/lib/overview'
import { formatReadableDuration } from '@/lib/duration'
import {
  browserTimeZone,
  formatCompactTime,
  formatExactTime,
  formatZoneHeader,
} from '@/lib/timeline-time'
import { cn, shortId } from '@/lib/utils'
import { StatCard } from '@/components/app/StatCard'
import { EmptyState } from '@/components/app/EmptyState'
import { CopyButton } from '@/components/app/CopyButton'
import { StatusPill, type PillTone } from '@/components/app/StatusPill'
import { DurationCell } from '@/components/app/logs/DurationCell'
import { KeyName } from '@/components/app/logs/KeyName'
import { Field } from '@/components/app/logs/Field'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'

type EventFilter = 'all' | TimelineEvent['plane']

const EVENT_FILTERS: { value: EventFilter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'llm', label: 'LLM' },
  { value: 'mcp', label: 'MCP' },
]

const ORDER_OPTIONS: { value: TimelineOrder; label: string }[] = [
  { value: 'asc', label: 'Oldest first' },
  { value: 'desc', label: 'Newest first' },
]

/** LLM orange / MCP cyan — the same pair the Overview traffic tiles use. */
const KIND_CLASSES: Record<TimelineEvent['plane'], { badge: string; dot: string }> = {
  llm: {
    badge: 'border-sev-medium/40 bg-sev-medium-bg text-sev-medium-fg',
    dot: 'bg-sev-medium',
  },
  mcp: {
    badge: 'border-brand-cyan-to/40 bg-brand-cyan-to/10 text-brand-cyan-to',
    dot: 'bg-brand-cyan-to',
  },
}

const statValueClass = 'text-[26px] leading-tight font-bold text-foreground'

function statusTone(status: TimelineStatus): PillTone {
  switch (status) {
    case 'error':
      return 'negative'
    case 'notification':
      return 'neutral'
    default:
      return 'positive'
  }
}

function statusLabel(status: TimelineStatus): string {
  return status.charAt(0).toUpperCase() + status.slice(1)
}

/** "YYYY-MM-DD" in the viewer's local timezone — matches what an
 * `<input type="date">` reads/writes, so a plain string comparison against
 * it is correct. */
function localDateOf(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const y = d.getFullYear()
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${y}-${m}-${day}`
}

export default function SessionTimelinePage() {
  const [searchParams, setSearchParams] = useSearchParams()
  const sessionId = searchParams.get('session_id') ?? ''
  const [draft, setDraft] = useState(sessionId)
  // Re-sync the input when the URL changes under it (back/forward).
  const [prevSessionId, setPrevSessionId] = useState(sessionId)
  if (sessionId !== prevSessionId) {
    setPrevSessionId(sessionId)
    setDraft(sessionId)
  }

  function load(next: string) {
    // Loading another session keeps the chosen order; clearing resets it.
    setSearchParams((prev) => {
      const params: Record<string, string> = next ? { session_id: next } : {}
      if (next && prev.get('order') === 'desc') params.order = 'desc'
      return params
    })
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
            Session Timeline
          </h1>
          <p className="text-[13px] text-text-subtle">
            Chronological view of LLM and MCP events for a session.
          </p>
        </div>
        {sessionId ? (
          <Button type="button" variant="outline" onClick={() => load('')}>
            <X className="size-3.5" aria-hidden="true" />
            Clear session
          </Button>
        ) : null}
      </div>

      <form
        className="flex flex-col gap-2 rounded-r-5 border border-border bg-card p-4"
        onSubmit={(e) => {
          e.preventDefault()
          load(draft.trim())
        }}
      >
        <div className="flex gap-2">
          <Input
            placeholder="Session ID"
            aria-label="Session ID"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            className="h-10 flex-1 font-mono"
          />
          <Button type="submit" className="h-10" disabled={!draft.trim()}>
            Load Timeline
          </Button>
        </div>
        <p className="text-xs text-text-subtle">
          Paste a session ID from Access Logs or LLM Logs, or the X-Session-Id header your
          agent sends.
        </p>
      </form>

      {sessionId ? (
        <Timeline key={sessionId} sessionId={sessionId} />
      ) : (
        <div className="flex flex-col items-center gap-2 rounded-r-5 border border-dashed border-border bg-card px-4 py-14 text-center">
          <Clock className="size-8 text-text-subtle" aria-hidden="true" />
          <p className="text-sm font-semibold text-foreground">No session loaded</p>
          <p className="text-[13px] text-text-muted">
            Enter a session ID to replay every LLM inference and MCP call in order.
          </p>
        </div>
      )}
    </div>
  )
}

function Timeline({ sessionId }: { sessionId: string }) {
  const [filter, setFilter] = useState<EventFilter>('all')
  const [selected, setSelected] = useState<TimelineEvent | null>(null)
  const [startDate, setStartDate] = useState('')
  const [endDate, setEndDate] = useState('')

  // Order: the URL wins (?order=desc; absent = asc), else the browser's last
  // choice. The server sorts; this page renders the list exactly as given.
  const [searchParams, setSearchParams] = useSearchParams()
  const urlOrder = searchParams.get('order')
  const [storedOrder] = useState(readStoredTimelineOrder)
  const order: TimelineOrder =
    urlOrder === 'desc' ? 'desc' : urlOrder === 'asc' ? 'asc' : storedOrder
  // Keep the URL truthful when the order came from storage.
  useEffect(() => {
    if (urlOrder === null && storedOrder === 'desc') {
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev)
          next.set('order', 'desc')
          return next
        },
        { replace: true },
      )
    }
  }, [urlOrder, storedOrder, setSearchParams])

  function chooseOrder(next: TimelineOrder) {
    storeTimelineOrder(next)
    setSearchParams((prev) => {
      const params = new URLSearchParams(prev)
      if (next === 'desc') params.set('order', 'desc')
      else params.delete('order')
      return params
    })
  }

  const query = useSessionTimeline(sessionId, order)
  // One zone for the whole page: the browser's, read once per mount.
  const [timeZone] = useState(browserTimeZone)

  // Rendered in the server's order; never re-sorted here.
  const events = useMemo(() => query.data?.events ?? [], [query.data])

  // The header's offset follows the most recent event (now when empty),
  // whichever end of the list it is at.
  const latestInstant = useMemo(() => {
    const latestMs = events.reduce((max, e) => Math.max(max, Date.parse(e.ts) || 0), 0)
    return latestMs ? new Date(latestMs) : new Date()
  }, [events])

  if (query.isLoading) {
    return (
      <div className="flex flex-col gap-2">
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-12 w-full" />
        <Skeleton className="h-12 w-full" />
      </div>
    )
  }
  if (query.isError) {
    return <p className="text-sm text-sev-high">Couldn&apos;t load this session.</p>
  }
  const timeline = query.data
  if (!timeline) {
    return (
      <div className="flex justify-center rounded-r-5 border border-dashed border-border bg-card py-12">
        <EmptyState label="Analytics requires the ClickHouse sink." />
      </div>
    )
  }

  // Stat tiles always summarize the whole session, same as the Event Type
  // filter below never changing them — only the rendered list narrows.
  const llmCount = events.filter((e) => e.plane === 'llm').length
  const totalMs = events.reduce((sum, e) => sum + e.duration_ms, 0)
  const errors = events.filter((e) => e.status === 'error').length

  const dateFiltered =
    startDate || endDate
      ? events.filter((e) => {
          const day = localDateOf(e.ts)
          if (startDate && day < startDate) return false
          if (endDate && day > endDate) return false
          return true
        })
      : events
  const visible = filter === 'all' ? dateFiltered : dateFiltered.filter((e) => e.plane === filter)

  return (
    <>
      {timeline.excluded_foreign_events > 0 ? (
        <p role="status" className="text-xs text-text-subtle">
          {timeline.excluded_foreign_events} event
          {timeline.excluded_foreign_events === 1 ? '' : 's'} from another key matched this
          session's tag and {timeline.excluded_foreign_events === 1 ? 'was' : 'were'} withheld.
        </p>
      ) : null}
      <div className="grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-4">
        <StatCard dotClassName="bg-primary" title="Total events">
          <span className={statValueClass}>{events.length}</span>
          <span className="text-xs text-text-subtle">
            {llmCount} LLM · {events.length - llmCount} MCP
          </span>
        </StatCard>
        <StatCard dotClassName="bg-brand-cyan-to" title="Total duration">
          <span className={statValueClass}>{formatReadableDuration(totalMs)}</span>
          <span className="text-xs text-text-subtle">
            avg {formatReadableDuration(events.length ? totalMs / events.length : 0)}
          </span>
        </StatCard>
        <StatCard dotClassName="bg-sev-high" title="Errors">
          <span className={statValueClass}>{errors}</span>
          <span className="text-xs text-text-subtle">
            {formatPercent(events.length ? (errors / events.length) * 100 : 0)} error rate
          </span>
        </StatCard>
        <StatCard dotClassName="bg-status-open" title="Session ID">
          <div className="flex min-w-0 items-center gap-1.5">
            <span
              className="truncate font-mono text-[13.5px] font-medium text-foreground"
              title={sessionId}
            >
              {shortId(sessionId, 12, 6)}
            </span>
            <CopyButton value={sessionId} label="Copy session ID" size="icon-xs" />
          </div>
        </StatCard>
      </div>

      <div className="rounded-r-5 border border-border bg-card p-4">
        <div className="mb-4 flex flex-wrap items-end gap-4">
          <div className="flex flex-col gap-1.5">
            <span className="text-xs font-semibold text-text-subtle uppercase">Event type</span>
            <div className="flex items-center overflow-hidden rounded-r-3 border border-border">
              {EVENT_FILTERS.map((f) => (
                <button
                  key={f.value}
                  type="button"
                  aria-pressed={filter === f.value}
                  onClick={() => setFilter(f.value)}
                  className={cn(
                    'h-8 border-r border-border px-3 text-[12.5px] font-medium text-text-muted last:border-r-0 hover:bg-bg-muted',
                    filter === f.value &&
                      'bg-primary text-primary-foreground hover:bg-primary/80',
                  )}
                >
                  {f.label}
                </button>
              ))}
            </div>
          </div>
          <div className="flex flex-col gap-1.5">
            <span className="text-xs font-semibold text-text-subtle uppercase">Order</span>
            <div
              role="group"
              aria-label="Order"
              className="flex items-center overflow-hidden rounded-r-3 border border-border"
            >
              {ORDER_OPTIONS.map((o) => (
                <button
                  key={o.value}
                  type="button"
                  aria-pressed={order === o.value}
                  onClick={() => chooseOrder(o.value)}
                  className={cn(
                    'h-8 border-r border-border px-3 text-[12.5px] font-medium text-text-muted last:border-r-0 hover:bg-bg-muted',
                    order === o.value &&
                      'bg-primary text-primary-foreground hover:bg-primary/80',
                  )}
                >
                  {o.label}
                </button>
              ))}
            </div>
          </div>
          <label
            htmlFor="timeline-start-date"
            className="flex flex-col gap-1.5 text-xs font-semibold text-text-subtle uppercase"
          >
            Start Date
            <Input
              id="timeline-start-date"
              type="date"
              value={startDate}
              max={endDate || undefined}
              onChange={(e) => setStartDate(e.target.value)}
              className="h-8 w-[150px] normal-case"
            />
          </label>
          <label
            htmlFor="timeline-end-date"
            className="flex flex-col gap-1.5 text-xs font-semibold text-text-subtle uppercase"
          >
            End Date
            <Input
              id="timeline-end-date"
              type="date"
              value={endDate}
              min={startDate || undefined}
              onChange={(e) => setEndDate(e.target.value)}
              className="h-8 w-[150px] normal-case"
            />
          </label>
          <p className="ml-auto text-xs text-text-subtle">
            {formatZoneHeader({ timeZone, now: latestInstant })}
          </p>
          {startDate || endDate ? (
            <Button
              type="button"
              variant="outline"
              className="h-8"
              onClick={() => {
                setStartDate('')
                setEndDate('')
              }}
            >
              Clear dates
            </Button>
          ) : null}
        </div>

        {visible.length === 0 ? (
          <div className="flex justify-center py-8">
            <EmptyState label="No events for this session." />
          </div>
        ) : (
          <ol className="ml-2 border-l border-border">
            {visible.map((e) => (
              <li key={`${e.plane}-${e.id}`} className="relative pl-5">
                <span
                  className={cn(
                    'absolute top-1/2 -left-[5px] size-2.5 -translate-y-1/2 rounded-full ring-2 ring-card',
                    KIND_CLASSES[e.plane].dot,
                  )}
                  aria-hidden="true"
                />
                <button
                  type="button"
                  onClick={() => setSelected(e)}
                  className="flex w-full flex-wrap items-center gap-3 rounded-r-4 px-3 py-2.5 text-left hover:bg-bg-muted"
                >
                  <span
                    className={cn(
                      'inline-flex w-11 justify-center rounded-r-3 border py-0.5 text-[11px] font-bold',
                      KIND_CLASSES[e.plane].badge,
                    )}
                  >
                    {e.plane.toUpperCase()}
                  </span>
                  <StatusPill tone={statusTone(e.status)} label={statusLabel(e.status)} />
                  <span className="min-w-0 flex-1 truncate font-mono text-xs font-medium text-foreground">
                    {e.name || e.kind}
                  </span>
                  <TimeCell iso={e.ts} timeZone={timeZone} />
                  <span className="flex w-28 shrink-0 justify-end text-right text-xs tabular-nums">
                    <DurationCell ms={e.duration_ms} readable />
                  </span>
                  <span className="font-mono text-xs text-text-subtle">{shortId(e.id)}</span>
                </button>
              </li>
            ))}
          </ol>
        )}
      </div>

      <EventDetail event={selected} timeZone={timeZone} onOpenChange={(open) => !open && setSelected(null)} />
    </>
  )
}

/** The row's timestamp: compact local time in a fixed-width, right-aligned
 * column, with the exact instant (milliseconds, zone, UTC) on hover. */
function TimeCell({ iso, timeZone }: { iso: string; timeZone: string }) {
  const compact = formatCompactTime(iso, { timeZone })
  const exact = formatExactTime(iso, { timeZone })
  const cellClass =
    'w-36 shrink-0 text-right text-xs whitespace-nowrap text-text-muted tabular-nums'
  if (!compact || !exact) return <span className={cellClass}>—</span>
  return (
    <TooltipProvider>
      <Tooltip>
        <TooltipTrigger render={<span className={cn(cellClass, 'cursor-default')} />}>
          {compact}
        </TooltipTrigger>
        <TooltipContent className="tabular-nums">{exact}</TooltipContent>
      </Tooltip>
    </TooltipProvider>
  )
}

/** A timeline row's detail: the full LLM or MCP log drawer (the one the log
 * pages open: tokens, headers, conversation, bodies), loaded by request id
 * from the admin-only detail routes. Falls back to the timeline's own summary
 * for a non-admin, or when the row cannot be read. */
function EventDetail({
  event,
  timeZone,
  onOpenChange,
}: {
  event: TimelineEvent | null
  timeZone: string
  onOpenChange: (open: boolean) => void
}) {
  const { principal } = useAuth()
  const isAdmin = !!principal?.roles.includes('admin')
  const llm = useLlmLogDetail(event?.plane === 'llm' ? event.id : undefined, isAdmin)
  const mcp = useAccessLogDetail(event?.plane === 'mcp' ? event.id : undefined, isAdmin)
  const connectors = useConnectors()
  const detail = event?.plane === 'llm' ? llm : mcp

  if (event && isAdmin && detail.isLoading) return null
  if (event?.plane === 'llm' && llm.data) {
    return <LlmLogDetailDrawer item={llm.data} onOpenChange={onOpenChange} readableDuration />
  }
  if (event?.plane === 'mcp' && mcp.data) {
    const name = connectors.data?.items.find((c) => c.id === mcp.data.connector_id)?.name
    return <AccessLogDetailDrawer
        item={mcp.data}
        connectorName={name}
        onOpenChange={onOpenChange}
        readableDuration
      />
  }
  return <EventDetailSheet event={event} timeZone={timeZone} onOpenChange={onOpenChange} />
}

/** A timeline row's detail: everything the merged timeline endpoint
 * carries for one event. There is no separate fetch here — the endpoint's
 * per-event shape is already this compact, by design (see
 * pkg/analytics.TimelineEvent): showing more (full request/response
 * bodies) would mean re-fetching from the plane-specific admin-only detail
 * routes, which is out of scope for this view. */
function EventDetailSheet({
  event,
  timeZone,
  onOpenChange,
}: {
  event: TimelineEvent | null
  timeZone: string
  onOpenChange: (open: boolean) => void
}) {
  const open = event !== null
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="w-full sm:max-w-md">
        <SheetHeader>
          <SheetTitle className="font-mono">
            {event ? `${event.plane.toUpperCase()} · ${event.kind}` : ''}
          </SheetTitle>
          <SheetDescription className="flex items-center gap-1.5 font-mono text-xs break-all">
            {event?.id}
            {event ? <CopyButton value={event.id} label="Copy request ID" size="icon-xs" /> : null}
          </SheetDescription>
        </SheetHeader>
        {event ? (
          <div className="grid grid-cols-2 gap-3 px-4 pb-4 text-sm">
            <Field label="Status">
              <StatusPill tone={statusTone(event.status)} label={statusLabel(event.status)} />
            </Field>
            <Field label="Duration">
              <DurationCell ms={event.duration_ms} readable />
            </Field>
            <Field label="Name" wide>
              <span className="font-mono text-xs break-all">{event.name || '—'}</span>
            </Field>
            <Field label="Timestamp" wide>
              {formatExactTime(event.ts, { timeZone }) ?? '—'}
            </Field>
            <Field label="API key" wide>
              <KeyName keyId={event.key_id} />
            </Field>
          </div>
        ) : null}
      </SheetContent>
    </Sheet>
  )
}
