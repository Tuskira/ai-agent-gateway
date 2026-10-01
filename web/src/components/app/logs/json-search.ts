import type { SearchQuery } from '@codemirror/search'
import type { EditorState } from '@codemirror/state'

export interface MatchPosition {
  total: number
  /** 1-based index of the selected match, or 0 when none is selected. */
  current: number
  /** True when counting stopped at the cap, so `total` is a lower bound. */
  capped: boolean
}

/** Counting runs on every keystroke; a common term in a multi-MB body
 * would otherwise walk tens of thousands of matches each time. */
const MAX_COUNTED = 1000

/** How many times `query` matches, and which of them the selection is on. */
export function matchPosition(
  state: EditorState,
  query: SearchQuery,
  cap = MAX_COUNTED,
): MatchPosition {
  const { from, to } = state.selection.main
  let total = 0
  let current = 0
  const cursor = query.getCursor(state)
  for (let match = cursor.next(); !match.done; match = cursor.next()) {
    if (total === cap) return { total, current, capped: true }
    total++
    if (match.value.from === from && match.value.to === to) current = total
  }
  return { total, current, capped: false }
}
