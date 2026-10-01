/**
 * Shared defaults for `BalancedSankey`. Kept in a `.ts` module so the
 * component file only exports components (react-refresh requirement).
 */

/** Categorical fallback palette, cycled by column index when no colour is supplied. */
export const DEFAULT_PALETTE = [
  '#6d83e8',
  '#0ea5e9',
  '#14b8a6',
  '#8b5cf6',
  '#22c55e',
  '#06b6d4',
  '#f59e0b',
  '#ec4899',
]

/** Neutral colour for nodes the palette cannot resolve (empty palette). */
export const FALLBACK_NODE_COLOR = '#a1a1aa'

/** Ribbon opacity when nothing is hovered. */
export const DEFAULT_LINK_OPACITY = 0.45
/** Ribbon opacity for the hovered ribbon / ribbons incident to the hovered bar. */
export const DEFAULT_LINK_HOVER_OPACITY = 0.85
/** Ribbon opacity for everything else while something is hovered. */
export const DEFAULT_LINK_DIM_OPACITY = 0.12
