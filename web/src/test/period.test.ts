import { describe, expect, it } from 'vitest'
import { periodLabel, periodParams, periodFromParams } from '@/lib/overview'

describe('dashboard periods', () => {
  it('turns a preset or custom dates into API params', () => {
    expect(periodParams({ range: '7d' })).toEqual({ range: '7d' })
    expect(
      periodParams({ range: 'custom', from: '2026-01-01', to: '2026-01-03' }),
    ).toEqual({
      from: '2026-01-01',
      to: '2026-01-03',
    })
  })

  it('labels a preset by name and custom dates by their span', () => {
    expect(periodLabel({ range: '24h' })).toBe('Last 24h')
    expect(periodLabel({ range: 'custom', from: '2026-01-01', to: '2026-01-03' })).toBe(
      'Jan 1 – Jan 3, 2026',
    )
    expect(periodLabel({ range: 'custom', from: '2026-01-05', to: '2026-01-05' })).toBe(
      'Jan 5, 2026',
    )
    expect(periodLabel({ range: 'custom', from: '2025-12-30', to: '2026-01-02' })).toBe(
      'Dec 30, 2025 – Jan 2, 2026',
    )
  })

  it('reads a period from the URL, falling back to the default on anything else', () => {
    const p = (q: string) => periodFromParams(new URLSearchParams(q), '24h')
    expect(p('')).toEqual({ range: '24h' })
    expect(p('range=30d')).toEqual({ range: '30d' })
    expect(p('range=today')).toEqual({ range: '24h' })
    expect(p('from=2026-01-01&to=2026-01-03&range=7d')).toEqual({
      range: 'custom',
      from: '2026-01-01',
      to: '2026-01-03',
    })
    expect(p('from=2026-01-01')).toEqual({ range: '24h' })
    expect(p('from=jan&to=feb')).toEqual({ range: '24h' })
  })
})
