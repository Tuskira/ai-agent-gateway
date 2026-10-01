import { Fragment, type ComponentProps, type ReactNode } from 'react'
import type { Cell, Table } from '@tanstack/react-table'
import { Button } from '@/components/ui/button'
import { HoverCard, HoverCardContent, HoverCardTrigger } from '@/components/ui/hover-card'
import { TableBody, TableCell, TableRow } from '@/components/ui/table'
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'
import { DataTableRowActions } from './DataTableRowActions'
import { DataTableRowContextMenu } from './DataTableRowContextMenu'
import { renderTemplate } from './render-template'
import { isSystemColumn, type CellAction, type RowAction } from './types'

/** Hover dwell before a cell tooltip opens, to avoid flicker while scanning. */
const TOOLTIP_DELAY_MS = 300
/** Text shorter than this fits most columns and gets no default tooltip. */
const TOOLTIP_MIN_LENGTH = 20

/** Cells clip to their fixed column width instead of spilling into the next. */
const CELL_CLASS = 'overflow-hidden'

/**
 * The colour a row is showing, as a variable its cells can read. The pinned
 * actions cell sits on top of the cells that scroll beneath it, so it needs an
 * opaque background that still follows the row's hover and selected states.
 */
const ROW_BG_CLASS = cn(
  '[--row-bg:var(--color-card)]',
  'hover:[--row-bg:color-mix(in_oklab,var(--color-muted)_50%,var(--color-card))]',
  'has-aria-expanded:[--row-bg:color-mix(in_oklab,var(--color-muted)_50%,var(--color-card))]',
  'data-[state=selected]:[--row-bg:var(--color-muted)]',
)

/** Pinned to the trailing edge, so the menu stays in reach however wide the table. */
const ACTIONS_CELL_CLASS = 'sticky right-0 overflow-visible bg-(--row-bg)'

function content<TData>(cell: Cell<TData, unknown>): ReactNode {
  return renderTemplate(cell.column.columnDef.cell, cell.getContext())
}

function resolveCellActions<TData>(cell: Cell<TData, unknown>): CellAction<TData>[] {
  const spec = cell.column.columnDef.meta?.cellActions
  if (!spec) return []
  const row = cell.row.original
  const actions: CellAction<TData>[] = typeof spec === 'function' ? spec(row) : spec
  return actions.filter((a) => !a.hidden?.(row))
}

function Truncated({ children }: { children: ReactNode }) {
  return <div className="truncate">{children}</div>
}

function WithTooltip({ text, children }: { text: string; children: ReactNode }) {
  return (
    <TooltipProvider delay={TOOLTIP_DELAY_MS}>
      <Tooltip>
        <TooltipTrigger
          render={(props) => (
            <div {...props} className="truncate">
              {children}
            </div>
          )}
        />
        <TooltipContent>{text}</TooltipContent>
      </Tooltip>
    </TooltipProvider>
  )
}

/** Cell content plus icon buttons revealed on hover or keyboard focus. */
function CellWithActions<TData>({
  cell,
  actions,
}: {
  cell: Cell<TData, unknown>
  actions: CellAction<TData>[]
}) {
  const row = cell.row.original
  return (
    <div className="group/cell relative flex items-center">
      <Truncated>{content(cell)}</Truncated>
      <div className="absolute inset-y-0 right-0 flex items-center opacity-0 transition-opacity group-hover/cell:opacity-100 focus-within:opacity-100">
        <div className="flex items-center gap-0.5 rounded-md border border-border bg-background p-0.5 shadow-sm">
          {actions.map((action) => (
            <Button
              key={action.label}
              type="button"
              variant="ghost"
              size="icon-xs"
              title={action.label}
              aria-label={action.label}
              onClick={(e) => {
                e.stopPropagation()
                action.onClick(row)
              }}
            >
              {action.icon}
            </Button>
          ))}
        </div>
      </div>
    </div>
  )
}

function CellContent<TData>({
  cell,
  enableTooltip,
}: {
  cell: Cell<TData, unknown>
  enableTooltip: boolean
}) {
  const meta = cell.column.columnDef.meta
  const row = cell.row.original

  const cellActions = resolveCellActions(cell)
  if (cellActions.length > 0) return <CellWithActions cell={cell} actions={cellActions} />

  if (meta?.hoverCard) {
    return (
      <HoverCard>
        <HoverCardTrigger
          // The trigger is typed as a link; here it renders as a plain block.
          render={(props) => (
            <div {...(props as ComponentProps<'div'>)} className="truncate">
              {content(cell)}
            </div>
          )}
        />
        <HoverCardContent className="w-72">{meta.hoverCard(row)}</HoverCardContent>
      </HoverCard>
    )
  }

  const value = cell.getValue()
  // Objects would stringify to "[object Object]", so only primitives qualify.
  const valueText =
    typeof value === 'string' || typeof value === 'number' ? String(value) : ''

  if (meta?.tooltip !== undefined && meta.tooltip !== false) {
    const text =
      typeof meta.tooltip === 'function'
        ? meta.tooltip(row)
        : typeof meta.tooltip === 'string'
          ? meta.tooltip
          : valueText
    return text ? (
      <WithTooltip text={text}>{content(cell)}</WithTooltip>
    ) : (
      <Truncated>{content(cell)}</Truncated>
    )
  }

  if (
    enableTooltip &&
    meta?.tooltip !== false &&
    valueText.length >= TOOLTIP_MIN_LENGTH
  ) {
    return <WithTooltip text={valueText}>{content(cell)}</WithTooltip>
  }

  return <Truncated>{content(cell)}</Truncated>
}

