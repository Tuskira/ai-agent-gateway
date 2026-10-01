import { describe, expect, it } from 'vitest'
import { endOfLocalDayISO } from '@/lib/dates'

describe('endOfLocalDayISO', () => {
  it('returns the UTC instant for 23:59:59 local time on a normal date', () => {
    const result = endOfLocalDayISO('2026-03-15')
    const expected = new Date(2026, 2, 15, 23, 59, 59).toISOString()
    expect(result).toBe(expected)
  })

  it('parses back to 23:59:59 local time on the given date, in any timezone', () => {
    const result = endOfLocalDayISO('2026-07-04')
    const parsed = new Date(result)
    expect(parsed.getFullYear()).toBe(2026)
    expect(parsed.getMonth()).toBe(6) // July, 0-indexed
    expect(parsed.getDate()).toBe(4)
    expect(parsed.getHours()).toBe(23)
    expect(parsed.getMinutes()).toBe(59)
    expect(parsed.getSeconds()).toBe(59)
  })

  it('handles a date at a month/year boundary', () => {
    const result = endOfLocalDayISO('2026-12-31')
    const expected = new Date(2026, 11, 31, 23, 59, 59).toISOString()
    expect(result).toBe(expected)
  })
})
