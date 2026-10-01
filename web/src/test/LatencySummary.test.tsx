import { describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import { LatencySummary } from '@/components/app/LatencySummary'
import { formatDuration } from '@/lib/overview'

const LATENCY = {
  medianMs: 2,
  p95Ms: 401,
  slowest: [
    { name: 'everything-requests-72593', ms: 1371 },
    { name: 'GET /mcp/stream', ms: 1038 },
    { name: 'gpt-4o-mini', ms: 906 },
  ],
}

function renderSummary(props: Partial<Parameters<typeof LatencySummary>[0]> = {}) {
  return render(
    <LatencySummary
      latency={LATENCY}
      loading={false}
      emptyLabel="No data yet"
      {...props}
    />,
  )
}

describe('formatDuration', () => {
  it.each([
    [0, '0ms'],
    [2, '2ms'],
    [906, '906ms'],
    [999.6, '1.00s'],
    [1371, '1.37s'],
    [9994, '9.99s'],
    [24133, '24.1s'],
    [59960, '1m 0s'],
    [125000, '2m 5s'],
  ])('writes %dms as %s', (ms, text) => {
    expect(formatDuration(ms)).toBe(text)
  })

  it('writes a value that is not a usable number as a dash', () => {
    expect(formatDuration(Number.NaN)).toBe('—')
    expect(formatDuration(-5)).toBe('—')
    expect(formatDuration(Number.POSITIVE_INFINITY)).toBe('—')
  })
})

describe('LatencySummary', () => {
  it('shows the median and p95, and how far apart they are', () => {
    renderSummary()

    expect(screen.getByRole('group', { name: 'Median' })).toHaveTextContent('2ms')
    const p95 = screen.getByRole('group', { name: 'p95' })
    expect(p95).toHaveTextContent('401ms')
    expect(p95).toHaveTextContent('201× median')
  })

  it('leaves out the comparison when the median is zero', () => {
    renderSummary({ latency: { ...LATENCY, medianMs: 0 } })

    expect(screen.queryByText(/× median/)).not.toBeInTheDocument()
  })

  it('lists the slowest calls in order, each with its duration', () => {
    renderSummary()

    const rows = within(screen.getByRole('list', { name: 'Slowest calls' })).getAllByRole(
      'listitem',
    )
    expect(rows.map((row) => row.textContent)).toEqual([
      'everything-requests-725931.37s',
      'GET /mcp/stream1.04s',
      'gpt-4o-mini906ms',
    ])
  })

  it('describes each call relative to p95 for assistive tech', () => {
    renderSummary()

    expect(
      screen.getByRole('listitem', {
        name: 'everything-requests-72593, 1.37s, 3.4× p95',
      }),
    ).toBeInTheDocument()
  })

  it('flags only the calls that pass the slow threshold', () => {
    renderSummary({
      latency: {
        medianMs: 487,
        p95Ms: 20255,
        slowest: [
          { name: 'mesh__run_query', ms: 24133 },
          { name: 'quick_lookup', ms: 900 },
        ],
      },
    })

    const rows = screen.getAllByRole('listitem')
    expect(within(rows[0]!).getByText('Slow')).toBeInTheDocument()
    expect(within(rows[1]!).queryByText('Slow')).not.toBeInTheDocument()
    // p95 itself is over the threshold here.
    expect(
      within(screen.getByRole('group', { name: 'p95' })).getByText('Slow'),
    ).toBeInTheDocument()
  })

  it('does not flag anything when every call is under the threshold', () => {
    renderSummary()

    expect(screen.queryByText('Slow')).not.toBeInTheDocument()
  })

  it('shows a loading state', () => {
    renderSummary({ latency: null, loading: true })

    expect(screen.getByRole('status', { name: 'Loading' })).toBeInTheDocument()
    expect(screen.queryByRole('list')).not.toBeInTheDocument()
  })

  it('shows dashes and the empty label when there is no data', () => {
    renderSummary({ latency: null })

    expect(screen.getByRole('group', { name: 'Median' })).toHaveTextContent('—')
    expect(screen.getByRole('group', { name: 'p95' })).toHaveTextContent('—')
    expect(screen.getByText('No data yet')).toBeInTheDocument()
  })

  it('shows the empty label when there are figures but no slow calls', () => {
    renderSummary({ latency: { ...LATENCY, slowest: [] } })

    expect(screen.getByRole('group', { name: 'Median' })).toHaveTextContent('2ms')
    expect(screen.getByText('No data yet')).toBeInTheDocument()
  })

  it('survives calls that report zero, and a p95 above every call', () => {
    renderSummary({
      latency: {
        medianMs: 0,
        p95Ms: 5000,
        slowest: [
          { name: 'a', ms: 0 },
          { name: 'b', ms: 0 },
        ],
      },
    })

    expect(screen.getAllByRole('listitem')).toHaveLength(2)
  })
})
