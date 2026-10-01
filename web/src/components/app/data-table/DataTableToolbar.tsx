import type { ReactNode } from 'react'
import type { Table } from '@tanstack/react-table'
import { Separator } from '@/components/ui/separator'
import { DataTableColumnToggle } from './DataTableColumnToggle'
import { DataTableExport } from './DataTableExport'
import { DataTablePagination } from './DataTablePagination'
import { LABELS } from './labels'
import type { PaginationModel } from './types'

interface DataTableToolbarProps<TData> {
  table: Table<TData>
  totalRows: number
  pagination: PaginationModel
  pageSizeOptions: number[]
  onPaginationChange: (model: PaginationModel) => void
  showTotalCount: boolean
  toolbarControls?: ReactNode
  showExport: boolean
  exportAllRows: boolean
  exportFileName: string
  onExport?: () => void
  enableColumnVisibility: boolean
  paginationInToolbar: boolean
}

/** Inline alignSelf beats the primitive's self-stretch, keeping a fixed height. */
function Divider() {
  return (
    <Separator
      orientation="vertical"
      className="mx-1 h-5"
      style={{ alignSelf: 'center' }}
    />
  )
}

export function DataTableToolbar<TData>({
  table,
  totalRows,
  pagination,
  pageSizeOptions,
  onPaginationChange,
  showTotalCount,
  toolbarControls,
  showExport,
  exportAllRows,
  exportFileName,
  onExport,
  enableColumnVisibility,
  paginationInToolbar,
}: DataTableToolbarProps<TData>) {
  return (
    <div className="flex shrink-0 flex-wrap items-center gap-2 pl-1">
      {showTotalCount ? (
        <span className="text-sm font-medium whitespace-nowrap text-foreground tabular-nums">
          {totalRows.toLocaleString()} {LABELS.items}
        </span>
      ) : null}

      {toolbarControls ? (
        <>
          {showTotalCount ? <Divider /> : null}
          {toolbarControls}
        </>
      ) : null}

      {showExport ? (
        <>
          {showTotalCount || toolbarControls ? <Divider /> : null}
          <DataTableExport
            table={table}
            allRows={exportAllRows}
            fileName={exportFileName}
            onExport={onExport}
          />
        </>
      ) : null}

      <div className="flex-1" />

      {enableColumnVisibility ? <DataTableColumnToggle table={table} /> : null}

      {paginationInToolbar ? (
        <>
          {enableColumnVisibility ? <Divider /> : null}
          <DataTablePagination
            pagination={pagination}
            totalRows={totalRows}
            pageSizeOptions={pageSizeOptions}
            onPaginationChange={onPaginationChange}
          />
        </>
      ) : null}
    </div>
  )
}
