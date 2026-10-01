import { describe, expect, it } from 'vitest'
import { formatReadableDuration } from '@/lib/duration'

describe('formatReadableDuration', () => {
  it.each([
    [0, '0 ms'],
    [984, '984 ms'],
    [999, '999 ms'],
    [1000, '1.0 s'],
    [44900, '44.9 s'],
    [59999, '1m 0.0s'],
    [59949, '59.9 s'],
    [60000, '1m 0.0s'],
    [233777, '3m 53.8s'],
    [3599940, '59m 59.9s'],
    [3599999, '1h 00m 00s'],
    [3600000, '1h 00m 00s'],
    [3725000, '1h 02m 05s'],
    [90061000, '25h 01m 01s'],
  ])('%d ms -> %s', (ms, want) => {
    expect(formatReadableDuration(ms)).toBe(want)
  })

  it('reads unusable input as an em dash', () => {
    expect(formatReadableDuration(-1)).toBe('—')
    expect(formatReadableDuration(Number.NaN)).toBe('—')
  })
})
