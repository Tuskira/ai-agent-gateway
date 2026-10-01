import type { PillTone } from '@/components/app/StatusPill'

/** Status pill color per the Access/LLM logs mock: green 200, blue 204, red
 * 4xx/5xx, neutral for anything else. */
export function httpStatusTone(status: number): PillTone {
  if (status === 200) return 'positive'
  if (status === 204) return 'info'
  if (status >= 400) return 'negative'
  return 'neutral'
}
