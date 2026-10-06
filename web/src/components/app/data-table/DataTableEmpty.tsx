import type { ReactNode } from 'react'
import { Inbox } from 'lucide-react'
import { TableBody, TableCell, TableRow } from '@/components/ui/table'
import { cn } from '@/lib/utils'

interface DataTableEmptyProps {
  columnCount: number
  title: string
  description?: string
  action?: ReactNode
  /** Stretch to the remaining table height so the message sits centred. */
  fill?: boolean
}

export function DataTableEmpty({
  columnCount,
  title,
  description,
  action,
  fill = false,
}: DataTableEmptyProps) {
  return (
    <TableBody className={cn(fill && 'h-full')}>
      <TableRow className="hover:bg-transparent">
        <TableCell
          colSpan={columnCount}
          className={cn('p-0 whitespace-normal', fill ? 'h-full' : 'h-48')}
        >
          {/* Centred in the visible part of a table wider than its region,
              not across the full scrollable width. */}
          <div className="sticky left-0 flex w-[100cqw] flex-col items-center justify-center gap-1.5 px-2 text-center">
            <Inbox className="size-5 text-text-subtle" aria-hidden="true" />
            <span className="text-sm font-medium text-foreground">{title}</span>
            {description ? (
              <span className="max-w-[360px] text-xs text-text-subtle">
                {description}
              </span>
            ) : null}
            {action ? <div className="mt-1.5">{action}</div> : null}
          </div>
        </TableCell>
      </TableRow>
    </TableBody>
  )
}
