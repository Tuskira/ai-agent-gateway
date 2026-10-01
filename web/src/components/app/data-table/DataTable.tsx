import { Table } from '@/components/ui/table'
import { cn } from '@/lib/utils'
import { DataTableBody } from './DataTableBody'
import { DataTableEmpty } from './DataTableEmpty'
import { DataTableError } from './DataTableError'
import { ColumnReorderProvider, DataTableHeader } from './DataTableHeader'
import { DataTablePagination } from './DataTablePagination'
import { DataTableSelectionBar } from './DataTableSelectionBar'
import { DataTableSkeleton } from './DataTableSkeleton'
import { DataTableToolbar } from './DataTableToolbar'
import { LABELS } from './labels'
import type { DataTableProps } from './types'
import { useDataTable } from './use-data-table'

const DEFAULT_PAGE_SIZES = [10, 25, 50]

/**
 * The console's data grid: sorting, column resize / reorder / visibility,
 * pagination, selection, row actions and per-table persistence.
 *
 * Omit `pagination` and pass every row for client mode; pass `pagination`,
 * `onPaginationChange` and `totalRows` with one page of rows for server mode.
 */
export function DataTable<TData>(props: DataTableProps<TData>) {
  const {
    storageKey,
    loading = false,
    error = false,
    emptyTitle = LABELS.noResults,
    emptyDescription,
    emptyAction,
    skeletonRows = 5,
    pageSizeOptions = DEFAULT_PAGE_SIZES,
    rowActions,
    enableRowContextMenu = true,
    bulkActions,
    renderDetailPanel,
    onRowClick,
    getRowClassName,
    getRowLabel,
    expandOnRowClick = false,
    showToolbar = true,
    showTotalCount = true,
    toolbarControls,
    showExport = false,
    onExport,
    enableColumnVisibility = true,
    enableColumnReorder = false,
    paginationInToolbar = true,
    enableTooltip = true,
    rowHeight = 48,
    height,
    fillHeight = false,
    className,
  } = props

  const { table, isServer, pagination, setPagination, totalRows, selectedRows } =
    useDataTable(props)

  const columnCount = table.getVisibleLeafColumns().length
  const rowCount = table.getRowModel().rows.length

  // Exactly one body renders, in this order of precedence.
  const isError = error !== false
  const isInitialLoading = !isError && loading && rowCount === 0
  const isEmpty = !isError && !loading && rowCount === 0
  const hasRows = !isError && rowCount > 0
  const stretchBody = fillHeight && (isEmpty || isError)

  return (
    <div
      className={cn(
        // A flex column that may shrink: inside a parent of bounded height
        // (see `DataTablePage`) the rows scroll here rather than the page.
        // In any other parent it simply takes the height of its content.
        'relative flex min-h-0 flex-col gap-3',
        fillHeight && 'flex-1',
        className,
      )}
    >
      {showToolbar ? (
        <DataTableToolbar
          table={table}
          totalRows={totalRows}
          pagination={pagination}
          pageSizeOptions={pageSizeOptions}
          onPaginationChange={setPagination}
          showTotalCount={showTotalCount}
          toolbarControls={toolbarControls}
          showExport={showExport}
          exportAllRows={!isServer}
          exportFileName={`${storageKey}.csv`}
          onExport={onExport}
          enableColumnVisibility={enableColumnVisibility}
          paginationInToolbar={paginationInToolbar}
        />
      ) : null}

      <div
        // A scrolling region must be reachable and nameable without a mouse.
        role="region"
        aria-label={LABELS.table}
        tabIndex={0}
        // The Table primitive wraps <table> in its own scroll container. The
        // sticky header needs this element to be the nearest scrolling
        // ancestor, so that inner one is neutralised. `table-fixed` pins
        // column widths for the resize handles, and `isolate` scopes the
        // header's z-index to this grid.
        className={cn(
          'isolate min-h-0 overflow-auto rounded-r-5 border border-border bg-card',
          'outline-none focus-visible:ring-3 focus-visible:ring-ring/50',
          '[scrollbar-color:var(--color-border-strong)_transparent] [scrollbar-width:thin]',
          '[&_[data-slot=table-container]]:overflow-visible [&_[data-slot=table]]:table-fixed',
          // Row rules belong to the cells, not the rows. Collapsed row borders
          // do not travel with a sticky header or a pinned column; cell
          // borders do.
          '[&_[data-slot=table]]:border-separate [&_[data-slot=table]]:border-spacing-0',
          '[&_td]:border-b [&_td]:border-border [&_th]:border-b [&_th]:border-border',
          '[&_tbody_tr:last-child_td]:border-b-0',
          fillHeight && 'flex-1',
          stretchBody &&
            '[&_[data-slot=table-container]]:h-full [&_[data-slot=table]]:h-full',
        )}
        style={height ? { maxHeight: height } : undefined}
      >
        <ColumnReorderProvider table={table} enabled={enableColumnReorder}>
          {/* Column sizes are minimums. A wider region stretches the columns
              in proportion; a narrower one scrolls sideways rather than
              squeezing them. */}
          <Table style={{ minWidth: table.getTotalSize() }}>
            <DataTableHeader table={table} enableColumnReorder={enableColumnReorder} />

            {isError ? (
              <DataTableError
                columnCount={columnCount}
                message={typeof error === 'string' ? error : LABELS.error}
              />
            ) : null}

            {isInitialLoading ? (
              <DataTableSkeleton
                columnCount={columnCount}
                rowCount={skeletonRows}
                rowHeight={rowHeight}
              />
            ) : null}

            {isEmpty ? (
              <DataTableEmpty
                columnCount={columnCount}
                title={emptyTitle}
                description={emptyDescription}
                action={emptyAction}
                fill={fillHeight}
              />
            ) : null}

            {hasRows ? (
              <DataTableBody
                table={table}
                rowActions={rowActions}
                renderDetailPanel={renderDetailPanel}
                onRowClick={onRowClick}
                getRowClassName={getRowClassName}
                getRowLabel={getRowLabel}
                expandOnRowClick={expandOnRowClick}
                loading={loading}
                enableTooltip={enableTooltip}
                enableRowContextMenu={enableRowContextMenu}
                rowHeight={rowHeight}
              />
            ) : null}
          </Table>
        </ColumnReorderProvider>
      </div>

      {!paginationInToolbar ? (
        <div className="flex shrink-0 justify-end">
          <DataTablePagination
            pagination={pagination}
            totalRows={totalRows}
            pageSizeOptions={pageSizeOptions}
            onPaginationChange={setPagination}
          />
        </div>
      ) : null}

      <DataTableSelectionBar
        selectedRows={selectedRows}
        selectedCount={selectedRows.length}
        bulkActions={bulkActions}
        onClearSelection={() => table.resetRowSelection(true)}
      />
    </div>
  )
}
