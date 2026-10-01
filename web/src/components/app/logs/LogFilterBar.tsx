import { RefreshCw, Search } from 'lucide-react'
import { QUICK_RANGES, type QuickRange } from '@/lib/timeRange'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

export interface LogFilterFieldDef<K extends string> {
  key: K
  label: string
  /** Renders a `<select>` with these options instead of a free-text input —
   * used for fields with a fixed value set (e.g. JSON-RPC method). */
  options?: { value: string; label: string }[]
}

interface LogFilterBarProps<K extends string> {
  fields: LogFilterFieldDef<K>[]
  values: Record<K, string>
  onFieldChange: (key: K, value: string) => void
  onApply: () => void
  onRefresh: () => void
  isFetching: boolean
  activeRange: QuickRange | null
  onRangeSelect: (range: QuickRange) => void
  /** Column count at the `xl` breakpoint — 7 for Access Logs, 6 for LLM
   * Logs, matching each page's field count so the grid fills one row. */
  xlColumns: 4 | 5 | 6 | 7
}

const XL_COLS_CLASS: Record<4 | 5 | 6 | 7, string> = {
  4: 'xl:grid-cols-4',
  5: 'xl:grid-cols-5',
  6: 'xl:grid-cols-6',
  7: 'xl:grid-cols-7',
}

const selectClassName =
  'h-10 w-full min-w-0 rounded-lg border border-input bg-transparent px-2.5 font-mono text-sm text-foreground outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50'

/** Filter bar shared by the Access Logs and LLM Logs pages: one input per
 * field (in a responsive grid that fills a single row at `xl`), Apply/
 * Refresh actions, time-range quick picks, and the slow-duration legend —
 * matches the mock's filter card. Typing debounces to the URL on its own
 * (see `useDebouncedValue` in the pages); "Apply Filters" commits
 * immediately. */
export function LogFilterBar<K extends string>({
  fields,
  values,
  onFieldChange,
  onApply,
  onRefresh,
  isFetching,
  activeRange,
  onRangeSelect,
  xlColumns,
}: LogFilterBarProps<K>) {
  return (
    <div className="flex flex-col gap-[var(--space-4)] rounded-r-5 border border-border bg-card p-4">
      <div
        className={cn(
          'grid grid-cols-1 gap-[var(--space-4)] sm:grid-cols-2 lg:grid-cols-3',
          XL_COLS_CLASS[xlColumns],
        )}
      >
        {fields.map((field) =>
          field.options ? (
            <select
              key={field.key}
              value={values[field.key] ?? ''}
              onChange={(e) => onFieldChange(field.key, e.target.value)}
              aria-label={field.label}
              className={selectClassName}
            >
              {field.options.map((opt) => (
                <option key={opt.value} value={opt.value} className="font-mono">
                  {opt.label}
                </option>
              ))}
            </select>
          ) : (
            <Input
              key={field.key}
              placeholder={field.label}
              value={values[field.key] ?? ''}
              onChange={(e) => onFieldChange(field.key, e.target.value)}
              className="h-10"
              aria-label={field.label}
            />
          ),
        )}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Button onClick={onApply}>
          <Search className="size-3.5" aria-hidden="true" />
          Apply Filters
        </Button>
        <Button variant="outline" onClick={onRefresh} disabled={isFetching}>
          <RefreshCw
            className={cn('size-3.5', isFetching && 'animate-spin')}
            aria-hidden="true"
          />
          {isFetching ? 'Refreshing…' : 'Refresh'}
        </Button>

        <div className="ml-1 flex items-center overflow-hidden rounded-r-3 border border-border">
          {QUICK_RANGES.map((r) => (
            <button
              key={r.key}
              type="button"
              onClick={() => onRangeSelect(r.key)}
              className={cn(
                'h-9 border-r border-border px-3 text-[12.5px] font-medium text-text-muted last:border-r-0 hover:bg-bg-muted',
                activeRange === r.key &&
                  'bg-primary text-primary-foreground hover:bg-primary/80',
              )}
            >
              {r.label}
            </button>
          ))}
        </div>

        <span className="ml-auto flex items-center gap-1.5 text-xs text-text-subtle">
          <span className="size-2.5 rounded-[3px] bg-sev-medium-bg" aria-hidden="true" />
          Duration over 5000ms — slow, even when status is 200
        </span>
      </div>
    </div>
  )
}
