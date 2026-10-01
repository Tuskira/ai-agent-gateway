import { ensureSyntaxTree, syntaxTree } from '@codemirror/language'
import type { EditorState } from '@codemirror/state'
import type { SyntaxNode } from '@lezer/common'

export interface FoldRange {
  from: number
  to: number
}

/** Folding is one decoration per range; past this a "collapse" would cost
 * more than the scrolling it saves, so deeper levels are left open. */
const MAX_FOLD_RANGES = 5000

/** How long to wait for a full parse before folding what's parsed so far. */
const PARSE_BUDGET_MS = 2000

const isContainer = (name: string) => name === 'Object' || name === 'Array'

/** The span between a container's brackets, or null when it sits on one
 * line. A body cut short by the capture limit has no closing bracket, so
 * the span runs to the end of what was captured. */
function insideBrackets(node: SyntaxNode, state: EditorState): FoldRange | null {
  const open = node.firstChild
  const close = node.lastChild
  if (!open || !close) return null
  const closed = close !== open && (close.name === '}' || close.name === ']')
  const from = open.to
  const to = closed ? close.from : node.to
  if (state.doc.lineAt(from).number >= state.doc.lineAt(to).number) return null
  return { from, to }
}

/** Fold ranges for every object and array nested under the root, in
 * document order, so a collapsed body reads as its top-level keys and
 * opens one level at a time. When there are more than `maxRanges`, only
 * the shallowest levels that fit are returned. */
export function nestedFoldRanges(
  state: EditorState,
  maxRanges = MAX_FOLD_RANGES,
): FoldRange[] {
  const tree =
    ensureSyntaxTree(state, state.doc.length, PARSE_BUDGET_MS) ?? syntaxTree(state)
  const byLevel: FoldRange[][] = []
  let depth = 0
  tree.iterate({
    enter(node) {
      if (!isContainer(node.name)) return
      depth++
      if (depth === 1) return
      const range = insideBrackets(node.node, state)
      if (range) (byLevel[depth - 2] ??= []).push(range)
    },
    leave(node) {
      if (isContainer(node.name)) depth--
    },
  })

  const ranges: FoldRange[] = []
  for (const level of byLevel) {
    if (!level) continue
    if (ranges.length > 0 && ranges.length + level.length > maxRanges) break
    ranges.push(...level)
  }
  return ranges.sort((a, b) => a.from - b.from)
}
