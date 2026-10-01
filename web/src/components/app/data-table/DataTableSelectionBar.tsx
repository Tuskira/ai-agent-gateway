import { X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Separator } from '@/components/ui/separator'
import { LABELS } from './labels'
import type { BulkAction } from './types'

interface DataTableSelectionBarProps<TData> {
  selectedRows: TData[]
  selectedCount: number
  bulkActions?: BulkAction<TData>[]
  onClearSelection: () => void
}

function buttonVariant(variant: BulkAction<unknown>['variant']) {
  if (variant === 'destructive') return 'destructive'
  if (variant === 'outline') return 'outline'
  return 'ghost'
}

/**
 * Floating bar shown while rows are selected: the count, the bulk actions and
 * a clear control. Its parent must be `relative`.
 */
export function DataTableSelectionBar<TData>({
  selectedRows,
  selectedCount,
  bulkActions,
  onClearSelection,
}: DataTableSelectionBarProps<TData>) {
  if (selectedCount === 0) return null

  const visibleActions = (bulkActions ?? []).filter(
    (a) => selectedCount >= (a.minSelected ?? 1),
  )

  return (
    // The wrapper is click-through so it never blocks the rows beneath.
    <div className="pointer-events-none absolute inset-x-0 bottom-4 z-20 flex justify-center px-4">
      <div
        role="toolbar"
        aria-label={`${selectedCount} ${LABELS.selected}`}
        className="pointer-events-auto flex animate-in items-center gap-2 rounded-lg border border-border bg-popover px-3 py-2 text-popover-foreground shadow-lg duration-200 fade-in-0 slide-in-from-bottom-4 motion-reduce:animate-none"
      >
        <span className="text-sm font-medium whitespace-nowrap tabular-nums">
          {selectedCount.toLocaleString()} {LABELS.selected}
        </span>

        {visibleActions.length > 0 ? (
          <>
            {/* Inline alignSelf beats the primitive's self-stretch. */}
            <Separator
              orientation="vertical"
              className="mx-1 h-5"
              style={{ alignSelf: 'center' }}
            />
            <div className="flex items-center gap-1">
              {visibleActions.map((action) => (
                <Button
                  key={action.label}
                  variant={buttonVariant(action.variant)}
                  size="sm"
                  onClick={() => action.onClick(selectedRows)}
                >
                  {action.icon}
                  {action.label}
                </Button>
              ))}
            </div>
          </>
        ) : null}

        <Separator
          orientation="vertical"
          className="mx-1 h-5"
          style={{ alignSelf: 'center' }}
        />
        <Button
          variant="ghost"
          size="sm"
          onClick={onClearSelection}
          className="text-muted-foreground hover:text-destructive"
        >
          <X className="size-4" aria-hidden="true" />
          {LABELS.clearSelection}
        </Button>
      </div>
    </div>
  )
}
