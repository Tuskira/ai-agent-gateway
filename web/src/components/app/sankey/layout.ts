import type {
  ColumnLayout,
  LayoutOptions,
  PositionedLink,
  PositionedNode,
  SankeyData,
  SankeyLayout,
} from './types'

/**
 * Pure layout for `BalancedSankey`. No React, no DOM — deterministic for a
 * given `(data, options)` so it can be memoised by the renderer and unit
 * tested in isolation.
 */

/** Defaults for the optional `LayoutOptions` knobs. */
export const LAYOUT_DEFAULTS = {
  nodeWidth: 20,
  nodeGap: 4,
  minNodeHeight: 15,
  zeroNodeHeight: 20,
  rightMargin: 150,
  leftMargin: 0,
  labelGap: 8,
  labelPadding: 8,
} as const

/** Result of `allocateColumnHeights`. */
export type ColumnHeights = {
  /** Bar heights in input order. `Σ heights + (n - 1) * gap === height` unless `degenerate`. */
  heights: number[]
  /** True where the bar was inflated to the min height. */
  clamped: boolean[]
  /** Gap actually used (squeezed when `n` bars at min height would not fit). */
  gap: number
  /** True when every bar is clamped (all values zero, or more bars than fit). */
  degenerate: boolean
}

const round2 = (v: number): number => Math.round(v * 100) / 100

/** `v` when it is a finite positive number, else `fallback`. */
const positiveOr = (v: unknown, fallback: number): number =>
  typeof v === 'number' && Number.isFinite(v) && v > 0 ? v : fallback

/**
 * Split `height` across a column's bars in proportion to `values`, inflating
 * tiny bars to `minHeight` (and zero-value bars to `zeroHeight`, so they read
 * as a distinct, deliberate placeholder) and rescaling the rest so bodies +
 * gaps fill the column exactly.
 *
 * Clamping a bar takes more space than it had, which shrinks the remaining
 * bars and may push another one under its floor — so the clamp set is grown
 * iteratively until it is stable (at most `n` rounds). The last free bar
 * absorbs float drift so the column height is exact.
 *
 * If the bars at their floors cannot physically fit, the gap is squeezed
 * first, then both floors are scaled down together — the column never
 * overflows.
 */
export function allocateColumnHeights(
  values: number[],
  height: number,
  gap: number,
  minHeight: number,
  zeroHeight: number = minHeight,
): ColumnHeights {
  const n = values.length
  if (n === 0) return { heights: [], clamped: [], gap, degenerate: false }

  const safe = values.map((v) => (Number.isFinite(v) && v > 0 ? v : 0))
  let g = Math.max(0, gap)
  let m = Math.max(0, minHeight)
  let z = Math.max(0, zeroHeight)
  const floorSum = (): number => safe.reduce((acc, v) => acc + (v > 0 ? m : z), 0)

  if (floorSum() + (n - 1) * g > height) {
    g = n > 1 ? Math.max(0, Math.floor((height - floorSum()) / (n - 1))) : 0
  }
  if (floorSum() + (n - 1) * g > height) {
    const room = Math.max(0, height - (n - 1) * g)
    const scale = floorSum() > 0 ? room / floorSum() : 0
    m *= scale
    z *= scale
  }

  const floors = safe.map((v) => (v > 0 ? m : z))
  const avail = Math.max(0, height - (n - 1) * g)
  const total = safe.reduce((acc, v) => acc + v, 0)
  const heights = floors.slice()
  const clamped = new Array<boolean>(n).fill(false)

  if (total <= 0) {
    return { heights, clamped: clamped.fill(true), gap: g, degenerate: true }
  }

  let remaining = avail
  let freeSum = total
  let clampedSum = 0
  let clampedCount = 0

  for (;;) {
    let changed = false
    for (let i = 0; i < n; i++) {
      if (clamped[i]) continue
      const floor = floors[i] ?? 0
      const h = freeSum > 0 ? ((safe[i] ?? 0) / freeSum) * remaining : 0
      if (h < floor) {
        clamped[i] = true
        heights[i] = floor
        clampedSum += floor
        clampedCount++
        changed = true
      }
    }
    if (!changed) break
    remaining = avail - clampedSum
    freeSum = safe.reduce((acc, v, i) => (clamped[i] ? acc : acc + v), 0)
    if (freeSum <= 0 || remaining <= 0) {
      for (let i = 0; i < n; i++) {
        if (!clamped[i]) {
          clamped[i] = true
          heights[i] = floors[i] ?? 0
          clampedCount++
        }
      }
      break
    }
  }

  if (clampedCount === n) return { heights, clamped, gap: g, degenerate: true }

  const free: number[] = []
  for (let i = 0; i < n; i++) if (!clamped[i]) free.push(i)
  let acc = 0
  free.forEach((i, k) => {
    const h =
      k < free.length - 1 ? ((safe[i] ?? 0) / freeSum) * remaining : remaining - acc
    heights[i] = h
    acc += h
  })

  return { heights, clamped, gap: g, degenerate: false }
}

