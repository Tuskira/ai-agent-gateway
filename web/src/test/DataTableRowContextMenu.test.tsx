import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import { DataTableRowContextMenu } from '@/components/app/data-table/DataTableRowContextMenu'
import type { RowAction } from '@/components/app/data-table/types'

interface Row {
  id: string
  status: string
}

const row: Row = { id: 'a1', status: 'open' }

function renderMenu(actions: RowAction<Row>[], onRowClick = vi.fn()) {
  return render(
    <table>
      <tbody>
        <DataTableRowContextMenu
          row={row}
          actions={actions}
          renderRow={(triggerProps) => (
            <tr {...triggerProps} data-testid="row" onClick={onRowClick}>
              <td>{row.id}</td>
            </tr>
          )}
        />
      </tbody>
    </table>,
  )
}

describe('DataTableRowContextMenu', () => {
  it('opens the row actions on right click and calls the handler with the row', () => {
    const onClick = vi.fn()
    renderMenu([{ label: 'View details', onClick }])

    fireEvent.contextMenu(screen.getByTestId('row'))
    fireEvent.click(screen.getByRole('menuitem', { name: 'View details' }))

    expect(onClick).toHaveBeenCalledWith(row)
  })

  it('omits actions hidden for the row', () => {
    renderMenu([
      { label: 'Assign', onClick: vi.fn() },
      { label: 'Reopen', onClick: vi.fn(), hidden: (r) => r.status === 'open' },
    ])

    fireEvent.contextMenu(screen.getByTestId('row'))

    expect(screen.getByRole('menuitem', { name: 'Assign' })).toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: 'Reopen' })).not.toBeInTheDocument()
  })

  it('renders a bare row when no action is visible', () => {
    renderMenu([{ label: 'Assign', onClick: vi.fn(), hidden: () => true }])

    fireEvent.contextMenu(screen.getByTestId('row'))

    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
    expect(screen.getByTestId('row')).not.toHaveAttribute(
      'data-slot',
      'context-menu-trigger',
    )
  })

  it('does not fire the row click when an item is chosen', () => {
    const onRowClick = vi.fn()
    renderMenu([{ label: 'Assign', onClick: vi.fn() }], onRowClick)

    fireEvent.contextMenu(screen.getByTestId('row'))
    fireEvent.click(screen.getByRole('menuitem', { name: 'Assign' }))

    expect(onRowClick).not.toHaveBeenCalled()
  })
})
