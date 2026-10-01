import { describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { CircleCheck } from 'lucide-react'
import { WidgetHelp } from '@/components/app/WidgetHelp'
import { KPI_HELP, WIDGET_HELP } from '@/lib/overview-help'

const FULL = {
  short: 'Responses grouped by outcome.',
  blocks: [
    { kind: 'callout', body: 'Grouped by outcome, not HTTP status.' },
    { kind: 'heading', title: 'Outcomes' },
    {
      kind: 'card',
      title: '200 · Success',
      body: 'The call completed without an error.',
      tone: 'success',
      icon: CircleCheck,
    },
  ],
} as const

describe('WidgetHelp', () => {
  it('shows nothing but the info button until it is opened', () => {
    render(<WidgetHelp title="Health" content={{ ...FULL, blocks: [...FULL.blocks] }} />)

    expect(screen.getByRole('button', { name: 'About Health' })).toBeInTheDocument()
    expect(screen.queryByText(FULL.short)).not.toBeInTheDocument()
  })

  it('short mode: opens a popover with the title and the short text', async () => {
    const user = userEvent.setup()
    render(<WidgetHelp title="Health" content={{ short: FULL.short }} />)

    await user.click(screen.getByRole('button', { name: 'About Health' }))

    expect(await screen.findByText(FULL.short)).toBeInTheDocument()
    expect(screen.getByText('Health')).toBeInTheDocument()
    // Nothing more to show, so no way into the panel.
    expect(
      screen.queryByRole('button', { name: /View full details/ }),
    ).not.toBeInTheDocument()
  })

  it('full mode: "View full details" opens the panel with every block', async () => {
    const user = userEvent.setup()
    render(<WidgetHelp title="Health" content={{ ...FULL, blocks: [...FULL.blocks] }} />)

    await user.click(screen.getByRole('button', { name: 'About Health' }))
    await user.click(await screen.findByRole('button', { name: /View full details/ }))

    const panel = await screen.findByRole('dialog', { name: 'Health' })
    expect(within(panel).getByText(FULL.short)).toBeInTheDocument()
    expect(
      within(panel).getByText('Grouped by outcome, not HTTP status.'),
    ).toBeInTheDocument()
    expect(within(panel).getByText('Outcomes')).toBeInTheDocument()
    expect(within(panel).getByText('200 · Success')).toBeInTheDocument()
    expect(
      within(panel).getByText('The call completed without an error.'),
    ).toBeInTheDocument()
  })
})

describe('Overview help text', () => {
  it('gives every KPI tile and widget a short explanation', () => {
    for (const content of [...Object.values(KPI_HELP), ...Object.values(WIDGET_HELP)]) {
      expect(content.short.trim().length).toBeGreaterThan(20)
    }
  })

  it('gives every widget a full explanation', () => {
    for (const content of Object.values(WIDGET_HELP)) {
      expect(content.blocks.length).toBeGreaterThan(0)
    }
  })
})