/**
 * Closed cubic-bezier band from `(x0, sy0..sy1)` to `(x1, ty0..ty1)`.
 * Thickness may differ at each end, so ribbons taper between bars whose
 * heights were clamped. `curvature` 0.5 puts both control points at the
 * horizontal midpoint (same feel as d3-sankey's horizontal link).
 */
export function ribbonPath(
  x0: number,
  sy0: number,
  sy1: number,
  x1: number,
  ty0: number,
  ty1: number,
  curvature = 0.5,
): string {
  const dx = x1 - x0
  const c1 = round2(x0 + dx * curvature)
  const c2 = round2(x1 - dx * curvature)
  const [X0, X1, SY0, SY1, TY0, TY1] = [x0, x1, sy0, sy1, ty0, ty1].map(round2)
  return (
    `M${X0},${SY0}` +
    `C${c1},${SY0} ${c2},${TY0} ${X1},${TY0}` +
    `L${X1},${TY1}` +
    `C${c2},${TY1} ${c1},${SY1} ${X0},${SY1}` +
    'Z'
  )
}

/**
 * Full layout: column x positions, per-column height allocation, node y
 * stacking in input order, link stacking on both ends, and ribbon paths.
 *
 * Nodes are grouped by column preserving `data.nodes` order. Links are
 * ordered on each node by the other end's y position (outgoing by target
 * y, incoming by source y) to reduce crossings. Link thickness on each
 * side is proportional to that side's *rendered* (possibly clamped) bar
 * height, so ribbons stay flush with both bars.
 */
