import { useMemo, useState } from 'react'
import { ChevronDown, ChevronLeft, ChevronRight } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  Command,
  CommandEmpty,
  CommandInput,
  CommandItem,
  CommandList,
} from '@/components/ui/command'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { cn } from '@/lib/utils'
import { LABELS } from './labels'
import type { PaginationModel } from './types'

/** Most entries the range-jump list will render at once. */
const MAX_RANGES = 100

interface DataTablePaginationProps {
  pagination: PaginationModel
  totalRows: number
  pageSizeOptions: number[]
  onPaginationChange: (model: PaginationModel) => void
}

export function DataTablePagination({
  pagination,
  totalRows,
  pageSizeOptions,
  onPaginationChange,
}: DataTablePaginationProps) {
  const { page, pageSize } = pagination
  const totalPages = Math.max(1, Math.ceil(totalRows / pageSize))
  const start = totalRows === 0 ? 0 : page * pageSize + 1
  const end = Math.min((page + 1) * pageSize, totalRows)
  const isFirst = page === 0
  const isLast = page >= totalPages - 1

  const [rangeOpen, setRangeOpen] = useState(false)
  const [search, setSearch] = useState('')

  // One entry per page, labelled by the 1-based row span it covers. Matching
  // is done here rather than by the list, and capped, so a table with
  // thousands of pages never renders thousands of entries.
  const ranges = useMemo(() => {
    const term = search.trim()
    const matches: { page: number; start: number; end: number }[] = []
    for (let i = 0; i < totalPages && matches.length < MAX_RANGES; i++) {
      const rangeStart = totalRows === 0 ? 0 : i * pageSize + 1
      const rangeEnd = Math.min((i + 1) * pageSize, totalRows)
      if (term === '' || `${i + 1} ${rangeStart} ${rangeEnd}`.includes(term)) {
        matches.push({ page: i, start: rangeStart, end: rangeEnd })
      }
    }
    return matches
  }, [search, totalPages, pageSize, totalRows])

  return (
    <div className="flex items-center gap-3 text-text-subtle">
      <div className="flex items-center gap-1.5">
        <span className="text-xs whitespace-nowrap">{LABELS.rowsPerPage}</span>
        <Select
          value={String(pageSize)}
          onValueChange={(value) =>
            onPaginationChange({ page: 0, pageSize: Number(value) })
          }
        >
          <SelectTrigger
            size="sm"
            className="h-7 w-16 text-xs text-foreground"
            aria-label={LABELS.rowsPerPage}
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {pageSizeOptions.map((size) => (
              <SelectItem key={size} value={String(size)}>
                {size}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      <div className="flex items-center gap-1 text-xs whitespace-nowrap tabular-nums">
        <Popover
          open={rangeOpen}
          onOpenChange={(open) => {
            setRangeOpen(open)
            if (!open) setSearch('')
          }}
        >
          <PopoverTrigger
            disabled={totalRows === 0}
            className={cn(
              'flex items-center gap-1 rounded-md px-1.5 py-0.5 transition-colors outline-none',
              'hover:bg-accent hover:text-foreground focus-visible:ring-3 focus-visible:ring-ring/50',
              'disabled:pointer-events-none disabled:opacity-50 data-popup-open:bg-accent data-popup-open:text-foreground',
            )}
          >
            {start.toLocaleString()}–{end.toLocaleString()}
            <ChevronDown className="size-3.5 opacity-60" aria-hidden="true" />
          </PopoverTrigger>
          <PopoverContent align="center" className="w-auto min-w-52 p-0">
            <Command shouldFilter={false} value={String(page)}>
              <CommandInput
                placeholder={LABELS.goToRange}
                value={search}
                onValueChange={setSearch}
              />
              <CommandList>
                <CommandEmpty>{LABELS.noRange}</CommandEmpty>
                {ranges.map((range) => (
                  <CommandItem
                    key={range.page}
                    value={String(range.page)}
                    onSelect={() => {
                      onPaginationChange({ page: range.page, pageSize })
                      setRangeOpen(false)
                      setSearch('')
                    }}
                    className={cn(
                      'gap-1.5 whitespace-nowrap tabular-nums',
                      range.page === page && 'font-medium',
                    )}
                  >
                    <span
                      className={cn(
                        range.page === page ? 'text-foreground' : 'text-muted-foreground',
                      )}
                    >
                      {LABELS.page(range.page + 1)}
                    </span>
                    <span className="text-muted-foreground/60">:</span>
                    <span>
                      {range.start.toLocaleString()}–{range.end.toLocaleString()}
                    </span>
                  </CommandItem>
                ))}
              </CommandList>
            </Command>
          </PopoverContent>
        </Popover>
        <span>
          {LABELS.of} {totalRows.toLocaleString()}
        </span>
      </div>

      <div className="flex items-center gap-0.5">
        <Button
          variant="ghost"
          size="icon-xs"
          disabled={isFirst}
          onClick={() => onPaginationChange({ page: page - 1, pageSize })}
          aria-label={LABELS.previousPage}
        >
          <ChevronLeft className="size-4" aria-hidden="true" />
        </Button>
        <Button
          variant="ghost"
          size="icon-xs"
          disabled={isLast || totalRows === 0}
          onClick={() => onPaginationChange({ page: page + 1, pageSize })}
          aria-label={LABELS.nextPage}
        >
          <ChevronRight className="size-4" aria-hidden="true" />
        </Button>
      </div>
    </div>
  )
}
