import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import { DataTablePage } from '@/components/app/data-table'

describe('DataTablePage', () => {
  it('renders its children in a column that fills the space it is given', () => {
    render(
      <DataTablePage>
        <h1>Title</h1>
        <p>Body</p>
      </DataTablePage>,
    )

    const column = screen.getByRole('heading', { name: 'Title' }).parentElement
    expect(column).toContainElement(screen.getByText('Body'))
    expect(column).toHaveClass('flex', 'flex-col', 'h-full')
  })
})