interface DataTableBodyProps<TData> {
  table: Table<TData>
  rowActions?: RowAction<TData>[]
  renderDetailPanel?: (row: TData) => ReactNode
  onRowClick?: (row: TData) => void
  getRowClassName?: (row: TData) => string | undefined
  getRowLabel?: (row: TData) => string
  expandOnRowClick: boolean
  loading: boolean
  enableTooltip: boolean
  enableRowContextMenu: boolean
  rowHeight: number
}

export function DataTableBody<TData>({
  table,
  rowActions,
  renderDetailPanel,
  onRowClick,
  getRowClassName,
  getRowLabel,
  expandOnRowClick,
  loading,
  enableTooltip,
  enableRowContextMenu,
  rowHeight,
}: DataTableBodyProps<TData>) {
  const rows = table.getRowModel().rows
  const columnCount = table.getVisibleLeafColumns().length
  const canExpandOnClick = expandOnRowClick && renderDetailPanel !== undefined
  const isRowInteractive = onRowClick !== undefined || canExpandOnClick
  const hasRowActions = (rowActions?.length ?? 0) > 0

  return (
    // While the next page loads, keep the old rows visible but inert.
    <TableBody className={cn(loading && 'pointer-events-none opacity-50')}>
      {rows.map((row) => {
        const cells = row.getVisibleCells().map((cell) => {
          if (cell.column.id === '__actions') {
            return (
              <TableCell
                key={cell.id}
                className={ACTIONS_CELL_CLASS}
                // The menu is portaled, but React still bubbles its events
                // through this cell. Stop them here so a click on the menu's
                // heading, a divider or a disabled item never clicks the row.
                onClick={(e) => e.stopPropagation()}
              >
                {rowActions ? (
                  <DataTableRowActions
                    row={row.original}
                    actions={rowActions}
                    rowLabel={getRowLabel?.(row.original)}
                  />
                ) : null}
              </TableCell>
            )
          }

          if (isSystemColumn(cell.column.id)) {
            return (
              <TableCell
                key={cell.id}
                className="overflow-visible"
                // Ticking a row must not also click the row itself.
                onClick={(e) => e.stopPropagation()}
              >
                {content(cell)}
              </TableCell>
            )
          }

          return (
            <TableCell key={cell.id} className={CELL_CLASS}>
              <CellContent cell={cell} enableTooltip={enableTooltip} />
            </TableCell>
          )
        })

        // `triggerProps` come from the context menu and carry its handlers and
        // ref. Spread them first so the row's own props stay in charge.
        const renderRow = (triggerProps?: ComponentProps<'tr'>) => (
          <TableRow
            {...triggerProps}
            data-state={row.getIsSelected() ? 'selected' : undefined}
            aria-expanded={renderDetailPanel ? row.getIsExpanded() : undefined}
            className={cn(
              triggerProps?.className,
              ROW_BG_CLASS,
              // The trigger disables text selection; rows need it back.
              triggerProps && 'select-text',
              isRowInteractive && 'cursor-pointer',
              getRowClassName?.(row.original),
            )}
            // A runtime size, not a token. Table rows treat it as a minimum.
            style={{ height: rowHeight }}
            onClick={() => {
              if (canExpandOnClick) row.toggleExpanded()
              onRowClick?.(row.original)
            }}
          >
            {cells}
          </TableRow>
        )

        return (
          <Fragment key={row.id}>
            {enableRowContextMenu && hasRowActions && rowActions ? (
              <DataTableRowContextMenu
                row={row.original}
                actions={rowActions}
                renderRow={renderRow}
              />
            ) : (
              renderRow()
            )}

            {row.getIsExpanded() && renderDetailPanel ? (
              <TableRow className="hover:bg-transparent">
                {/* A detail panel is full-width wrapping content. */}
                <TableCell colSpan={columnCount} className="p-0 whitespace-normal">
                  <div className="bg-muted/30 px-4 py-3">
                    {renderDetailPanel(row.original)}
                  </div>
                </TableCell>
              </TableRow>
            ) : null}
          </Fragment>
        )
      })}
    </TableBody>
  )
}
