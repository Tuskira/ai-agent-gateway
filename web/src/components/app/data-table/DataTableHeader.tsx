import {
  useRef,
  type CSSProperties,
  type MouseEvent,
  type ReactNode,
  type TouchEvent,
} from 'react'
import type { Header, Table } from '@tanstack/react-table'
import {
  DndContext,
  KeyboardSensor,
  PointerSensor,
  closestCenter,
  useSensor,
  useSensors,
  type DragEndEvent,
} from '@dnd-kit/core'
import { restrictToHorizontalAxis } from '@dnd-kit/modifiers'
import {
  SortableContext,
  arrayMove,
  horizontalListSortingStrategy,
  sortableKeyboardCoordinates,
  useSortable,
} from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import { ArrowDown, ArrowUp, ArrowUpDown, GripVertical } from 'lucide-react'
import { TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { cn } from '@/lib/utils'
import { LABELS } from './labels'
import { renderTemplate } from './render-template'
import { isSystemColumn } from './types'

function SortIcon<TData>({ header }: { header: Header<TData, unknown> }) {
  if (!header.column.getCanSort()) return null
  const sorted = header.column.getIsSorted()

  return (
    <span className="ml-1 inline-flex shrink-0" aria-hidden="true">
      {sorted === 'asc' ? (
        <ArrowUp className="size-3.5" />
      ) : sorted === 'desc' ? (
        <ArrowDown className="size-3.5" />
      ) : (
        // Idle: a faint hint that the column sorts. Brightens on hover.
        <ArrowUpDown className="size-3.5 text-muted-foreground/40 transition-colors group-hover/th:text-muted-foreground" />
      )}
    </span>
  )
}

function ResizeHandle<TData>({
  header,
  onResizeEnd,
}: {
  header: Header<TData, unknown>
  onResizeEnd: () => void
}) {
  const isResizing = header.column.getIsResizing()

  const beginResize = (e: MouseEvent | TouchEvent) => {
    header.getResizeHandler()(e)
    // The drag is tracked on the document and can release anywhere, so listen
    // there for the end of it.
    document.addEventListener('mouseup', onResizeEnd, { once: true })
    document.addEventListener('touchend', onResizeEnd, { once: true })
  }

  return (
    <div
      role="separator"
      aria-orientation="vertical"
      onMouseDown={beginResize}
      onTouchStart={beginResize}
      onClick={(e) => e.stopPropagation()}
      onDoubleClick={(e) => {
        e.stopPropagation()
        header.column.resetSize()
      }}
      className={cn(
        'absolute top-1/4 right-0 h-1/2 w-1.5 cursor-col-resize touch-none select-none',
        // `before` is the visible line; the element itself is the wider hit area.
        'before:absolute before:inset-y-0 before:right-[3px] before:w-px before:bg-border',
        'before:transition-colors hover:before:w-0.5 hover:before:bg-ring',
        isResizing && 'before:w-0.5 before:bg-ring',
      )}
    />
  )
}

function HeadContent<TData>({
  header,
  dragHandle,
}: {
  header: Header<TData, unknown>
  dragHandle?: ReactNode
}) {
  if (header.isPlaceholder) return null
  const label = renderTemplate(header.column.columnDef.header, header.getContext())

  if (header.column.getCanSort()) {
    return (
      <div className="flex min-w-0 items-center pr-2">
        {dragHandle}
        {/* A real button, so sorting can be reached and run from the keyboard.
            It has no handler of its own: its click bubbles to the header cell,
            which sorts, so the whole cell stays a click target. */}
        <button
          type="button"
          className="flex min-w-0 cursor-pointer items-center rounded-sm text-left outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
        >
          <span className="truncate">{label}</span>
          <SortIcon header={header} />
        </button>
      </div>
    )
  }

  return (
    <div className="flex min-w-0 items-center pr-2">
      {dragHandle}
      {/* The select-all checkbox must not sit in a clipping wrapper. */}
      {header.column.id === '__select' ? (
        label
      ) : (
        <span className="truncate">{label}</span>
      )}
    </div>
  )
}

/**
 * Widths are a share of the total rather than fixed pixels. The table is never
 * narrower than the total, so a column is never narrower than its `size`; in a
 * wider region the columns stretch in proportion to fill it.
 */
function headWidth<TData>(header: Header<TData, unknown>, totalSize: number): string {
  return `${(header.getSize() / totalSize) * 100}%`
}

/**
 * Click-to-sort that ignores the click the browser synthesises at the end of a
 * column resize, which would otherwise sort the column.
 */
function useResizeAwareSort<TData>(header: Header<TData, unknown>) {
  const resizeEndAt = useRef(0)

  const onResizeEnd = () => {
    resizeEndAt.current = Date.now()
  }

  const onSortClick = header.column.getCanSort()
    ? (e: MouseEvent) => {
        if (Date.now() - resizeEndAt.current < 250) return
        header.column.getToggleSortingHandler()?.(e)
      }
    : undefined

  return { onResizeEnd, onSortClick }
}

function ariaSort<TData>(header: Header<TData, unknown>) {
  if (!header.column.getCanSort()) return undefined
  const sorted = header.column.getIsSorted()
  if (sorted === 'asc') return 'ascending'
  if (sorted === 'desc') return 'descending'
  return 'none'
}

const HEAD_CLASS = 'group/th relative overflow-hidden text-xs text-text-subtle'

/** Classes for one header cell. The actions column is pinned, as its cells are. */
function headClass(columnId: string, sortable: boolean): string {
  return cn(
    HEAD_CLASS,
    sortable && 'cursor-pointer select-none',
    columnId === '__select' && 'overflow-visible',
    columnId === '__actions' && 'sticky right-0 bg-card',
  )
}

interface HeadCellProps<TData> {
  header: Header<TData, unknown>
  totalSize: number
  showHandle: boolean
}

function PlainHeadCell<TData>({ header, totalSize, showHandle }: HeadCellProps<TData>) {
  const { onResizeEnd, onSortClick } = useResizeAwareSort(header)
  return (
    <TableHead
      aria-sort={ariaSort(header)}
      className={headClass(header.column.id, onSortClick !== undefined)}
      style={{ width: headWidth(header, totalSize) }}
      onClick={onSortClick}
    >
      <HeadContent header={header} />
      {showHandle ? <ResizeHandle header={header} onResizeEnd={onResizeEnd} /> : null}
    </TableHead>
  )
}

function SortableHeadCell<TData>({
  header,
  totalSize,
  showHandle,
}: HeadCellProps<TData>) {
  const reorderable = !isSystemColumn(header.column.id)
  const { onResizeEnd, onSortClick } = useResizeAwareSort(header)
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } =
    useSortable({ id: header.column.id, disabled: !reorderable })

  const style: CSSProperties = {
    width: headWidth(header, totalSize),
    transform: CSS.Translate.toString(transform),
    transition,
    ...(isDragging ? { zIndex: 2, opacity: 0.85 } : {}),
  }

  const dragHandle = reorderable ? (
    <button
      type="button"
      className="mr-1 -ml-1 flex shrink-0 cursor-grab items-center text-muted-foreground/50 opacity-0 transition-opacity group-hover/th:opacity-100 hover:text-muted-foreground focus-visible:opacity-100 active:cursor-grabbing"
      onClick={(e) => e.stopPropagation()}
      {...attributes}
      {...listeners}
      aria-label={LABELS.reorderColumn}
    >
      <GripVertical className="size-3.5" aria-hidden="true" />
    </button>
  ) : null

  return (
    <TableHead
      ref={setNodeRef}
      aria-sort={ariaSort(header)}
      className={headClass(header.column.id, onSortClick !== undefined)}
      style={style}
      onClick={onSortClick}
    >
      <HeadContent header={header} dragHandle={dragHandle} />
      {showHandle ? <ResizeHandle header={header} onResizeEnd={onResizeEnd} /> : null}
    </TableHead>
  )
}

