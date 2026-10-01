import { Fragment } from 'react'
import { MoreHorizontal } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { LABELS } from './labels'
import type { RowAction } from './types'

interface DataTableRowActionsProps<TData> {
  row: TData
  actions: RowAction<TData>[]
  /** Names the row in the trigger's accessible name. */
  rowLabel?: string
}

export function DataTableRowActions<TData>({
  row,
  actions,
  rowLabel,
}: DataTableRowActionsProps<TData>) {
  const visibleActions = actions.filter((a) => !a.hidden?.(row))
  if (visibleActions.length === 0) return null

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={(props) => (
          <Button
            variant="ghost"
            size="icon-xs"
            {...props}
            aria-label={rowLabel ? LABELS.actionsFor(rowLabel) : LABELS.actions}
            onClick={(e) => {
              // Keep the click from also firing the row's own click handler.
              e.stopPropagation()
              props.onClick?.(e)
            }}
          >
            <MoreHorizontal className="size-4" aria-hidden="true" />
          </Button>
        )}
      />
      {/* w-auto overrides the popup's default anchor width (the small trigger),
          so labels size to their content instead of clipping. */}
      <DropdownMenuContent align="end" className="w-auto min-w-44">
        {/* A menu label must live inside a group. */}
        <DropdownMenuGroup>
          <DropdownMenuLabel>{LABELS.actions}</DropdownMenuLabel>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        {visibleActions.map((action, i) => (
          <Fragment key={action.label}>
            {action.separatorBefore && i > 0 ? <DropdownMenuSeparator /> : null}
            <DropdownMenuItem
              variant={action.variant}
              disabled={action.disabled?.(row)}
              onClick={(e) => {
                e.stopPropagation()
                action.onClick(row)
              }}
              className="whitespace-nowrap"
            >
              {action.icon}
              {action.label}
            </DropdownMenuItem>
          </Fragment>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
