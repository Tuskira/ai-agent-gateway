import type { ReactNode } from 'react'

/**
 * Data + layout types for `BalancedSankey`.
 *
 * "Balanced" means every column is drawn edge to edge, so the diagram forms
 * a rectangle. Each column is scaled against its OWN sum, so a dataset whose
 * columns don't all sum to `total` still renders — the columns simply use
 * different pixels-per-unit, and the ribbons taper across the change. Node
 * `percent` stays a share of `total`, so in such a dataset a bar's label and
 * its share of the column height won't agree.
 */

/** One vertical stage of the diagram. Array order = left-to-right render order. */
export type SankeyColumn = {
  /** Matches `SankeyNode.column`. */
  id: string
  /** Header text rendered above the column. */
  label: string
  /** Optional second header line under `label` — a quieter qualifier such as
   *  the system that produced the column. Any column carrying one switches
   *  the whole header row to its two-line form (title, sub-label, rule), so
   *  headers stay aligned across columns. Takes a node, so a consumer can
   *  supply its own badge instead of plain text; the slot is one 16px line,
   *  so keep it to that height. */
  subLabel?: ReactNode
  /** Optional element rendered inline after the header text (e.g. a filter button). */
  headerSlot?: ReactNode
  /** Bar width for this column in px. Overrides `LayoutOptions.nodeWidth`; a node's own `width` overrides this. */
  width?: number
  /**
   * Which side of the bar its labels sit on. Default `'right'`. A `'left'`
   * column lays its labels in the space before it -- `LayoutOptions.leftMargin`
   * for the first column, the gap after the previous column's bars otherwise
   * -- with the text end-aligned against the bar.
   */
  labelSide?: 'left' | 'right'
}

/** A bar in a column. Identity is `key`; labels may repeat across columns. */
export type SankeyNode = {
  /** Unique across the whole diagram — links reference this. */
  key: string
  /** `SankeyColumn.id` this node belongs to. Nodes with an unknown column are dropped. */
  column: string
  /** Display text. */
  label: string
  /** Absolute count. Zero is legitimate and still rendered (min height, dashed). */
  value: number
  /** Share of `SankeyData.total`, unrounded (0–100). */
  percent: number
  /** Optional machine code for consumer-side styling (e.g. colour mapping). */
  code?: string
  /** Bar width for this node in px. Overrides the column's `width` and `LayoutOptions.nodeWidth`. */
  width?: number
}

/** A ribbon between two nodes. */
export type SankeyLink = {
  /** `SankeyNode.key` of the left end. */
  source: string
  /** `SankeyNode.key` of the right end. */
  target: string
  /** Absolute count. Zero-value links are not drawn. */
  value: number
  /** Share of the SOURCE node's value, unrounded (0–100). */
  percent: number
}

/** A dataset to draw. Columns usually — but need not — sum to `total`. */
export type SankeyData = {
  columns: SankeyColumn[]
  /** Must already be in render order (top to bottom within a column); the layout never sorts. */
  nodes: SankeyNode[]
  links: SankeyLink[]
  /** Population node `percent` is a share of. Zero is a valid empty state. */
  total: number
}

/** Geometry knobs for `computeLayout`. All lengths in px. */
export type LayoutOptions = {
  /** Measured container width. */
  width: number
  /** Chart area height (headers are rendered outside of it). */
  height: number
  /** Default bar width. Default 20. Columns and nodes may override it via their own `width`. */
  nodeWidth?: number
  /** Vertical gap between bars in a column. Default 4. */
  nodeGap?: number
  /** Bars thinner than this are inflated to it. Default 15. */
  minNodeHeight?: number
  /** Height of zero-value bars, so they stay distinct from tiny non-zero ones. Default 20. */
  zeroNodeHeight?: number
  /** Space reserved right of the last column for its labels. Default 150. */
  rightMargin?: number
  /** Space left of the first column. Default 0. */
  leftMargin?: number
  /** Gap between a bar's right edge and its label. Default 8. */
  labelGap?: number
  /** Breathing room kept between a label block and the next column. Default 8. */
  labelPadding?: number
}

/** A column with its resolved x range and label geometry. */
export type ColumnLayout = SankeyColumn & {
  /** Position in `SankeyData.columns`. */
  index: number
  /** Left edge of the bars. */
  x0: number
  /** Right edge of the column's widest bar. Individual bars may end before this. */
  x1: number
  /** Left edge of the label block for the column's widest bar (per-node values live on `PositionedNode`). For a `labelSide: 'left'` column this is the block's left edge too -- it ends `labelGap` before the bar. */
  labelX: number
  /** Max label width before it would collide with the next column (or the right margin). */
  labelWidth: number
  /** Stack height actually used. Equals `height` unless `degenerate`. */
  contentHeight: number
  /** True when every node was clamped (e.g. `total` 0) and the column cannot fill `height`. */
  degenerate: boolean
  /** Gap used after any squeeze (see `allocateColumnHeights`). */
  gap: number
}

/** A node with its resolved geometry. */
export type PositionedNode = SankeyNode & {
  /** Index of the owning column. */
  columnIndex: number
  /** Position within its column, top to bottom. */
  index: number
  x0: number
  x1: number
  y0: number
  y1: number
  /** `y1 - y0`. */
  height: number
  /** True when the height was inflated to `minNodeHeight`. */
  clamped: boolean
  /** Left edge of this bar's label block: `x1 + labelGap`, or for a left-labelled column `x0 - labelGap - labelWidth`. */
  labelX: number
  /** Max width of this bar's label block before the next column (or the chart edge on that side). */
  labelWidth: number
}

/** A drawable link with its resolved geometry. */
export type PositionedLink = SankeyLink & {
  /** Index into `SankeyData.links`; stable id for gradient defs. */
  index: number
  sourceNode: PositionedNode
  targetNode: PositionedNode
  /** Y range on the source node's right edge. Thickness = `sy1 - sy0`. */
  sy0: number
  sy1: number
  /** Y range on the target node's left edge. Thickness = `ty1 - ty0`. */
  ty0: number
  ty1: number
  /** Closed SVG path for the tapered ribbon. */
  path: string
}

/** Output of `computeLayout`. */
export type SankeyLayout = {
  width: number
  height: number
  columns: ColumnLayout[]
  nodes: PositionedNode[]
  /** Only links with `value > 0` whose endpoints both resolved. */
  links: PositionedLink[]
  nodeByKey: Map<string, PositionedNode>
}
