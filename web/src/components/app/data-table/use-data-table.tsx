import { useEffect, useMemo, useRef, useState } from 'react'
import {
  getCoreRowModel,
  getExpandedRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type ExpandedState,
  type RowSelectionState,
  type SortingState,
  type Updater,
} from '@tanstack/react-table'
import { ChevronRight } from 'lucide-react'
import { Checkbox } from '@/components/ui/checkbox'
import { cn } from '@/lib/utils'
import { LABELS } from './labels'
import {
  PAGE_SIZES,
  type DataTableProps,
  type PaginationModel,
  type SortModel,
} from './types'
import { useTableStorage } from './use-table-storage'

/** Width, in pixels, of a column that declares no `size`. */
const DEFAULT_COLUMN_SIZE = 120
/** Narrowest a column can be dragged. */
const MIN_COLUMN_SIZE = 48

function toTanstackSorting(sorting: SortModel[]): SortingState {
  return sorting.map((s) => ({ id: s.field, desc: s.sort === 'desc' }))
}

function fromTanstackSorting(sorting: SortingState): SortModel[] {
  return sorting.map((s) => ({ field: s.id, sort: s.desc ? 'desc' : 'asc' }))
}

function resolve<T>(updater: Updater<T>, previous: T): T {
  return typeof updater === 'function' ? (updater as (old: T) => T)(previous) : updater
}

/** The row objects behind a selection state, in selection order. */
function rowsForSelection<TData>(
  selection: RowSelectionState,
  data: TData[],
  getRowId: ((row: TData) => string) | undefined,
): TData[] {
  const ids = Object.keys(selection).filter((id) => selection[id])
  const byId = new Map(
    data.map((row, index) => [getRowId ? getRowId(row) : String(index), row]),
  )
  return ids.flatMap((id) => {
    const row = byId.get(id)
    return row === undefined ? [] : [row]
  })
}

