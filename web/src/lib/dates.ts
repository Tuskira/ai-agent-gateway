/** Given a `YYYY-MM-DD` string, returns the ISO-8601 UTC instant
 * corresponding to 23:59:59 local time on that date. Used to turn a
 * date-only form field (e.g. an expiry picker) into the strict RFC 3339
 * timestamp the API requires, without silently shifting the calendar day
 * for people not in UTC. */
export function endOfLocalDayISO(dateStr: string): string {
  const year = Number(dateStr.slice(0, 4))
  const month = Number(dateStr.slice(5, 7))
  const day = Number(dateStr.slice(8, 10))
  const d = new Date(year, month - 1, day, 23, 59, 59)
  return d.toISOString()
}