/**
 * Drag-to-reorder context for the header.
 *
 * It must wrap `<Table>` from outside. The context renders its own
 * accessibility nodes, which would be invalid as direct children of `<table>`.
 */
export function ColumnReorderProvider<TData>({
  table,
  enabled,
  children,
}: {
  table: Table<TData>
  enabled: boolean
  children: ReactNode
}) {
  const sensors = useSensors(
    // A drag starts only once the pointer moves, so click-to-sort still works.
    useSensor(PointerSensor, { activationConstraint: { distance: 8 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  )

  function handleDragEnd(event: DragEndEvent) {
    const { active, over } = event
    if (!over || active.id === over.id) return
    const current = table.getState().columnOrder
    const base = current.length ? current : table.getAllLeafColumns().map((c) => c.id)
    const oldIndex = base.indexOf(String(active.id))
    const newIndex = base.indexOf(String(over.id))
    if (oldIndex === -1 || newIndex === -1) return
    // System columns stay pinned where the table put them.
    if (isSystemColumn(String(over.id))) return
    table.setColumnOrder(arrayMove(base, oldIndex, newIndex))
  }

  if (!enabled) return <>{children}</>

  return (
    <DndContext
      sensors={sensors}
      collisionDetection={closestCenter}
      modifiers={[restrictToHorizontalAxis]}
      onDragEnd={handleDragEnd}
    >
      {children}
    </DndContext>
  )
}

interface DataTableHeaderProps<TData> {
  table: Table<TData>
  enableColumnReorder: boolean
}

export function DataTableHeader<TData>({
  table,
  enableColumnReorder,
}: DataTableHeaderProps<TData>) {
  const totalSize = table.getTotalSize()
  const HeadCell = enableColumnReorder ? SortableHeadCell : PlainHeadCell

  return (
    <TableHeader className="sticky top-0 z-10 bg-card">
      {table.getHeaderGroups().map((headerGroup) => {
        const cells = headerGroup.headers.map((header, i, all) => (
          <HeadCell
            key={header.id}
            header={header}
            totalSize={totalSize}
            showHandle={
              header.column.getCanResize() &&
              !isSystemColumn(header.column.id) &&
              i !== all.length - 1
            }
          />
        ))
        return (
          <TableRow key={headerGroup.id} className="hover:bg-transparent">
            {enableColumnReorder ? (
              <SortableContext
                items={headerGroup.headers.map((h) => h.column.id)}
                strategy={horizontalListSortingStrategy}
              >
                {cells}
              </SortableContext>
            ) : (
              cells
            )}
          </TableRow>
        )
      })}
    </TableHeader>
  )
}
