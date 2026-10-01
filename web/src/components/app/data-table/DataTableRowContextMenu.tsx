import { Fragment, type ComponentProps, type ReactElement } from 'react'
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuGroup,
  ContextMenuItem,
  ContextMenuLabel,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from '@/components/ui/context-menu'
import { LABELS } from './labels'
import type { RowAction } from './types'

interface DataTableRowContextMenuProps<TData> {
  row: TData
  /** The same list the kebab menu uses, so actions are declared once. */
  actions: RowAction<TData>[]
  /**
   * Renders the `<tr>`. The trigger props carry the right-click and
   * long-press handlers plus the element ref, and must be spread onto it.
   */
  renderRow: (triggerProps?: ComponentProps<'tr'>) => ReactElement
}

/**
 * Right-click (or long-press) a row for the actions its kebab menu offers.
 * A row with no visible action renders bare, leaving the browser's own menu.
 */
export function DataTableRowContextMenu<TData>({
  row,
  actions,
  renderRow,
}: DataTableRowContextMenuProps<TData>) {
  const visibleActions = actions.filter((a) => !a.hidden?.(row))
  if (visibleActions.length === 0) return renderRow()

  return (
    <ContextMenu>
      <ContextMenuTrigger render={(props) => renderRow(props as ComponentProps<'tr'>)} />
      {/* w-auto: size to the labels rather than to the full-width row. */}
      <ContextMenuContent className="w-auto min-w-44">
        {/* A menu label must live inside a group. */}
        <ContextMenuGroup>
          <ContextMenuLabel>{LABELS.actions}</ContextMenuLabel>
        </ContextMenuGroup>
        <ContextMenuSeparator />
        {visibleActions.map((action, i) => (
          <Fragment key={action.label}>
            {action.separatorBefore && i > 0 ? <ContextMenuSeparator /> : null}
            <ContextMenuItem
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
            </ContextMenuItem>
          </Fragment>
        ))}
      </ContextMenuContent>
    </ContextMenu>
  )
}
