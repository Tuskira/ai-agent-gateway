import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {
  DataTable,
  tableToCsv,
  type ColumnDef,
  type DataTableProps,
  type RowAction,
} from '@/components/app/data-table'
import { useDataTable } from '@/components/app/data-table/use-data-table'

interface Item {
  id: string
  name: string
  count: number
}

const COLUMNS: ColumnDef<Item>[] = [
  { id: 'name', accessorKey: 'name', header: 'Name' },
  { id: 'count', accessorKey: 'count', header: 'Count' },
]

function makeItems(n: number): Item[] {
  return Array.from({ length: n }, (_, i) => ({
    id: `id-${i + 1}`,
    name: `item-${String(i + 1).padStart(2, '0')}`,
    count: (i * 7) % 5,
  }))
}

const STORAGE_KEY = 'gateway.table.test'

/** Names in the first body column, top to bottom. */
function visibleNames(): string[] {
  return screen
    .getAllByRole('row')
    .slice(1)
    .map((row) => within(row).getAllByRole('cell')[0]?.textContent ?? '')
}

function renderTable(props: Partial<DataTableProps<Item>> = {}) {
  return render(
    <DataTable<Item>
      columns={COLUMNS}
      data={makeItems(12)}
      storageKey="test"
      getRowId={(r) => r.id}
      {...(props as object)}
    />,
  )
}