export function useDataTable<TData>(props: DataTableProps<TData>) {
  const {
    columns,
    data,
    storageKey,
    storageVersion = 1,
    getRowId,
    pageSizeOptions = PAGE_SIZES,
    defaultPageSize,
    defaultSorting,
    enableRowSelection = false,
    enableMultiRowSelection = true,
    rowSelection,
    onRowSelectionChange,
    defaultColumnVisibility,
    enableColumnResize = true,
    renderDetailPanel,
    singleExpand = true,
    rowActions,
    onPaginationChange,
    onSortingChange,
  } = props

  // Server mode is chosen by passing `pagination`; see `DataTableProps`.
  const isServer = props.pagination !== undefined

  const [persisted, setPersisted] = useTableStorage({
    storageKey,
    defaultVisibility: defaultColumnVisibility,
    defaultSorting,
    version: storageVersion,
  })

  // -- Pagination -------------------------------------------------------------

  const [clientPagination, setClientPagination] = useState<PaginationModel>({
    page: 0,
    pageSize: defaultPageSize ?? pageSizeOptions[0] ?? 25,
  })
  const totalRows = isServer ? (props.totalRows ?? 0) : data.length
  const requested = props.pagination ?? clientPagination
  // A page size of zero or less would divide by zero below.
  const pageSize =
    requested.pageSize > 0 ? requested.pageSize : (pageSizeOptions[0] ?? 25)
  const lastPage = Math.max(0, Math.ceil(totalRows / pageSize) - 1)
  // A shorter row set can leave the client's page past the end. Clamp during
  // render instead of stranding the viewer on an empty page.
  const pagination: PaginationModel = {
    page: isServer ? Math.max(0, requested.page) : Math.min(requested.page, lastPage),
    pageSize,
  }

  const setPagination = (model: PaginationModel) => {
    if (isServer) onPaginationChange?.(model)
    else setClientPagination(model)
  }

  // -- Sorting ----------------------------------------------------------------

  const sortingEnabled = isServer ? onSortingChange !== undefined : true
  const sorting: SortModel[] = isServer ? (props.sorting ?? []) : persisted.sorting

  // Server mode: the caller owns `sorting`, so seed it once from the saved (or
  // default) sort. Later changes flow through `onSortingChange` as usual.
  const hydratedRef = useRef(false)
  useEffect(() => {
    if (hydratedRef.current) return
    hydratedRef.current = true
    if (!isServer || !onSortingChange || persisted.sorting.length === 0) return
    if (JSON.stringify(persisted.sorting) === JSON.stringify(props.sorting ?? [])) return
    onSortingChange(persisted.sorting)
    // Mount only.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // -- Selection and expansion ------------------------------------------------

  const [internalSelection, setInternalSelection] = useState<RowSelectionState>({})
  const selection = rowSelection ?? internalSelection
  const selectedRows = rowsForSelection(selection, data, getRowId)

  const [expanded, setExpanded] = useState<ExpandedState>({})

  // Session-only: a saved order could outlive a change to the column set.
  const [columnOrder, setColumnOrder] = useState<string[]>([])

  // -- System columns injected around the caller's columns ----------------------

  const hasDetailPanel = renderDetailPanel !== undefined
  const hasRowActions = (rowActions?.length ?? 0) > 0

  // `rows` is a fresh copy of `data`, made whenever the columns change.
  // TanStack caches each row's accessor values until `data` changes identity.
  // An accessor often reads a lookup that loads after the rows, such as a
  // per-row count from a second request, and the cached value would then be
  // stale: sorting and tooltips would use it while the cell showed the fresh
  // one. Rebuilding the rows with the columns keeps the two in step.
  const { allColumns, rows } = useMemo(() => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const cols: ColumnDef<TData, any>[] = []

    if (enableRowSelection) {
      cols.push({
        id: '__select',
        header: ({ table }) =>
          enableMultiRowSelection ? (
            <Checkbox
              checked={table.getIsAllPageRowsSelected()}
              indeterminate={table.getIsSomePageRowsSelected()}
              onCheckedChange={(value) => table.toggleAllPageRowsSelected(!!value)}
              aria-label={LABELS.selectAll}
            />
          ) : null,
        cell: ({ row }) => (
          <Checkbox
            checked={row.getIsSelected()}
            disabled={!row.getCanSelect()}
            onCheckedChange={(value) => row.toggleSelected(!!value)}
            aria-label={LABELS.selectRow}
          />
        ),
        enableSorting: false,
        enableResizing: false,
        enableHiding: false,
        size: 40,
        minSize: 40,
      })
    }

    if (hasDetailPanel) {
      cols.push({
        id: '__expand',
        header: () => null,
        cell: ({ row }) => (
          <button
            type="button"
            aria-label={LABELS.toggleDetails}
            aria-expanded={row.getIsExpanded()}
            className="flex size-6 cursor-pointer items-center justify-center rounded-md text-muted-foreground outline-none hover:text-foreground focus-visible:ring-3 focus-visible:ring-ring/50"
            onClick={(e) => {
              e.stopPropagation()
              row.toggleExpanded()
            }}
          >
            <ChevronRight
              className={cn(
                'size-4 shrink-0 transition-transform',
                row.getIsExpanded() && 'rotate-90',
              )}
              aria-hidden="true"
            />
          </button>
        ),
        enableSorting: false,
        enableResizing: false,
        enableHiding: false,
        size: 36,
        minSize: 36,
      })
    }

    cols.push(...columns)

    if (hasRowActions) {
      // Placeholder: the body renders the menu for this column.
      cols.push({
        id: '__actions',
        header: () => null,
        cell: () => null,
        enableSorting: false,
        enableResizing: false,
        enableHiding: false,
        size: 44,
        minSize: 44,
      })
    }

    return { allColumns: cols, rows: data.slice() }
  }, [
    data,
    columns,
    enableRowSelection,
    enableMultiRowSelection,
    hasDetailPanel,
    hasRowActions,
  ])

  // TanStack Table returns functions React Compiler can't memoize; it opts out
  // on its own, so silence the advisory.
  // eslint-disable-next-line react-hooks/incompatible-library
  const table = useReactTable({
    data: rows,
    columns: allColumns,
    defaultColumn: { size: DEFAULT_COLUMN_SIZE, minSize: MIN_COLUMN_SIZE },
    rowCount: totalRows,
    state: {
      pagination: { pageIndex: pagination.page, pageSize: pagination.pageSize },
      sorting: toTanstackSorting(sorting),
      rowSelection: selection,
      columnVisibility: persisted.columnVisibility,
      columnSizing: persisted.columnSizing,
      columnOrder,
      expanded,
    },

    manualPagination: isServer,
    autoResetPageIndex: false,
    onPaginationChange: (updater) => {
      const next = resolve(updater, {
        pageIndex: pagination.page,
        pageSize: pagination.pageSize,
      })
      setPagination({ page: next.pageIndex, pageSize: next.pageSize })
    },

    manualSorting: isServer,
    enableSorting: sortingEnabled,
    enableSortingRemoval: true,
    // Every column sorts ascending first. Left to itself the library starts
    // numeric columns descending, so the same click would mean two things.
    sortDescFirst: false,
    onSortingChange: (updater) => {
      const next = fromTanstackSorting(resolve(updater, toTanstackSorting(sorting)))
      setPersisted((prev) => ({ ...prev, sorting: next }))
      if (isServer) onSortingChange?.(next)
      setPagination({ page: 0, pageSize: pagination.pageSize })
    },

    enableRowSelection,
    enableMultiRowSelection,
    onRowSelectionChange: (updater) => {
      const next = resolve(updater, selection)
      if (rowSelection === undefined) setInternalSelection(next)
      onRowSelectionChange?.(next, rowsForSelection(next, data, getRowId))
    },

    onColumnVisibilityChange: (updater) => {
      setPersisted((prev) => ({
        ...prev,
        columnVisibility: resolve(updater, prev.columnVisibility),
      }))
    },

    onColumnOrderChange: (updater) => {
      setColumnOrder((prev) => resolve(updater, prev))
    },

    enableColumnResizing: enableColumnResize,
    columnResizeMode: 'onChange',
    onColumnSizingChange: (updater) => {
      setPersisted((prev) => ({
        ...prev,
        columnSizing: resolve(updater, prev.columnSizing),
      }))
    },

    onExpandedChange: (updater) => {
      const next = resolve(updater, expanded)
      if (singleExpand && typeof next === 'object' && typeof expanded === 'object') {
        const opened = Object.keys(next).find((id) => next[id] && !expanded[id])
        if (opened) {
          setExpanded({ [opened]: true })
          return
        }
      }
      setExpanded(next)
    },

    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
    getExpandedRowModel: getExpandedRowModel(),
    getRowId: getRowId ? (row) => getRowId(row) : undefined,
  })

  return { table, isServer, pagination, setPagination, totalRows, selectedRows }
}
