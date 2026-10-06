import { describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { PeriodFilter } from '@/components/app/PeriodFilter'

describe('PeriodFilter', () => {
  it('picks a preset from the segmented group', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<PeriodFilter value={{ range: '24h' }} onChange={onChange} />)
    const group = screen.getByRole('group', { name: 'Time range' })
    expect(within(group).getByRole('button', { name: 'Last 24h' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(screen.getByRole('button', { name: /custom/i })).toHaveTextContent('Custom')
    await user.click(within(group).getByRole('button', { name: 'Last 30d' }))
    expect(onChange).toHaveBeenCalledWith({ range: '30d' })
  })

  it('shows custom dates on the picker and no preset pressed', () => {
    render(
      <PeriodFilter
        value={{ range: 'custom', from: '2026-01-01', to: '2026-01-03' }}
        onChange={vi.fn()}
      />,
    )
    for (const b of within(
      screen.getByRole('group', { name: 'Time range' }),
    ).getAllByRole('button')) {
      expect(b).toHaveAttribute('aria-pressed', 'false')
    }
    expect(screen.getByRole('button', { name: /custom/i })).toHaveTextContent(
      'Jan 1 – Jan 3, 2026',
    )
  })

  it('applies a start and end date chosen on the calendar', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(
      <PeriodFilter
        value={{ range: 'custom', from: '2026-01-01', to: '2026-01-03' }}
        onChange={onChange}
      />,
    )
    await user.click(screen.getByRole('button', { name: /custom/i }))
    const apply = await screen.findByRole('button', { name: 'Apply' })
    await user.click(screen.getByRole('button', { name: /January 5th, 2026/ }))
    await user.click(screen.getByRole('button', { name: /January 9th, 2026/ }))
    await user.click(apply)
    expect(onChange).toHaveBeenCalledWith({
      range: 'custom',
      from: '2026-01-05',
      to: '2026-01-09',
    })
  })
})