describe('DataTable', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  describe('client mode', () => {
    it('shows 25 rows by default, with 25, 50 and 100 to choose from', async () => {
      const user = userEvent.setup()
      render(
        <DataTable<Item>
          columns={COLUMNS}
          data={makeItems(30)}
          storageKey="test"
          getRowId={(r) => r.id}
        />,
      )
      expect(visibleNames()).toHaveLength(25)
      await user.click(screen.getByRole('combobox', { name: /rows per page/i }))
      expect((await screen.findAllByRole('option')).map((o) => o.textContent)).toEqual([
        '25',
        '50',
        '100',
      ])
    })

    it('pages locally', async () => {
      const user = userEvent.setup()
      renderTable({ defaultPageSize: 5, pageSizeOptions: [5, 10] })

      expect(visibleNames()).toEqual([
        'item-01',
        'item-02',
        'item-03',
        'item-04',
        'item-05',
      ])
      expect(screen.getByText('12 items')).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Previous page' })).toBeDisabled()

      await user.click(screen.getByRole('button', { name: 'Next page' }))
      expect(visibleNames()).toEqual([
        'item-06',
        'item-07',
        'item-08',
        'item-09',
        'item-10',
      ])

      await user.click(screen.getByRole('button', { name: 'Next page' }))
      expect(visibleNames()).toEqual(['item-11', 'item-12'])
      expect(screen.getByRole('button', { name: 'Next page' })).toBeDisabled()
    })

    it('clamps to the last page when the rows shrink', async () => {
      const user = userEvent.setup()
      const { rerender } = renderTable({ defaultPageSize: 5, pageSizeOptions: [5, 10] })
      await user.click(screen.getByRole('button', { name: 'Next page' }))
      await user.click(screen.getByRole('button', { name: 'Next page' }))

      rerender(
        <DataTable<Item>
          columns={COLUMNS}
          data={makeItems(7)}
          storageKey="test"
          getRowId={(r) => r.id}
          defaultPageSize={5}
          pageSizeOptions={[5, 10]}
        />,
      )

      expect(visibleNames()).toEqual(['item-06', 'item-07'])
    })

    it('sorts ascending, descending, then unsorted', async () => {
      const user = userEvent.setup()
      const data: Item[] = [
        { id: 'b', name: 'bravo', count: 2 },
        { id: 'c', name: 'charlie', count: 3 },
        { id: 'a', name: 'alpha', count: 1 },
      ]
      renderTable({ data })
      const header = screen.getByRole('columnheader', { name: 'Name' })

      await user.click(header)
      expect(visibleNames()).toEqual(['alpha', 'bravo', 'charlie'])
      expect(header).toHaveAttribute('aria-sort', 'ascending')

      await user.click(header)
      expect(visibleNames()).toEqual(['charlie', 'bravo', 'alpha'])
      expect(header).toHaveAttribute('aria-sort', 'descending')

      await user.click(header)
      expect(visibleNames()).toEqual(['bravo', 'charlie', 'alpha'])
      expect(header).toHaveAttribute('aria-sort', 'none')
    })

    it('sorts from the keyboard', async () => {
      const user = userEvent.setup()
      const data: Item[] = [
        { id: 'b', name: 'bravo', count: 2 },
        { id: 'a', name: 'alpha', count: 1 },
      ]
      renderTable({ data })

      screen.getByRole('button', { name: 'Name' }).focus()
      await user.keyboard('{Enter}')
      expect(visibleNames()).toEqual(['alpha', 'bravo'])

      await user.keyboard(' ')
      expect(visibleNames()).toEqual(['bravo', 'alpha'])
    })

    it('sorts across every row, not only the current page', async () => {
      const user = userEvent.setup()
      renderTable({ defaultPageSize: 5, pageSizeOptions: [5, 10] })

      const header = screen.getByRole('columnheader', { name: 'Name' })
      await user.click(header)
      await user.click(header)

      expect(visibleNames()).toEqual([
        'item-12',
        'item-11',
        'item-10',
        'item-09',
        'item-08',
      ])
    })
  })

  describe('server mode', () => {
    it('reports page changes and never slices the rows it is given', async () => {
      const user = userEvent.setup()
      const onPaginationChange = vi.fn()
      renderTable({
        data: makeItems(3),
        pagination: { page: 0, pageSize: 2 },
        onPaginationChange,
        totalRows: 40,
        pageSizeOptions: [2, 25],
      })

      expect(visibleNames()).toEqual(['item-01', 'item-02', 'item-03'])
      expect(screen.getByText('40 items')).toBeInTheDocument()

      await user.click(screen.getByRole('button', { name: 'Next page' }))
      expect(onPaginationChange).toHaveBeenCalledWith({ page: 1, pageSize: 2 })
    })

    it('offers no sort buttons without onSortingChange', () => {
      renderTable({
        data: makeItems(3),
        pagination: { page: 0, pageSize: 25 },
        onPaginationChange: vi.fn(),
        totalRows: 3,
      })

      expect(screen.queryByRole('button', { name: 'Name' })).not.toBeInTheDocument()
    })

    it('exposes no sortable headers without onSortingChange', async () => {
      const user = userEvent.setup()
      const onPaginationChange = vi.fn()
      renderTable({
        data: makeItems(3),
        pagination: { page: 0, pageSize: 25 },
        onPaginationChange,
        totalRows: 3,
      })
      const header = screen.getByRole('columnheader', { name: 'Name' })

      expect(header).not.toHaveAttribute('aria-sort')
      await user.click(header)
      expect(visibleNames()).toEqual(['item-01', 'item-02', 'item-03'])
      expect(onPaginationChange).not.toHaveBeenCalled()
    })

    it('reports sort changes and returns to the first page', async () => {
      const user = userEvent.setup()
      const onPaginationChange = vi.fn()
      const onSortingChange = vi.fn()
      renderTable({
        data: makeItems(3),
        pagination: { page: 2, pageSize: 25 },
        onPaginationChange,
        totalRows: 100,
        sorting: [],
        onSortingChange,
      })

      await user.click(screen.getByRole('columnheader', { name: 'Name' }))

      expect(onSortingChange).toHaveBeenCalledWith([{ field: 'name', sort: 'asc' }])
      expect(onPaginationChange).toHaveBeenCalledWith({ page: 0, pageSize: 25 })
    })

    it('stays bounded when the total runs to a million rows', async () => {
      const user = userEvent.setup()
      renderTable({
        data: makeItems(3),
        pagination: { page: 0, pageSize: 25 },
        onPaginationChange: vi.fn(),
        totalRows: 1_000_000,
      })

      await user.click(screen.getByRole('button', { name: /1–25/ }))

      expect(screen.getAllByRole('option').length).toBeLessThanOrEqual(100)
    })

    it('falls back to a usable page size when given zero', () => {
      renderTable({
        data: makeItems(3),
        pagination: { page: 0, pageSize: 0 },
        onPaginationChange: vi.fn(),
        totalRows: 3,
      })

      expect(visibleNames()).toEqual(['item-01', 'item-02', 'item-03'])
      expect(screen.getByRole('button', { name: /1–3/ })).toBeInTheDocument()
    })
  })

  describe('render states', () => {
    it('shows only the error state, with the message given', () => {
      renderTable({ data: makeItems(2), error: "Couldn't load things.", loading: true })

      expect(screen.getByRole('alert')).toHaveTextContent("Couldn't load things.")
      expect(screen.queryByText('item-01')).not.toBeInTheDocument()
    })

    it('shows a generic message when error is true', () => {
      renderTable({ data: [], error: true })

      expect(screen.getByRole('alert')).toHaveTextContent(
        'Something went wrong loading this data.',
      )
    })

    it('shows skeleton rows on first load', () => {
      renderTable({ data: [], loading: true, skeletonRows: 3 })

      // Header row plus three skeleton rows.
      expect(screen.getAllByRole('row')).toHaveLength(4)
      expect(screen.queryByText('No results')).not.toBeInTheDocument()
    })

    it('shows the empty state with title, description and action', () => {
      renderTable({
        data: [],
        emptyTitle: 'No keys yet',
        emptyDescription: 'Create one to get started.',
        emptyAction: <button type="button">Create key</button>,
      })

      expect(screen.getByText('No keys yet')).toBeInTheDocument()
      expect(screen.getByText('Create one to get started.')).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Create key' })).toBeInTheDocument()
    })

    it('keeps rows on screen while the next page loads', () => {
      renderTable({ data: makeItems(2), loading: true })

      expect(screen.getByText('item-01')).toBeInTheDocument()
    })
  })

  describe('persistence', () => {
    it('restores a hidden column after a remount', async () => {
      const user = userEvent.setup()
      const { unmount } = renderTable()

      await user.click(screen.getByRole('button', { name: 'Toggle columns' }))
      await user.click(screen.getByRole('checkbox', { name: 'Count' }))
      expect(
        screen.queryByRole('columnheader', { name: 'Count' }),
      ).not.toBeInTheDocument()

      unmount()
      renderTable()

      expect(
        screen.queryByRole('columnheader', { name: 'Count' }),
      ).not.toBeInTheDocument()
      expect(screen.getByRole('columnheader', { name: 'Name' })).toBeInTheDocument()
    })

    it('restores the sort after a remount', async () => {
      const user = userEvent.setup()
      const { unmount } = renderTable({ defaultPageSize: 5, pageSizeOptions: [5] })
      const header = screen.getByRole('columnheader', { name: 'Name' })
      await user.click(header)
      await user.click(header)

      unmount()
      renderTable({ defaultPageSize: 5, pageSizeOptions: [5] })

      expect(visibleNames()[0]).toBe('item-12')
    })

    it('lets defaultSorting override a saved sort', () => {
      localStorage.setItem(
        STORAGE_KEY,
        JSON.stringify({ v: 1, state: { sorting: [{ field: 'name', sort: 'desc' }] } }),
      )
      renderTable({ defaultSorting: [{ field: 'name', sort: 'asc' }] })

      expect(visibleNames()[0]).toBe('item-01')
    })

    it('discards saved state when storageVersion changes', () => {
      localStorage.setItem(
        STORAGE_KEY,
        JSON.stringify({ v: 1, state: { columnVisibility: { count: false } } }),
      )
      renderTable({ storageVersion: 2 })

      expect(screen.getByRole('columnheader', { name: 'Count' })).toBeInTheDocument()
    })

    it('ignores saved state that is not valid JSON', () => {
      localStorage.setItem(STORAGE_KEY, '{not json')
      renderTable()

      expect(screen.getByRole('columnheader', { name: 'Count' })).toBeInTheDocument()
      expect(visibleNames()[0]).toBe('item-01')
    })

    it.each([
      ['sorting that is not a list', { sorting: 'name' }],
      ['a sort entry that is not an object', { sorting: [null] }],
      [
        'a sort entry with an unknown direction',
        { sorting: [{ field: 'name', sort: 'up' }] },
      ],
      ['visibility that is not an object', { columnVisibility: 'count' }],
      ['a visibility flag that is not a boolean', { columnVisibility: { count: 'no' } }],
      ['a width that is not a number', { columnSizing: { name: 'wide' } }],
      ['a width of zero', { columnSizing: { name: 0 } }],
      ['state that is not an object', 'oops'],
    ])('falls back to defaults for %s', (_, state) => {
      localStorage.setItem(STORAGE_KEY, JSON.stringify({ v: 1, state }))
      renderTable()

      expect(visibleNames()[0]).toBe('item-01')
      expect(screen.getByRole('columnheader', { name: 'Name' })).toBeInTheDocument()
      expect(screen.getByRole('columnheader', { name: 'Count' })).toBeInTheDocument()
      expect(screen.getByRole('columnheader', { name: 'Name' }).style.width).not.toBe('')
      expect(
        screen.getByRole('columnheader', { name: 'Name' }).style.width,
      ).not.toContain('NaN')
    })

    it('keeps the valid parts of saved state when another part is malformed', () => {
      localStorage.setItem(
        STORAGE_KEY,
        JSON.stringify({
          v: 1,
          state: { sorting: 'name', columnVisibility: { count: false } },
        }),
      )
      renderTable()

      expect(
        screen.queryByRole('columnheader', { name: 'Count' }),
      ).not.toBeInTheDocument()
      expect(visibleNames()[0]).toBe('item-01')
    })

    it('ignores saved state that names a column that no longer exists', () => {
      vi.spyOn(console, 'error').mockImplementation(() => {})
      vi.spyOn(console, 'warn').mockImplementation(() => {})
      localStorage.setItem(
        STORAGE_KEY,
        JSON.stringify({
          v: 1,
          state: {
            sorting: [{ field: 'removed', sort: 'desc' }],
            columnVisibility: { removed: false },
            columnSizing: { removed: 300 },
          },
        }),
      )
      renderTable()

      expect(visibleNames()[0]).toBe('item-01')
      expect(screen.getByRole('columnheader', { name: 'Name' })).toBeInTheDocument()
    })

    it('keeps working when storage throws', async () => {
      const user = userEvent.setup()
      vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
        throw new Error('storage disabled')
      })
      vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
        throw new Error('storage disabled')
      })
      renderTable()

      await user.click(screen.getByRole('columnheader', { name: 'Name' }))
      await user.click(screen.getByRole('columnheader', { name: 'Name' }))

      expect(visibleNames()[0]).toBe('item-12')
    })
  })

  describe('rows', () => {
    it('applies getRowClassName to the row element', () => {
      renderTable({
        data: makeItems(2),
        getRowClassName: (r) => (r.id === 'id-2' ? 'opacity-55' : undefined),
      })

      expect(screen.getByText('item-02').closest('tr')).toHaveClass('opacity-55')
      expect(screen.getByText('item-01').closest('tr')).not.toHaveClass('opacity-55')
    })

    it('calls onRowClick with the row', async () => {
      const user = userEvent.setup()
      const onRowClick = vi.fn()
      renderTable({ data: makeItems(2), onRowClick })

      await user.click(screen.getByText('item-02'))

      expect(onRowClick).toHaveBeenCalledWith(
        expect.objectContaining({ id: 'id-2', name: 'item-02' }),
      )
    })

    it('shows one detail panel at a time', async () => {
      const user = userEvent.setup()
      renderTable({
        data: makeItems(2),
        renderDetailPanel: (r) => <p>details for {r.name}</p>,
      })
      const toggles = screen.getAllByRole('button', { name: 'Toggle row details' })

      await user.click(toggles[0]!)
      expect(screen.getByText('details for item-01')).toBeInTheDocument()

      await user.click(toggles[1]!)
      expect(screen.getByText('details for item-02')).toBeInTheDocument()
      expect(screen.queryByText('details for item-01')).not.toBeInTheDocument()
    })
  })

  describe('scrolling', () => {
    it('exposes the rows as a named scroll region that the keyboard can reach', () => {
      renderTable()

      const region = screen.getByRole('region', { name: 'Table' })
      expect(region).toHaveAttribute('tabindex', '0')
      expect(within(region).getByRole('table')).toBeInTheDocument()
    })

    it('never squeezes columns below their declared sizes', () => {
      renderTable({
        columns: [
          { id: 'name', accessorKey: 'name', header: 'Name', size: 300 },
          { id: 'count', accessorKey: 'count', header: 'Count', size: 100 },
        ],
        rowActions: [{ label: 'Edit', onClick: vi.fn() }],
      })

      // 300 + 100, plus 44 for the actions column.
      expect(screen.getByRole('table')).toHaveStyle({ minWidth: '444px' })
    })

    it('keeps the toolbar outside the scroll region, so it never scrolls away', () => {
      renderTable()

      const region = screen.getByRole('region', { name: 'Table' })
      expect(
        within(region).queryByRole('button', { name: 'Next page' }),
      ).not.toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Next page' })).toBeInTheDocument()
    })
  })

  describe('unstable props', () => {
    it('re-reads accessor values when a lookup they depend on changes', async () => {
      const user = userEvent.setup()
      const rows = makeItems(3)
      function Host() {
        const [counts, setCounts] = useState<Record<string, number>>({})
        return (
          <>
            <button
              type="button"
              onClick={() => setCounts({ 'id-1': 5, 'id-2': 50, 'id-3': 1 })}
            >
              load counts
            </button>
            <DataTable<Item>
              columns={[
                { id: 'name', accessorKey: 'name', header: 'Name' },
                {
                  id: 'tools',
                  accessorFn: (r) => counts[r.id],
                  header: 'Tools',
                  sortUndefined: 'last',
                },
              ]}
              data={rows}
              storageKey="test"
              getRowId={(r) => r.id}
            />
          </>
        )
      }
      render(<Host />)

      await user.click(screen.getByRole('button', { name: 'load counts' }))
      await user.click(screen.getByRole('columnheader', { name: 'Tools' }))

      expect(visibleNames()).toEqual(['item-03', 'item-01', 'item-02'])
    })

    it('survives data and columns that are rebuilt on every render', async () => {
      const user = userEvent.setup()
      function Host() {
        const [count, setCount] = useState(0)
        return (
          <>
            <button type="button" onClick={() => setCount((c) => c + 1)}>
              rerender {count}
            </button>
            <DataTable<Item>
              columns={[
                {
                  id: 'name',
                  accessorKey: 'name',
                  header: 'Name',
                  cell: ({ row }) => <span>{row.original.name}</span>,
                },
              ]}
              data={makeItems(3)}
              storageKey="test"
              getRowId={(r) => r.id}
              enableRowSelection
              renderDetailPanel={(r) => <p>details for {r.name}</p>}
            />
          </>
        )
      }
      render(<Host />)
      const firstCell = screen.getByText('item-01')

      await user.click(screen.getAllByRole('button', { name: 'Toggle row details' })[0]!)
      await user.click(screen.getAllByRole('checkbox', { name: 'Select row' })[1]!)
      await user.click(screen.getByRole('button', { name: /rerender/ }))
      await user.click(screen.getByRole('button', { name: /rerender/ }))

      expect(screen.getByRole('button', { name: 'rerender 2' })).toBeInTheDocument()
      // The same DOM node, not a remounted copy.
      expect(firstCell).toBeInTheDocument()
      expect(screen.getByText('details for item-01')).toBeInTheDocument()
      expect(screen.getByText('1 selected')).toBeInTheDocument()
    })
  })

  describe('row actions', () => {
    const onEdit = vi.fn()
    const onDelete = vi.fn()
    const actions: RowAction<Item>[] = [
      { label: 'Edit', onClick: onEdit },
      { label: 'Archive', onClick: vi.fn(), hidden: (r) => r.id === 'id-1' },
      { label: 'Rotate', onClick: vi.fn(), disabled: (r) => r.id === 'id-1' },
      {
        label: 'Delete',
        onClick: onDelete,
        variant: 'destructive',
        separatorBefore: true,
      },
    ]

    beforeEach(() => {
      onEdit.mockClear()
      onDelete.mockClear()
    })

    it('names each menu trigger after its row', () => {
      renderTable({ data: makeItems(2), rowActions: actions, getRowLabel: (r) => r.name })

      expect(
        screen.getByRole('button', { name: 'Actions for item-01' }),
      ).toBeInTheDocument()
      expect(
        screen.getByRole('button', { name: 'Actions for item-02' }),
      ).toBeInTheDocument()
    })

    it('honours hidden, disabled and separatorBefore', async () => {
      const user = userEvent.setup()
      renderTable({ data: makeItems(2), rowActions: actions, getRowLabel: (r) => r.name })

      await user.click(screen.getByRole('button', { name: 'Actions for item-01' }))

      const menu = await screen.findByRole('menu')
      expect(within(menu).getByRole('menuitem', { name: 'Edit' })).toBeInTheDocument()
      expect(
        within(menu).queryByRole('menuitem', { name: 'Archive' }),
      ).not.toBeInTheDocument()
      expect(within(menu).getByRole('menuitem', { name: 'Rotate' })).toHaveAttribute(
        'aria-disabled',
        'true',
      )
      // One separator under the heading, one above Delete.
      expect(within(menu).getAllByRole('separator')).toHaveLength(2)
    })

    it('runs the action without firing the row click', async () => {
      const user = userEvent.setup()
      const onRowClick = vi.fn()
      renderTable({
        data: makeItems(2),
        rowActions: actions,
        getRowLabel: (r) => r.name,
        onRowClick,
      })

      await user.click(screen.getByRole('button', { name: 'Actions for item-02' }))
      await user.click(await screen.findByRole('menuitem', { name: 'Delete' }))

      expect(onDelete).toHaveBeenCalledWith(expect.objectContaining({ id: 'id-2' }))
      expect(onRowClick).not.toHaveBeenCalled()
    })

    it('does not fire the row click for clicks that land inside the menu or its cell', async () => {
      const user = userEvent.setup()
      const onRowClick = vi.fn()
      renderTable({
        data: makeItems(2),
        rowActions: actions,
        getRowLabel: (r) => r.name,
        onRowClick,
      })
      const trigger = screen.getByRole('button', { name: 'Actions for item-01' })

      await user.click(trigger.closest('td')!)
      await user.click(trigger)
      const menu = await screen.findByRole('menu')
      await user.click(within(menu).getByText('Actions'))
      await user.click(within(menu).getByRole('menuitem', { name: 'Rotate' }))

      expect(onRowClick).not.toHaveBeenCalled()
    })

    it('pins the actions column, so the menu stays in reach when the table scrolls sideways', () => {
      renderTable({ data: makeItems(2), rowActions: actions, getRowLabel: (r) => r.name })

      const cell = screen
        .getByRole('button', { name: 'Actions for item-01' })
        .closest('td')
      expect(cell).toHaveClass('sticky', 'right-0')
      const headers = screen.getAllByRole('columnheader')
      expect(headers[headers.length - 1]).toHaveClass('sticky', 'right-0')
      expect(headers[0]).not.toHaveClass('sticky')
    })

    it('renders no menu for a row whose actions are all hidden', () => {
      renderTable({
        data: makeItems(2),
        rowActions: [{ label: 'Edit', onClick: onEdit, hidden: (r) => r.id === 'id-1' }],
        getRowLabel: (r) => r.name,
      })

      expect(
        screen.queryByRole('button', { name: 'Actions for item-01' }),
      ).not.toBeInTheDocument()
      expect(
        screen.getByRole('button', { name: 'Actions for item-02' }),
      ).toBeInTheDocument()
    })
  })

  describe('selection', () => {
    it('shows the selection bar, runs a bulk action, and clears', async () => {
      const user = userEvent.setup()
      const onBulk = vi.fn()
      renderTable({
        data: makeItems(3),
        enableRowSelection: true,
        bulkActions: [{ label: 'Revoke selected', onClick: onBulk }],
      })
      expect(screen.queryByText(/selected$/)).not.toBeInTheDocument()

      const boxes = screen.getAllByRole('checkbox', { name: 'Select row' })
      await user.click(boxes[0]!)
      await user.click(boxes[2]!)
      expect(screen.getByText('2 selected')).toBeInTheDocument()

      await user.click(screen.getByRole('button', { name: 'Revoke selected' }))
      expect(onBulk).toHaveBeenCalledWith([
        expect.objectContaining({ id: 'id-1' }),
        expect.objectContaining({ id: 'id-3' }),
      ])

      await user.click(screen.getByRole('button', { name: 'Clear selection' }))
      expect(screen.queryByText('2 selected')).not.toBeInTheDocument()
    })
  })

  describe('export', () => {
    function Probe({
      onCsv,
      ...props
    }: DataTableProps<Item> & { onCsv: (csv: string) => void }) {
      const { table, isServer } = useDataTable(props)
      onCsv(tableToCsv(table, !isServer))
      return null
    }

    it('writes every row in client mode, skipping hidden and system columns', () => {
      const onCsv = vi.fn()
      render(
        <Probe
          onCsv={onCsv}
          columns={COLUMNS}
          data={makeItems(7)}
          storageKey="test"
          getRowId={(r) => r.id}
          defaultPageSize={5}
          enableRowSelection
          rowActions={[{ label: 'Edit', onClick: vi.fn() }]}
          defaultColumnVisibility={{ count: false }}
        />,
      )

      expect(onCsv).toHaveBeenLastCalledWith(
        [
          'Name',
          'item-01',
          'item-02',
          'item-03',
          'item-04',
          'item-05',
          'item-06',
          'item-07',
        ].join('\n'),
      )
    })

    it('calls onExport instead of downloading when it is given', async () => {
      const user = userEvent.setup()
      const onExport = vi.fn()
      renderTable({ showExport: true, onExport })

      await user.click(screen.getByRole('button', { name: 'Export' }))

      expect(onExport).toHaveBeenCalledTimes(1)
    })
  })
})
