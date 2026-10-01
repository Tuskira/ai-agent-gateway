import {
  DEFAULT_LINK_DIM_OPACITY,
  DEFAULT_LINK_HOVER_OPACITY,
  DEFAULT_LINK_OPACITY,
  DEFAULT_PALETTE,
  FALLBACK_NODE_COLOR,
} from './defaults'
import { computeLayout } from './layout'
import { buildSkeletonData } from './skeleton'
import type {
  LayoutOptions,
  PositionedLink,
  PositionedNode,
  SankeyData,
  SankeyLink,
  SankeyNode,
} from './types'
import { useMeasuredSize } from './useMeasuredSize'
import { cn } from '@/lib/utils'
import {
  type CSSProperties,
  type KeyboardEvent as ReactKeyboardEvent,
  type MouseEvent as ReactMouseEvent,
  type ReactNode,
  useCallback,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from 'react'

/** Props for {@link BalancedSankey}. */
export type BalancedSankeyProps = {
  /** Balanced dataset — `columns` order is left-to-right, `nodes` order is top-to-bottom. */
  data: SankeyData
  /**
   * Fixed chart-area height in px (headers render above it). Omit to fill
   * the parent instead — the parent then needs a definite height, e.g. a
   * flex child with `min-h-0`; otherwise `minHeight` is used.
   */
  height?: number
  /** Chart-area height used when `height` is omitted and the parent has no definite height. Default 240. */
  minHeight?: number
  /** Below this width the chart stops shrinking and scrolls horizontally instead. Default 0 (fully fluid). */
  minWidth?: number
  /** Render the header row plus a pulse skeleton (placeholder bars and ribbons) instead of the diagram. Keeps `headerSlot`s mounted. */
  loading?: boolean
  /** Render a greyed-out placeholder diagram with `emptyMessage` over it. `loading` takes precedence. Keeps `headerSlot`s mounted. */
  empty?: boolean
  /** Message shown over the placeholder when `empty`. Default "No data found". */
  emptyMessage?: ReactNode
  /**
   * Node colour resolver; also feeds the ribbon gradient on that side.
   * Receives the positioned node, so `index` / `columnIndex` are available
   * for shade progressions. Return `undefined` to fall through to
   * `nodeColors`, then `palette`.
   */
  getNodeColor?: (node: PositionedNode) => string | undefined
  /** Static colour map keyed by node `key` first, then by column `id`. */
  nodeColors?: Record<string, string>
  /**
   * Ribbon colour resolver. Return one colour for a flat ribbon or a
   * `[from, to]` pair for a gradient; `undefined` falls back to a gradient
   * from the source bar's colour to the target bar's. Lets a diagram keep
   * neutral bars while its ribbons carry the flow's identity.
   */
  getLinkColors?: (link: PositionedLink) => string | [string, string] | undefined
  /** Categorical fallback palette, cycled by column index. Default {@link DEFAULT_PALETTE}. */
  palette?: string[]
  /** Count formatter for labels + tooltips. Default `String`. */
  formatValue?: (value: number) => string
  /** Percent formatter (input 0–100). Default one decimal place. */
  formatPercent?: (percent: number) => string
  /** Inline style for numeric spans (e.g. a mono font). Default tabular numerals. */
  numberStyle?: CSSProperties
  /** Show the count in node labels. Default true. */
  showValue?: boolean
  /** Show the percent-of-total in node labels. Default true. */
  showPercent?: boolean
  /** Replace the default label row (label · count · percent) for a node. */
  renderNodeLabel?: (node: PositionedNode) => ReactNode
  /** Optional icon rendered before the label text in the default label row (e.g. a vendor logo). */
  renderNodeIcon?: (node: PositionedNode) => ReactNode
  /** Enable hover tooltips. Default true. */
  tooltips?: boolean
  /** Replace the default node tooltip body. */
  renderNodeTooltip?: (node: PositionedNode) => ReactNode
  /** Replace the default link tooltip body. */
  renderLinkTooltip?: (link: PositionedLink) => ReactNode
  /** Classes for the tooltip surface (background, border, text colour). */
  tooltipClassName?: string
  /** Inline style for the tooltip surface (e.g. a box shadow). */
  tooltipStyle?: CSSProperties
  /** Makes bars clickable (pointer cursor, keyboard-activatable). */
  onNodeClick?: (node: SankeyNode) => void
  /** Makes ribbons clickable. */
  onLinkClick?: (link: SankeyLink) => void
  /** With `onNodeClick` set, limits which bars are clickable (cursor, focus, activation). Default: every bar. */
  isNodeClickable?: (node: PositionedNode) => boolean
  /** Overrides for the layout geometry (bar width, gaps, min height, margins). */
  layout?: Omit<LayoutOptions, 'width' | 'height'>
  /** Ribbon opacity when nothing is hovered. Default 0.45. */
  linkOpacity?: number
  /** Opacity of the hovered ribbon / ribbons incident to the hovered bar. Default 0.85. */
  linkHoverOpacity?: number
  /** Opacity of the other ribbons while something is hovered. Default 0.12. */
  linkDimOpacity?: number
  /** Render the column header row. Default true. */
  showHeaders?: boolean
  /** Extra classes for each column header. */
  headerClassName?: string
  /** Extra classes for the outer wrapper. */
  className?: string
  /** Accessible name for the diagram. Default "Flow diagram". */
  ariaLabel?: string
}

type Hover = { kind: 'node'; key: string } | { kind: 'link'; index: number } | null

const HEADER_HEIGHT = 24
/** Header height when any column carries a `subLabel`: title line, sub-label, rule. */
const HEADER_HEIGHT_WITH_SUBLABEL = 44
const HEADER_GAP = 4
const LABEL_HEIGHT = 16
/** Below this label width only the count is shown. */
const MIN_FULL_LABEL_WIDTH = 48
const TOOLTIP_OFFSET = 12
const DEFAULT_NUMBER_STYLE: CSSProperties = { fontVariantNumeric: 'tabular-nums' }
/** Skeleton label stub: max width and height of the rounded placeholder bar. */
const SKELETON_LABEL_WIDTH = 64
const SKELETON_LABEL_HEIGHT = 8

const defaultFormatPercent = (p: number): string => `${p.toFixed(1)}%`

const isActivationKey = (e: ReactKeyboardEvent): boolean =>
  e.key === 'Enter' || e.key === ' '

/**
 * Balanced Sankey: every column fills the chart height edge to edge, nodes
 * keep their input order, tiny nodes are held at a minimum height, and
 * ribbons taper so they stay flush with both bars. Column headers are HTML
 * positioned from the same layout numbers as the bars, so they can't drift.
 *
 * Responsive: width follows the container (fluid down to `minWidth`, then
 * horizontal scroll); height is fixed via `height` or fills the parent.
 * Hover dimming is React state, but the tooltip follows the pointer via a
 * direct transform update so mouse moves never re-render the SVG.
 */
export const BalancedSankey = ({
  data,
  height,
  minHeight = 240,
  minWidth = 0,
  loading = false,
  empty = false,
  emptyMessage = 'No data found',
  getNodeColor,
  nodeColors,
  getLinkColors,
  palette = DEFAULT_PALETTE,
  formatValue = String,
  formatPercent = defaultFormatPercent,
  numberStyle = DEFAULT_NUMBER_STYLE,
  showValue = true,
  showPercent = true,
  renderNodeLabel,
  renderNodeIcon,
  tooltips = true,
  renderNodeTooltip,
  renderLinkTooltip,
  tooltipClassName,
  tooltipStyle,
  onNodeClick,
  onLinkClick,
  isNodeClickable,
  layout,
  linkOpacity = DEFAULT_LINK_OPACITY,
  linkHoverOpacity = DEFAULT_LINK_HOVER_OPACITY,
  linkDimOpacity = DEFAULT_LINK_DIM_OPACITY,
  showHeaders = true,
  headerClassName,
  className,
  ariaLabel = 'Flow diagram',
}: BalancedSankeyProps) => {
  const { ref, width: measuredWidth, height: measuredHeight } = useMeasuredSize()
  const rawId = useId()
  const uid = useMemo(() => rawId.replace(/[^a-zA-Z0-9_-]/g, ''), [rawId])
  const [hover, setHover] = useState<Hover>(null)
  const tooltipRef = useRef<HTMLDivElement | null>(null)
  const lastPointer = useRef<{ x: number; y: number } | null>(null)

  // ── Dimensions ────────────────────────────────────────────────────────────
  const fillHeight = height === undefined
  // One column with a `subLabel` switches every header to the two-line form,
  // so the title baselines stay level across the row.
  const hasSubLabels = data.columns.some((c) => Boolean(c.subLabel))
  const headerHeight = hasSubLabels ? HEADER_HEIGHT_WITH_SUBLABEL : HEADER_HEIGHT
  const headerSpace = showHeaders ? headerHeight + HEADER_GAP : 0
  const chartHeight = fillHeight
    ? Math.max(minHeight, measuredHeight - headerSpace)
    : Math.max(0, height)
  const chartWidth = Math.max(measuredWidth, minWidth)
  const scrolls = chartWidth > measuredWidth
  const ready = measuredWidth > 0

  // ── Layout ────────────────────────────────────────────────────────────────
  const computed = useMemo(
    () =>
      ready
        ? computeLayout(data, { ...layout, width: chartWidth, height: chartHeight })
        : null,
    [ready, data, layout, chartWidth, chartHeight],
  )

  // Skeleton shares the layout engine so its bars and ribbons sit exactly
  // where real ones will. Seeded data → stable across renders and resizes.
  const skeletonData = useMemo(() => buildSkeletonData(data.columns), [data.columns])
  const placeholder = loading || empty
  const skeleton = useMemo(
    () =>
      placeholder && ready
        ? computeLayout(skeletonData, {
            ...layout,
            width: chartWidth,
            height: chartHeight,
          })
        : null,
    [placeholder, ready, skeletonData, layout, chartWidth, chartHeight],
  )

  // ── Colours ───────────────────────────────────────────────────────────────
  const colorOf = useCallback(
    (n: PositionedNode): string =>
      getNodeColor?.(n) ??
      nodeColors?.[n.key] ??
      nodeColors?.[n.column] ??
      palette[n.columnIndex % Math.max(1, palette.length)] ??
      FALLBACK_NODE_COLOR,
    [getNodeColor, nodeColors, palette],
  )

  const linkColorsOf = useCallback(
    (l: PositionedLink): [string, string] => {
      const custom = getLinkColors?.(l)
      if (typeof custom === 'string') return [custom, custom]
      if (custom) return custom
      return [colorOf(l.sourceNode), colorOf(l.targetNode)]
    },
    [getLinkColors, colorOf],
  )

  // ── Hover + tooltip ───────────────────────────────────────────────────────
  // Position is written straight to the tooltip element's transform so the
  // SVG only re-renders on enter/leave (dimming), never on mouse move.
  const place = useCallback(
    (clientX: number, clientY: number) => {
      lastPointer.current = { x: clientX, y: clientY }
      const el = tooltipRef.current
      const host = ref.current
      if (!el || !host) return
      const r = host.getBoundingClientRect()
      const lx = clientX - r.left
      const ly = clientY - r.top
      const x = lx + host.scrollLeft
      const y = ly + host.scrollTop
      const w = el.offsetWidth
      const h = el.offsetHeight
      const left =
        lx + TOOLTIP_OFFSET + w > r.width ? x - w - TOOLTIP_OFFSET : x + TOOLTIP_OFFSET
      const top =
        ly - TOOLTIP_OFFSET - h < 0 ? y + TOOLTIP_OFFSET : y - TOOLTIP_OFFSET - h
      el.style.transform = `translate(${Math.max(0, Math.round(left))}px, ${Math.max(0, Math.round(top))}px)`
    },
    [ref],
  )
  // First placement after the tooltip mounts (sizes are only known then).
  useLayoutEffect(() => {
    if (hover && lastPointer.current) place(lastPointer.current.x, lastPointer.current.y)
  }, [hover, place])

  const onMove = useCallback(
    (e: ReactMouseEvent) => {
      if (tooltipRef.current) place(e.clientX, e.clientY)
    },
    [place],
  )
  const onLeave = useCallback(() => setHover(null), [])
  // Hover always drives ribbon dimming; `tooltips` only gates the tooltip body.
  const enterNode = (key: string) => (e: ReactMouseEvent) => {
    setHover({ kind: 'node', key })
    place(e.clientX, e.clientY)
  }
  const enterLink = (index: number) => (e: ReactMouseEvent) => {
    setHover({ kind: 'link', index })
    place(e.clientX, e.clientY)
  }

  const linkOpacityFor = (l: PositionedLink): number => {
    if (!hover) return linkOpacity
    if (hover.kind === 'link')
      return hover.index === l.index ? linkHoverOpacity : linkDimOpacity
    return l.source === hover.key || l.target === hover.key
      ? linkHoverOpacity
      : linkDimOpacity
  }

  let tooltipBody: ReactNode = null
  if (tooltips && hover && computed && !placeholder) {
    if (hover.kind === 'node') {
      const node = computed.nodeByKey.get(hover.key)
      if (node) {
        tooltipBody = renderNodeTooltip ? (
          renderNodeTooltip(node)
        ) : (
          <div className="flex flex-col gap-1">
            <div className="flex items-center gap-2">
              <span
                aria-hidden
                className="size-2.5 shrink-0 rounded-[3px]"
                style={{ backgroundColor: colorOf(node) }}
              />
              <span className="text-foreground font-semibold">{node.label}</span>
            </div>
            <div className="text-muted-foreground">
              <span className="text-foreground font-semibold" style={numberStyle}>
                {formatValue(node.value)}
              </span>
              {' · '}
              <span style={numberStyle}>{formatPercent(node.percent)}</span> of total
            </div>
          </div>
        )
      }
    } else {
      const link = computed.links.find((l) => l.index === hover.index)
      if (link) {
        tooltipBody = renderLinkTooltip ? (
          renderLinkTooltip(link)
        ) : (
          <div className="flex flex-col gap-1">
            <div className="flex items-center gap-1.5">
              <span
                aria-hidden
                className="size-2.5 shrink-0 rounded-[3px]"
                style={{ backgroundColor: linkColorsOf(link)[0] }}
              />
              <span className="text-foreground font-semibold">
                {link.sourceNode.label}
              </span>
              <span className="text-muted-foreground">→</span>
              <span
                aria-hidden
                className="size-2.5 shrink-0 rounded-[3px]"
                style={{ backgroundColor: linkColorsOf(link)[1] }}
              />
              <span className="text-foreground font-semibold">
                {link.targetNode.label}
              </span>
            </div>
            <div className="text-muted-foreground">
              <span className="text-foreground font-semibold" style={numberStyle}>
                {formatValue(link.value)}
              </span>
              {' · '}
              <span style={numberStyle}>{formatPercent(link.percent)}</span> of{' '}
              {link.sourceNode.label}
            </div>
          </div>
        )
      }
    }
  }

  const nodesClickable = Boolean(onNodeClick)
  const linksClickable = Boolean(onLinkClick)

  return (
    <div
      ref={ref}
      className={cn(
        'relative w-full min-w-0',
        scrolls ? 'overflow-x-auto overflow-y-hidden' : 'overflow-visible',
        fillHeight && 'h-full',
        className,
      )}
    >
      {!computed ? (
        <div style={{ height: headerSpace + chartHeight }} />
      ) : (
        <div style={{ width: chartWidth }}>
          {showHeaders && (
            <div
              className="relative"
              style={{
                height: headerHeight,
                width: chartWidth,
                marginBottom: HEADER_GAP,
              }}
            >
              {computed.columns.map((c, i) => {
                const next = computed.columns[i + 1]
                const maxWidth = next ? next.x0 - c.x0 - 6 : chartWidth - c.x0
                return (
                  <div
                    key={c.id}
                    className={cn(
                      'absolute top-0 flex h-full flex-col',
                      // Single-line headers centre in the row as before; the
                      // two-line form stacks from the top so the rule lands at
                      // a consistent height across columns.
                      hasSubLabels ? 'justify-start gap-0.5' : 'justify-center',
                      headerClassName,
                    )}
                    style={{ left: c.x0, maxWidth }}
                  >
                    <div className="text-foreground flex items-center gap-1 text-sm font-bold tracking-wider whitespace-nowrap uppercase">
                      <span className="truncate" title={c.label}>
                        {c.label}
                      </span>
                      {c.headerSlot}
                    </div>
                    {hasSubLabels && (
                      // Rendered even for a column without a sub-label, so the
                      // rule runs unbroken across the header row.
                      <div
                        className="text-muted-foreground border-border/80 min-h-4 truncate border-b pb-1.5 text-xs font-medium"
                        title={typeof c.subLabel === 'string' ? c.subLabel : undefined}
                      >
                        {c.subLabel}
                      </div>
                    )}
                  </div>
                )
              })}
            </div>
          )}

          {placeholder && skeleton ? (
            <div className="relative" style={{ width: chartWidth, height: chartHeight }}>
              <svg
                width={chartWidth}
                height={chartHeight}
                className={cn(
                  'text-muted-foreground block overflow-visible',
                  loading && 'animate-pulse',
                )}
                role="img"
                aria-label={loading ? `Loading ${ariaLabel}` : `${ariaLabel}: no data`}
                aria-busy={loading || undefined}
              >
                <g data-layer="skeleton-links">
                  {skeleton.links.map((l) => (
                    <path
                      key={l.index}
                      d={l.path}
                      fill="currentColor"
                      fillOpacity={0.1}
                    />
                  ))}
                </g>
                <g data-layer="skeleton-nodes">
                  {skeleton.nodes.map((n) => (
                    <rect
                      key={n.key}
                      x={n.x0}
                      y={n.y0}
                      width={n.x1 - n.x0}
                      height={n.height}
                      rx={2}
                      fill="currentColor"
                      fillOpacity={0.25}
                    />
                  ))}
                </g>
                <g data-layer="skeleton-labels">
                  {skeleton.nodes.map((n) =>
                    n.labelWidth > 0 && n.height >= SKELETON_LABEL_HEIGHT ? (
                      <rect
                        key={n.key}
                        x={n.labelX}
                        y={n.y0 + n.height / 2 - SKELETON_LABEL_HEIGHT / 2}
                        width={Math.min(SKELETON_LABEL_WIDTH, n.labelWidth)}
                        height={SKELETON_LABEL_HEIGHT}
                        rx={SKELETON_LABEL_HEIGHT / 2}
                        fill="currentColor"
                        fillOpacity={0.15}
                      />
                    ) : null,
                  )}
                </g>
              </svg>
              {!loading && (
                <div className="pointer-events-none absolute inset-0 flex items-center justify-center">
                  <div className="bg-background/90 text-muted-foreground rounded-md border px-3 py-1.5 text-sm shadow-sm">
                    {emptyMessage}
                  </div>
                </div>
              )}
            </div>
          ) : (
            <svg
              width={chartWidth}
              height={chartHeight}
              className="block overflow-visible"
              role="img"
              aria-label={ariaLabel}
              onMouseMove={onMove}
              onMouseLeave={onLeave}
            >
              <defs>
                {computed.links.map((l) => (
                  <linearGradient
                    key={l.index}
                    id={`${uid}-l${l.index}`}
                    gradientUnits="userSpaceOnUse"
                    x1={l.sourceNode.x1}
                    y1={0}
                    x2={l.targetNode.x0}
                    y2={0}
                  >
                    <stop offset="0%" stopColor={linkColorsOf(l)[0]} />
                    <stop offset="100%" stopColor={linkColorsOf(l)[1]} />
                  </linearGradient>
                ))}
              </defs>

              <g data-layer="links">
                {computed.links.map((l) => (
                  <path
                    key={l.index}
                    d={l.path}
                    fill={`url(#${uid}-l${l.index})`}
                    style={{
                      opacity: linkOpacityFor(l),
                      transition: 'opacity 150ms ease',
                      cursor: linksClickable ? 'pointer' : 'default',
                    }}
                    onMouseEnter={enterLink(l.index)}
                    onMouseLeave={onLeave}
                    onClick={linksClickable ? () => onLinkClick?.(l) : undefined}
                  />
                ))}
              </g>

              <g data-layer="nodes">
                {computed.nodes.map((n) => {
                  const zero = n.value <= 0
                  const color = colorOf(n)
                  const clickable = nodesClickable && (isNodeClickable?.(n) ?? true)
                  return (
                    <rect
                      key={n.key}
                      x={n.x0}
                      y={n.y0}
                      width={n.x1 - n.x0}
                      height={n.height}
                      rx={2}
                      fill={color}
                      fillOpacity={zero ? 0.35 : 1}
                      stroke={zero ? color : undefined}
                      strokeWidth={zero ? 1 : 0}
                      strokeDasharray={zero ? '3 2' : undefined}
                      style={{ cursor: clickable ? 'pointer' : 'default' }}
                      tabIndex={clickable ? 0 : undefined}
                      role={clickable ? 'button' : undefined}
                      aria-label={clickable ? n.label : undefined}
                      onMouseEnter={enterNode(n.key)}
                      onMouseLeave={onLeave}
                      onClick={clickable ? () => onNodeClick?.(n) : undefined}
                      onKeyDown={
                        clickable
                          ? (e) => {
                              if (isActivationKey(e)) {
                                e.preventDefault()
                                onNodeClick?.(n)
                              }
                            }
                          : undefined
                      }
                    />
                  )
                })}
              </g>

              <g data-layer="labels" style={{ pointerEvents: 'none' }}>
                {computed.nodes.map((n) => {
                  if (n.labelWidth <= 0) return null
                  // Dense column: label bands would overlap, so only bars tall
                  // enough to own their band get a label.
                  const gap = computed.columns[n.columnIndex]?.gap ?? 0
                  if (n.height + gap < LABEL_HEIGHT) return null
                  const zero = n.value <= 0
                  const compact = n.labelWidth < MIN_FULL_LABEL_WIDTH
                  const left = computed.columns[n.columnIndex]?.labelSide === 'left'
                  return (
                    <foreignObject
                      key={n.key}
                      x={n.labelX}
                      y={n.y0 + n.height / 2 - LABEL_HEIGHT / 2}
                      width={n.labelWidth}
                      height={LABEL_HEIGHT}
                      style={{ overflow: 'visible' }}
                    >
                      {renderNodeLabel ? (
                        renderNodeLabel(n)
                      ) : (
                        <div
                          className={cn(
                            'flex h-4 items-center gap-1 text-[12px] leading-4 whitespace-nowrap',
                            left && 'justify-end',
                            // A zero bar's label stays muted — it is a
                            // placeholder, not data — but at full token
                            // strength, so it reads rather than disappears.
                            zero ? 'text-muted-foreground' : 'text-foreground',
                          )}
                        >
                          {renderNodeIcon?.(n)}
                          {!compact && (
                            <span
                              className="min-w-0 truncate font-medium"
                              title={n.label}
                            >
                              {n.label}
                            </span>
                          )}
                          {showValue && (
                            <span className="shrink-0 font-semibold" style={numberStyle}>
                              {formatValue(n.value)}
                            </span>
                          )}
                          {!compact && showPercent && (
                            <span
                              className={cn(
                                'inline-flex h-4 shrink-0 items-center rounded-full bg-bg-subtle px-1.5 text-[11px] leading-4 font-semibold text-text-muted',
                                zero && 'opacity-80',
                              )}
                            >
                              <span style={numberStyle}>{formatPercent(n.percent)}</span>
                            </span>
                          )}
                        </div>
                      )}
                    </foreignObject>
                  )
                })}
              </g>
            </svg>
          )}
        </div>
      )}

      {tooltipBody && (
        <div
          ref={tooltipRef}
          role="tooltip"
          className={cn(
            'pointer-events-none absolute top-0 left-0 z-[60] w-max max-w-[260px] will-change-transform',
            tooltipClassName,
          )}
          style={tooltipStyle}
        >
          {tooltipBody}
        </div>
      )}
    </div>
  )
}
