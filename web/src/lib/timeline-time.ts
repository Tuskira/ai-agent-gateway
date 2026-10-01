/**
 * Timestamp formatting for the Session Timeline. Every helper takes an
 * explicit time zone (and, where it matters, `now`) so the output is
 * deterministic in tests; the page passes the browser's own zone. Zones are
 * labelled with UTC offsets computed here ("UTC+05:30"), never with the
 * browser's locale-dependent short names.
 */

export interface TimeOptions {
  /** IANA zone, e.g. "Asia/Kolkata". */
  timeZone: string
  /** Decides whether the year is shown. Defaults to the current time. */
  now?: Date
}

/** Legacy IANA ids that browsers may still report, mapped to current names. */
const ZONE_ALIASES: Record<string, string> = {
  'Asia/Calcutta': 'Asia/Kolkata',
  'Asia/Saigon': 'Asia/Ho_Chi_Minh',
  'Asia/Katmandu': 'Asia/Kathmandu',
  'Asia/Rangoon': 'Asia/Yangon',
  'Europe/Kiev': 'Europe/Kyiv',
  'America/Buenos_Aires': 'America/Argentina/Buenos_Aires',
}

/** The zone id to show the user: legacy aliases replaced by current names. */
export function normalizeZoneId(timeZone: string): string {
  return ZONE_ALIASES[timeZone] ?? timeZone
}

/** The browser's IANA time zone, falling back to UTC when unavailable. */
export function browserTimeZone(): string {
  try {
    return normalizeZoneId(Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC')
  } catch {
    return 'UTC'
  }
}

function parts(
  date: Date,
  timeZone: string,
  options: Intl.DateTimeFormatOptions,
): Record<string, string> {
  const out: Record<string, string> = {}
  for (const p of new Intl.DateTimeFormat('en-US', {
    timeZone,
    hourCycle: 'h23',
    ...options,
  }).formatToParts(date)) {
    out[p.type] = p.value
  }
  return out
}

/** Offset of `timeZone` from UTC at this instant, in minutes (DST-correct):
 * the zone's wall clock read as if it were UTC, minus the real instant. */
export function zoneOffsetMinutes(date: Date, timeZone: string): number {
  const p = parts(date, timeZone, {
    year: 'numeric',
    month: 'numeric',
    day: 'numeric',
    hour: 'numeric',
    minute: 'numeric',
    second: 'numeric',
  })
  const n = (k: string) => Number(p[k] ?? 0)
  const wall = Date.UTC(n('year'), n('month') - 1, n('day'), n('hour'), n('minute'), n('second'))
  return Math.round((wall - Math.floor(date.getTime() / 1000) * 1000) / 60000)
}

/** "UTC+05:30", "UTC\u221207:00" (real minus sign), or plain "UTC" at zero. */
export function formatUtcOffset(date: Date, timeZone: string): string {
  const minutes = zoneOffsetMinutes(date, timeZone)
  if (minutes === 0) return 'UTC'
  const abs = Math.abs(minutes)
  const hh = String(Math.floor(abs / 60)).padStart(2, '0')
  const mm = String(abs % 60).padStart(2, '0')
  return `UTC${minutes < 0 ? '\u2212' : '+'}${hh}:${mm}`
}

/** "30 Sep 20:59:52", or "30 Sep 2025 20:59:52" when the year is not the
 * current one. Null for an unparseable timestamp. */
export function formatCompactTime(iso: string, { timeZone, now = new Date() }: TimeOptions): string | null {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return null
  const p = parts(date, timeZone, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
  const currentYear = parts(now, timeZone, { year: 'numeric' }).year
  const year = p.year === currentYear ? '' : ` ${p.year}`
  return `${p.day} ${p.month}${year} ${p.hour}:${p.minute}:${p.second}`
}

/** Exact value for a tooltip: "30 Sep 2026, 20:59:52.418 UTC+05:30 · 15:29:52.418 UTC". */
export function formatExactTime(iso: string, { timeZone }: TimeOptions): string | null {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return null
  const clock = {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    fractionalSecondDigits: 3,
  } as const
  const local = parts(date, timeZone, { year: 'numeric', month: 'short', day: 'numeric', ...clock })
  const utc = parts(date, 'UTC', clock)
  const hms = (x: Record<string, string>) =>
    `${x.hour}:${x.minute}:${x.second}.${x.fractionalSecond}`
  return `${local.day} ${local.month} ${local.year}, ${hms(local)} ${formatUtcOffset(date, timeZone)} · ${hms(utc)} UTC`
}

/** Header note: "Times in UTC+05:30 (Asia/Kolkata)". `now` should be the most
 * recent event's instant so the offset matches what the rows show. */
export function formatZoneHeader({ timeZone, now = new Date() }: TimeOptions): string {
  return `Times in ${formatUtcOffset(now, timeZone)} (${normalizeZoneId(timeZone)})`
}
