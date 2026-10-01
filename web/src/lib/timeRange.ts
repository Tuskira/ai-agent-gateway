/** Time-range quick picks shared by the Access Logs and LLM Logs filter
 * bars — selecting one sets `from`/`to` (ISO) in the URL. */
export type QuickRange = '1h' | '24h' | '7d'

export const QUICK_RANGES: { key: QuickRange; label: string }[] = [
  { key: '1h', label: '1h' },
  { key: '24h', label: '24h' },
  { key: '7d', label: '7d' },
]

const RANGE_MS: Record<QuickRange, number> = {
  '1h': 60 * 60 * 1000,
  '24h': 24 * 60 * 60 * 1000,
  '7d': 7 * 24 * 60 * 60 * 1000,
}

/** `{ from, to }` ISO strings for a quick-pick range, anchored to now. */
export function quickRangeToWindow(range: QuickRange): { from: string; to: string } {
  const to = new Date()
  const from = new Date(to.getTime() - RANGE_MS[range])
  return { from: from.toISOString(), to: to.toISOString() }
}
