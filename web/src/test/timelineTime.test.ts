import { describe, expect, it } from 'vitest'
import {
  formatCompactTime,
  formatExactTime,
  formatZoneHeader,
  formatUtcOffset,
  normalizeZoneId,
} from '@/lib/timeline-time'

const NOW = new Date('2026-10-01T12:00:00Z')
const TS = '2026-09-30T15:29:52.698Z'
const IST = { timeZone: 'Asia/Kolkata', now: NOW }
const LA = { timeZone: 'America/Los_Angeles', now: NOW }

describe('formatCompactTime', () => {
  it('omits the year in the current year', () => {
    expect(formatCompactTime(TS, IST)).toBe('30 Sep 20:59:52')
  })

  it('adds the year when it differs from the current one', () => {
    expect(formatCompactTime('2025-09-30T15:29:52Z', IST)).toBe('30 Sep 2025 20:59:52')
  })

  it('applies the given zone, including across a calendar-day boundary', () => {
    const late = '2026-01-01T02:00:00Z'
    expect(formatCompactTime(late, IST)).toBe('1 Jan 07:30:00')
    // Still Dec 31 in Los Angeles, and so last year.
    expect(formatCompactTime(late, LA)).toBe('31 Dec 2025 18:00:00')
    expect(formatCompactTime(TS, LA)).toBe('30 Sep 08:29:52')
  })

  it('uses 24-hour time at midnight', () => {
    expect(formatCompactTime('2026-09-30T18:30:00Z', IST)).toBe('1 Oct 00:00:00')
  })

  it('returns null for an unparseable timestamp', () => {
    expect(formatCompactTime('nope', IST)).toBeNull()
  })
})

describe('formatExactTime', () => {
  it('shows milliseconds, the UTC offset and UTC', () => {
    expect(formatExactTime(TS, IST)).toBe(
      '30 Sep 2026, 20:59:52.698 UTC+05:30 \u00b7 15:29:52.698 UTC',
    )
    expect(formatExactTime(TS, LA)).toBe(
      '30 Sep 2026, 08:29:52.698 UTC\u221207:00 \u00b7 15:29:52.698 UTC',
    )
  })
})

describe('formatUtcOffset', () => {
  const summer = new Date('2026-07-01T12:00:00Z')
  const winter = new Date('2026-01-15T12:00:00Z')
  it('formats positive, negative and fractional offsets', () => {
    expect(formatUtcOffset(summer, 'Asia/Kolkata')).toBe('UTC+05:30')
    expect(formatUtcOffset(summer, 'Asia/Kathmandu')).toBe('UTC+05:45')
    expect(formatUtcOffset(summer, 'America/Los_Angeles')).toBe('UTC\u221207:00')
    expect(formatUtcOffset(winter, 'America/Los_Angeles')).toBe('UTC\u221208:00')
  })
  it('shows plain UTC at a zero offset', () => {
    expect(formatUtcOffset(summer, 'Etc/UTC')).toBe('UTC')
    expect(formatUtcOffset(winter, 'Europe/London')).toBe('UTC')
    expect(formatUtcOffset(summer, 'Europe/London')).toBe('UTC+01:00')
  })
})

describe('zone header', () => {
  it('shows the offset and zone id', () => {
    expect(formatZoneHeader(IST)).toBe('Times in UTC+05:30 (Asia/Kolkata)')
    expect(formatZoneHeader({ timeZone: 'Etc/UTC', now: NOW })).toBe('Times in UTC (Etc/UTC)')
    expect(formatZoneHeader({ timeZone: 'Europe/London', now: new Date('2026-01-15T00:00:00Z') })).toBe(
      'Times in UTC (Europe/London)',
    )
  })
  it('uses the offset of the given instant', () => {
    const z = 'America/Los_Angeles'
    expect(formatZoneHeader({ timeZone: z, now: new Date('2026-07-01T00:00:00Z') })).toBe(
      'Times in UTC\u221207:00 (America/Los_Angeles)',
    )
    expect(formatZoneHeader({ timeZone: z, now: new Date('2026-12-01T00:00:00Z') })).toBe(
      'Times in UTC\u221208:00 (America/Los_Angeles)',
    )
  })
  it('normalises legacy zone ids', () => {
    expect(normalizeZoneId('Asia/Calcutta')).toBe('Asia/Kolkata')
    expect(normalizeZoneId('Asia/Saigon')).toBe('Asia/Ho_Chi_Minh')
    expect(normalizeZoneId('Asia/Katmandu')).toBe('Asia/Kathmandu')
    expect(normalizeZoneId('Asia/Rangoon')).toBe('Asia/Yangon')
    expect(normalizeZoneId('Europe/Kiev')).toBe('Europe/Kyiv')
    expect(normalizeZoneId('America/Buenos_Aires')).toBe('America/Argentina/Buenos_Aires')
    expect(normalizeZoneId('Europe/Paris')).toBe('Europe/Paris')
    expect(formatZoneHeader({ timeZone: 'Asia/Calcutta', now: NOW })).toBe(
      'Times in UTC+05:30 (Asia/Kolkata)',
    )
  })
})
