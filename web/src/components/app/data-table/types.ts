import type { ReactNode } from 'react'
import type {
  ColumnDef,
  RowData,
  RowSelectionState,
  VisibilityState,
} from '@tanstack/react-table'

declare module '@tanstack/react-table' {
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  interface ColumnMeta<TData extends RowData, TValue> {
    /**
     * Tooltip on cell hover.
     * - `true` shows the cell's own text value
     * - a string is shown as-is
     * - a function derives the text from the row
     */
    tooltip?: boolean | string | ((row: TData) => string)
    /** Rich hover card for the cell. When set, `tooltip` is ignored. */
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    hoverCard?: (row: any) => ReactNode
    /**
     * Quick actions revealed on cell hover, as icon buttons pinned to the
     * cell's trailing edge. When set, the cell skips the default tooltip.
     */
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    cellActions?: CellAction<any>[] | ((row: any) => CellAction<any>[])
  }
}

/** Column ids the table injects itself. Never sortable, resizable or exported. */
export const SYSTEM_COLUMN_IDS = ['__select', '__expand', '__actions'] as const

export function isSystemColumn(id: string): boolean {
  return (SYSTEM_COLUMN_IDS as readonly string[]).includes(id)
}

/** A single hover-revealed cell action (see `ColumnMeta.cellActions`). */
export interface CellAction<TData> {
  /** Accessible name and tooltip for the icon button. */
  label: string
  icon: ReactNode
  onClick: (row: TData) => void
  hidden?: (row: TData) => boolean
}

export interface PaginationModel {
  /** Zero-based page index. */
  page: number
  pageSize: number
}

export interface SortModel {
  /** Column id. */
  field: string
  sort: 'asc' | 'desc'
}

export interface RowAction<TData> {
  label: string
  icon?: ReactNode
  onClick: (row: TData) => void
  variant?: 'default' | 'destructive'
  /** Hide this action for a given row. */
  hidden?: (row: TData) => boolean
  /** Disable this action for a given row. */
  disabled?: (row: TData) => boolean
  /** Render a divider above this action. Ignored on the first visible item. */
  separatorBefore?: boolean
}

export interface BulkAction<TData> {
  label: string
  icon?: ReactNode
  onClick: (selectedRows: TData[]) => void
  variant?: 'default' | 'outline' | 'destructive'
  /** Minimum selected rows required to show this action. @default 1 */
  minSelected?: number
}

/** Rows-per-page choices every table offers; the first is the default. */
export const PAGE_SIZES = [25, 50, 100]

interface DataTableBaseProps<TData> {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  columns: ColumnDef<TData, any>[]
  data: TData[]

  /**
   * Unique per table. Column visibility, widths and sorting are saved to
   * `localStorage['gateway.table.<storageKey>']`.
   */
  storageKey: string
  /** Bump to discard saved state after the column set changes. @default 1 */
  storageVersion?: number

  /** Derive a stable id from each row. Defaults to the row index. */
  getRowId?: (row: TData) => string
  /** Name a row for assistive tech, e.g. "Actions for <label>". */
  getRowLabel?: (row: TData) => string
  /** Extra class for a row, e.g. to dim a revoked entry. */
  getRowClassName?: (row: TData) => string | undefined

  loading?: boolean
  /** `true` shows a generic message; a string is shown as the message. */
  error?: boolean | string
  /** @default 'No results' */
  emptyTitle?: string
  emptyDescription?: string
  emptyAction?: ReactNode
  /** Skeleton rows shown on first load. @default 5 */
  skeletonRows?: number

  /** @default PAGE_SIZES */
  pageSizeOptions?: number[]
  /** Initial sort. Overrides any sort saved under `storageKey`. */
  defaultSorting?: SortModel[]

  /** @default false */
  enableRowSelection?: boolean
  /** @default true */
  enableMultiRowSelection?: boolean
  rowSelection?: RowSelectionState
  onRowSelectionChange?: (selection: RowSelectionState, selectedRows: TData[]) => void

  /** @default true */
  enableColumnVisibility?: boolean
  defaultColumnVisibility?: VisibilityState
  /** @default true */
  enableColumnResize?: boolean
  /** Drag-to-reorder columns. The order is not persisted. @default false */
  enableColumnReorder?: boolean

  rowActions?: RowAction<TData>[]
  /** Right-click a row for the same `rowActions`. @default true */
  enableRowContextMenu?: boolean
  bulkActions?: BulkAction<TData>[]

  renderDetailPanel?: (row: TData) => ReactNode
  /** Allow only one expanded row at a time. @default true */
  singleExpand?: boolean
  /** Toggle the detail panel when the row body is clicked. @default false */
  expandOnRowClick?: boolean
  onRowClick?: (row: TData) => void

  /** @default true */
  showToolbar?: boolean
  /** @default true */
  showTotalCount?: boolean
  toolbarControls?: ReactNode
  /** @default false */
  showExport?: boolean
  /** When set, the export button calls this instead of writing a CSV. */
  onExport?: () => void
  /** `false` moves pagination to a footer. @default true */
  paginationInToolbar?: boolean

  /** Tooltip on text cells of 20+ characters. @default true */
  enableTooltip?: boolean

  /** Row height in pixels. @default 48 */
  rowHeight?: number
  /** Max height of the scrolling body, e.g. "400px" or "60vh". */
  height?: string
  /** Stretch to the parent's height. Needs a bounded ancestor. @default false */
  fillHeight?: boolean
  className?: string
}

/** All rows are in `data`; the table sorts and pages them itself. */
interface ClientModeProps {
  pagination?: undefined
  onPaginationChange?: undefined
  totalRows?: undefined
  sorting?: undefined
  onSortingChange?: undefined
  /** @default first entry of `pageSizeOptions` */
  defaultPageSize?: number
}

/** `data` is one page; the caller owns pagination and (optionally) sorting. */
interface ServerModeProps {
  pagination: PaginationModel
  onPaginationChange: (model: PaginationModel) => void
  totalRows: number
  sorting?: SortModel[]
  /** Sorting is enabled only when this is provided. */
  onSortingChange?: (sorting: SortModel[]) => void
  defaultPageSize?: undefined
}

export type DataTableProps<TData> = DataTableBaseProps<TData> &
  (ClientModeProps | ServerModeProps)
