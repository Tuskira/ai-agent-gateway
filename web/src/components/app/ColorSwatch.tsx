interface ColorSwatchProps {
  color: string
  size?: number
  className?: string
}

/** A small colored square, drawn as an inline SVG rect (not a `style=`
 * background) so per-series colors from `tokens.ts` never require inline
 * styles. Used by chart legends and tooltips. */
export function ColorSwatch({ color, size = 10, className }: ColorSwatchProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 10 10"
      role="presentation"
      className={className ?? 'shrink-0'}
    >
      <rect width={10} height={10} rx={3} fill={color} />
    </svg>
  )
}
