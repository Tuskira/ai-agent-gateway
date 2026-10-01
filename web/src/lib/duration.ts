/**
 * A duration in the compact, human-readable form the Session Timeline uses:
 *
 *   < 1 s   "984 ms"
 *   < 1 min "44.9 s"
 *   < 1 h   "3m 53.8s"
 *   >= 1 h  "1h 02m 05s"
 *
 * Boundaries are decided after rounding to the precision shown, so a value
 * never reads as "60.0 s" or "1000 ms". Anything that is not a usable
 * duration reads as an em dash, never a made-up number.
 */
export function formatReadableDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—'

  const wholeMs = Math.round(ms)
  if (wholeMs < 1000) return `${wholeMs} ms`

  // Work in tenths of a second so rounding carries into the next unit.
  const tenths = Math.round(ms / 100)
  if (tenths < 600) return `${(tenths / 10).toFixed(1)} s`
  if (tenths < 36000) {
    const minutes = Math.floor(tenths / 600)
    const seconds = (tenths % 600) / 10
    return `${minutes}m ${seconds.toFixed(1)}s`
  }

  const total = Math.round(ms / 1000)
  const hours = Math.floor(total / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  const seconds = total % 60
  return `${hours}h ${String(minutes).padStart(2, '0')}m ${String(seconds).padStart(2, '0')}s`
}
