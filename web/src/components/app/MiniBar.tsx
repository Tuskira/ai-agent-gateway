interface MiniBarProps {
  /** 0–100. */
  pct: number
  /** CSS color (hex or `var(--token)`) for the filled portion. */
  color: string
  /** Bar thickness in px — 8 for the compact lists, 12 for the hero rows. */
  heightPx?: 8 | 12
}

/**
 * A single horizontal ranked-bar track, rendered as an SVG rect pair
 * (muted track + colored fill). The fill's `width` attribute is a
 * percentage, which keeps the dynamic, per-row bar width out of the `style`
 * prop entirely.
 *
 * There is deliberately no `viewBox`: user units then equal CSS pixels, so
 * `rx` gives circular end caps at any bar width. A stretched viewBox scales
 * `rx` horizontally only and turns the caps into long ellipses.
 *
 * The fill wipes in on mount and eases to a new width when the data
 * changes (`.bar-fill` in index.css, off under reduced motion).
 */
export function MiniBar({ pct, color, heightPx = 8 }: MiniBarProps) {
  const r = heightPx / 2
  const clamped = Math.min(100, Math.max(0, pct))
  return (
    <svg className="block h-full w-full" role="presentation">
      <rect
        x={0}
        y={0}
        width="100%"
        height={heightPx}
        rx={r}
        className="fill-[var(--bg-muted)]"
      />
      <rect
        x={0}
        y={0}
        width={`${clamped}%`}
        height={heightPx}
        rx={r}
        fill={color}
        className="bar-fill"
      />
    </svg>
  )
}
