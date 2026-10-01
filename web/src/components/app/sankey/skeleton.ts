import type { SankeyColumn, SankeyData, SankeyLink, SankeyNode } from './types'

/**
 * Placeholder dataset for the loading state. Runs through the same layout
 * as real data, so the skeleton has the exact geometry of the diagram it
 * stands in for: full-height columns, tapered ribbons, label stubs.
 */

/** Relative bar weights per skeleton column, cycled by column index. */
const SKELETON_STACKS = [
  [3, 2, 2, 1, 1, 1],
  [4, 2],
  [3, 2, 1],
  [2, 2, 1, 1],
  [3, 2, 1],
  [2, 1, 1, 1],
  [1, 2, 2, 1],
]

/** Every skeleton column sums to this, so each fills the chart height. */
const SKELETON_TOTAL = 100

/** Small deterministic PRNG (mulberry32) — identical ribbons on every render. */
const mulberry32 = (seed: number) => (): number => {
  seed = (seed + 0x6d2b79f5) | 0
  let t = Math.imul(seed ^ (seed >>> 15), 1 | seed)
  t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296
}

/**
 * Build a balanced placeholder dataset for `columns`: fixed bar proportions
 * per column plus pseudo-random ribbons between adjacent columns. Each bar
 * fans out to one to three targets (always including the largest one so big
 * bars stay connected) with random weights that conserve the bar's value.
 *
 * Seeded, so the pattern is stable across renders and resizes — the skeleton
 * never flickers while data loads.
 */
export const buildSkeletonData = (columns: SankeyColumn[], seed = 7): SankeyData => {
  const rand = mulberry32(seed)
  const nodes: SankeyNode[] = []
  const cols = Array.isArray(columns) ? columns : []

  const perColumn: SankeyNode[][] = cols.map((c, i) => {
    const weights = SKELETON_STACKS[i % SKELETON_STACKS.length] ?? [1]
    const sum = weights.reduce((acc, w) => acc + w, 0)
    const colNodes = weights.map((w, j): SankeyNode => ({
      key: `skeleton:${c.id}:${j}`,
      column: c.id,
      label: '',
      value: (w / sum) * SKELETON_TOTAL,
      percent: (w / sum) * 100,
    }))
    nodes.push(...colNodes)
    return colNodes
  })

  const links: SankeyLink[] = []
  for (let i = 0; i < perColumn.length - 1; i++) {
    const sources = perColumn[i] ?? []
    const targets = perColumn[i + 1] ?? []
    if (targets.length === 0) continue
    const largest = targets.reduce(
      (best, t, idx) => (t.value > (targets[best]?.value ?? 0) ? idx : best),
      0,
    )
    sources.forEach((source) => {
      const picks = new Set<number>([largest])
      const count = 1 + (rand() < 0.65 ? 1 : 0) + (rand() < 0.35 ? 1 : 0)
      while (picks.size < Math.min(count, targets.length)) {
        picks.add(Math.floor(rand() * targets.length))
      }
      const chosen = [...picks]
      const weights = chosen.map(() => 0.4 + rand())
      const wSum = weights.reduce((acc, w) => acc + w, 0)
      chosen.forEach((t, k) => {
        const target = targets[t]
        if (!target) return
        const value = ((weights[k] ?? 0) / wSum) * source.value
        links.push({
          source: source.key,
          target: target.key,
          value,
          percent: (value / source.value) * 100,
        })
      })
    })
  }

  return { columns: cols, nodes, links, total: SKELETON_TOTAL }
}
