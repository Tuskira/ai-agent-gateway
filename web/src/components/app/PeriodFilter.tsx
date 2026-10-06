import { useState } from 'react'
import { format, parseISO, subMonths } from 'date-fns'
import { CalendarDays } from 'lucide-react'
import type { DateRange } from 'react-day-picker'
import { SegmentedControl } from '@/components/app/SegmentedControl'
import { Button } from '@/components/ui/button'
import { Calendar } from '@/components/ui/calendar'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { periodLabel, TIME_RANGES, type Period } from '@/lib/overview'

const day = (d: Date) => format(d, 'yyyy-MM-dd')

/** The dashboards' time filter: the presets, and a two-month calendar for
 * custom dates (whole UTC days, at most 366, none after today in UTC). */
export function PeriodFilter({
  value,
  onChange,
}: {
  value: Period
  onChange: (p: Period) => void
}) {
  const [open, setOpen] = useState(false)
  const [draft, setDraft] = useState<DateRange | undefined>()
  const custom = value.range === 'custom'
  const now = new Date()
  const today = new Date(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate())

  const toggle = (next: boolean) => {
    if (next) {
      setDraft(
        value.range === 'custom'
          ? { from: parseISO(value.from), to: parseISO(value.to) }
          : undefined,
      )
    }
    setOpen(next)
  }
  const picked: Period | null = draft?.from
    ? { range: 'custom', from: day(draft.from), to: day(draft.to ?? draft.from) }
    : null
  const apply = () => {
    if (!picked) return
    onChange(picked)
    setOpen(false)
  }

  return (
    // One button group: the presets, then Custom.
    <div className="flex items-center rounded-r-4 border border-border bg-card p-0.5">
      <SegmentedControl
        label="Time range"
        options={TIME_RANGES}
        value={custom ? null : value.range}
        onChange={(range) => onChange({ range })}
        className="border-0 bg-transparent p-0"
      />
      <Popover open={open} onOpenChange={toggle}>
        <PopoverTrigger
          render={
            <button
              type="button"
              aria-pressed={custom}
              aria-label={custom ? `Custom dates: ${periodLabel(value)}` : 'Custom dates'}
              className="inline-flex h-8 cursor-pointer items-center gap-1.5 rounded-r-3 px-3 text-[13px] text-text-muted transition-colors hover:text-foreground aria-pressed:bg-primary aria-pressed:text-primary-foreground"
            />
          }
        >
          <CalendarDays className="size-3.5" aria-hidden="true" />
          {custom ? periodLabel(value) : 'Custom'}
        </PopoverTrigger>
        <PopoverContent align="end" className="w-auto gap-0 p-0">
          <Calendar
            mode="range"
            numberOfMonths={2}
            selected={draft}
            onSelect={setDraft}
            defaultMonth={draft?.from ?? subMonths(today, 1)}
            disabled={{ after: today }}
            max={365}
            resetOnSelect
          />
          <div className="flex items-center justify-between gap-3 border-t border-border p-2">
            <span className="text-[12px] text-text-muted">
              {picked ? periodLabel(picked) : 'Pick a start and end date'} · UTC
            </span>
            <Button size="sm" disabled={!picked} onClick={apply}>
              Apply
            </Button>
          </div>
        </PopoverContent>
      </Popover>
    </div>
  )
}
