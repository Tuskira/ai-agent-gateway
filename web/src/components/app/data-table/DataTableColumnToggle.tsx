import type { Table } from '@tanstack/react-table'
import { Settings2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { LABELS } from './labels'
import { isSystemColumn } from './types'

interface DataTableColumnToggleProps<TData> {
  table: Table<TData>
}

export function DataTableColumnToggle<TData>({
  table,
}: DataTableColumnToggleProps<TData>) {
  const columns = table
    .getAllColumns()
    .filter((col) => col.getCanHide() && !isSystemColumn(col.id))

  if (columns.length === 0) return null

  return (
    <Popover>
      <PopoverTrigger
        render={(props) => (
          <Button
            variant="ghost"
            size="icon-xs"
            {...props}
            aria-label={LABELS.toggleColumns}
          >
            <Settings2 className="size-4" aria-hidden="true" />
          </Button>
        )}
      />
      <PopoverContent align="end" className="w-48 gap-1 p-2">
        <p className="px-1 pb-1 text-xs font-medium text-muted-foreground">
          {LABELS.columns}
        </p>
        {columns.map((col) => (
          <label
            key={col.id}
            className="flex cursor-pointer items-center gap-2 rounded-sm px-1 py-1 text-sm hover:bg-muted"
          >
            <Checkbox
              checked={col.getIsVisible()}
              onCheckedChange={(value) => col.toggleVisibility(!!value)}
            />
            <span className="truncate">
              {typeof col.columnDef.header === 'string' ? col.columnDef.header : col.id}
            </span>
          </label>
        ))}
      </PopoverContent>
    </Popover>
  )
}