export function computeLayout(data: SankeyData, opts: LayoutOptions): SankeyLayout {
  const nodeWidth = opts.nodeWidth ?? LAYOUT_DEFAULTS.nodeWidth
  const nodeGap = opts.nodeGap ?? LAYOUT_DEFAULTS.nodeGap
  const minNodeHeight = opts.minNodeHeight ?? LAYOUT_DEFAULTS.minNodeHeight
  const zeroNodeHeight = opts.zeroNodeHeight ?? LAYOUT_DEFAULTS.zeroNodeHeight
  const rightMargin = opts.rightMargin ?? LAYOUT_DEFAULTS.rightMargin
  const leftMargin = opts.leftMargin ?? LAYOUT_DEFAULTS.leftMargin
  const labelGap = opts.labelGap ?? LAYOUT_DEFAULTS.labelGap
  const labelPadding = opts.labelPadding ?? LAYOUT_DEFAULTS.labelPadding

  const width = Math.max(0, opts.width)
  const height = Math.max(0, opts.height)
  // Tolerate wrong-typed input: a non-array degrades to "nothing to draw".
  const columnList = Array.isArray(data.columns) ? data.columns : []
  const nodeList = Array.isArray(data.nodes) ? data.nodes : []
  const linkList = Array.isArray(data.links) ? data.links : []
  const n = columnList.length

  // Bar width resolves node → column → layout default. Each column reserves
  // its widest bar so column spacing and the right margin never overlap it.
  const columnBaseWidth = columnList.map((c) => positiveOr(c?.width, nodeWidth))
  const columnWidths = columnList.map((c, i) =>
    nodeList.reduce(
      (max, nd) =>
        nd && nd.column === c.id
          ? Math.max(max, positiveOr(nd.width, columnBaseWidth[i] ?? nodeWidth))
          : max,
      columnBaseWidth[i] ?? nodeWidth,
    ),
  )
  const lastColumnWidth = columnWidths[n - 1] ?? nodeWidth
  const innerW = Math.max(0, width - leftMargin - rightMargin - lastColumnWidth)

  const columns: ColumnLayout[] = columnList.map((c, i) => {
    const x0 = leftMargin + (n > 1 ? (i / (n - 1)) * innerW : 0)
    const x1 = x0 + (columnWidths[i] ?? nodeWidth)
    return {
      ...c,
      index: i,
      x0,
      x1,
      labelX: x1 + labelGap,
      labelWidth: 0,
      contentHeight: 0,
      degenerate: false,
      gap: nodeGap,
    }
  })
  // Right-side labels may run up to the next column (minus padding) or to
  // the chart's right edge; left-side labels run back to the previous
  // column's bars (plus padding) or to the chart's left edge.
  const labelLimit = (i: number): number => {
    const next = columns[i + 1]
    return next ? next.x0 - labelPadding : width
  }
  const leftLimit = (i: number): number => {
    const prev = i > 0 ? columns[i - 1] : undefined
    return prev ? prev.x1 + labelPadding : 0
  }
  const labelsLeft = (i: number): boolean => columnList[i]?.labelSide === 'left'
  columns.forEach((c, i) => {
    if (labelsLeft(i)) {
      c.labelWidth = Math.max(0, c.x0 - labelGap - leftLimit(i))
      c.labelX = c.x0 - labelGap - c.labelWidth
    } else {
      c.labelWidth = Math.max(0, labelLimit(i) - c.labelX)
    }
  })

  const nodes: PositionedNode[] = []
  const nodeByKey = new Map<string, PositionedNode>()
  const seen = new Set<string>()
  columns.forEach((col) => {
    // First occurrence of a key wins; later duplicates are dropped so links
    // resolve to one bar and React keys stay unique.
    const colNodes = nodeList.filter((nd) => {
      if (!nd || nd.column !== col.id || seen.has(nd.key)) return false
      seen.add(nd.key)
      return true
    })
    const alloc = allocateColumnHeights(
      colNodes.map((nd) => nd.value),
      height,
      nodeGap,
      minNodeHeight,
      zeroNodeHeight,
    )
    col.gap = alloc.gap
    col.degenerate = alloc.degenerate
    let y = 0
    colNodes.forEach((nd, j) => {
      const h = alloc.heights[j] ?? 0
      const x1 = col.x0 + positiveOr(nd.width, columnBaseWidth[col.index] ?? nodeWidth)
      const labelWidth = labelsLeft(col.index)
        ? col.labelWidth
        : Math.max(0, labelLimit(col.index) - (x1 + labelGap))
      const labelX = labelsLeft(col.index) ? col.labelX : x1 + labelGap
      const positioned: PositionedNode = {
        ...nd,
        columnIndex: col.index,
        index: j,
        x0: col.x0,
        x1,
        y0: y,
        y1: y + h,
        height: h,
        clamped: alloc.clamped[j] ?? false,
        labelX,
        labelWidth,
      }
      nodes.push(positioned)
      nodeByKey.set(nd.key, positioned)
      y += h + alloc.gap
    })
    col.contentHeight = colNodes.length > 0 ? y - alloc.gap : 0
  })

  const links: PositionedLink[] = []
  linkList.forEach((l, index) => {
    if (!l) return
    const sourceNode = nodeByKey.get(l.source)
    const targetNode = nodeByKey.get(l.target)
    // Only left-to-right flows between resolved bars are drawn: dangling,
    // zero, self-loop and backward links are skipped.
    if (!sourceNode || !targetNode || !(l.value > 0)) return
    if (targetNode.columnIndex <= sourceNode.columnIndex) return
    links.push({
      ...l,
      index,
      sourceNode,
      targetNode,
      sy0: 0,
      sy1: 0,
      ty0: 0,
      ty1: 0,
      path: '',
    })
  })

  const outgoing = new Map<string, PositionedLink[]>()
  const incoming = new Map<string, PositionedLink[]>()
  links.forEach((l) => {
    outgoing.set(l.source, [...(outgoing.get(l.source) ?? []), l])
    incoming.set(l.target, [...(incoming.get(l.target) ?? []), l])
  })

  nodes.forEach((node) => {
    const out = (outgoing.get(node.key) ?? []).sort(
      (a, b) => a.targetNode.y0 - b.targetNode.y0 || a.index - b.index,
    )
    const outDenom = Math.max(
      node.value,
      out.reduce((acc, l) => acc + l.value, 0),
    )
    let cursor = node.y0
    out.forEach((l) => {
      const t = outDenom > 0 ? (l.value / outDenom) * node.height : 0
      l.sy0 = cursor
      l.sy1 = cursor + t
      cursor += t
    })

    const inc = (incoming.get(node.key) ?? []).sort(
      (a, b) => a.sourceNode.y0 - b.sourceNode.y0 || a.index - b.index,
    )
    const inDenom = Math.max(
      node.value,
      inc.reduce((acc, l) => acc + l.value, 0),
    )
    cursor = node.y0
    inc.forEach((l) => {
      const t = inDenom > 0 ? (l.value / inDenom) * node.height : 0
      l.ty0 = cursor
      l.ty1 = cursor + t
      cursor += t
    })
  })

  links.forEach((l) => {
    l.path = ribbonPath(l.sourceNode.x1, l.sy0, l.sy1, l.targetNode.x0, l.ty0, l.ty1)
  })

  return { width, height, columns, nodes, links, nodeByKey }
}
