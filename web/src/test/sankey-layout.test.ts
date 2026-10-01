import { describe, expect, it } from 'vitest'
import {
  allocateColumnHeights,
  computeLayout,
  ribbonPath,
  type SankeyData,
} from '@/components/app/sankey'

const DATA: SankeyData = {
  columns: [
    { id: 'A', label: 'A', labelSide: 'left' },
    { id: 'B', label: 'B' },
    { id: 'C', label: 'C' },
  ],
  nodes: [
    { key: 'a1', column: 'A', label: 'a1', value: 80, percent: 80 },
    { key: 'a2', column: 'A', label: 'a2', value: 20, percent: 20 },
    { key: 'b1', column: 'B', label: 'b1', value: 100, percent: 100 },
    { key: 'c1', column: 'C', label: 'c1', value: 60, percent: 60 },
    { key: 'c2', column: 'C', label: 'c2', value: 40, percent: 40 },
  ],
  links: [
    { source: 'a1', target: 'b1', value: 80, percent: 100 },
    { source: 'a2', target: 'b1', value: 20, percent: 100 },
    { source: 'b1', target: 'c1', value: 60, percent: 60 },
    { source: 'b1', target: 'c2', value: 40, percent: 40 },
  ],
  total: 100,
}

describe('allocateColumnHeights', () => {
  it('fills the column exactly, in proportion to the values', () => {
    const r = allocateColumnHeights([80, 20], 200, 10, 5)
    expect(r.heights[0]! + r.heights[1]! + r.gap).toBe(200)
    expect(r.heights[0]! / r.heights[1]!).toBeCloseTo(4)
    expect(r.clamped).toEqual([false, false])
    expect(r.degenerate).toBe(false)
  })

  it('inflates tiny bars to the minimum height and rescales the rest', () => {
    const r = allocateColumnHeights([1000, 1], 200, 0, 24)
    expect(r.heights[1]).toBe(24)
    expect(r.clamped).toEqual([false, true])
    expect(r.heights[0]).toBe(176)
  })

  it('marks an all-zero column degenerate with zero-height placeholders', () => {
    const r = allocateColumnHeights([0, 0], 200, 4, 15, 20)
    expect(r.degenerate).toBe(true)
    expect(r.heights).toEqual([20, 20])
  })

  it('squeezes the gap rather than overflowing when the floors do not fit', () => {
    const r = allocateColumnHeights([1, 1, 1, 1], 100, 20, 24)
    const used = r.heights.reduce((a, b) => a + b, 0) + r.gap * 3
    expect(used).toBeLessThanOrEqual(100)
  })
})

describe('computeLayout', () => {
  const layout = computeLayout(DATA, {
    width: 1000,
    height: 300,
    nodeWidth: 10,
    nodeGap: 10,
    leftMargin: 150,
    rightMargin: 200,
    labelGap: 8,
    labelPadding: 12,
  })

  it('places columns left to right with every column filling the height', () => {
    const [a, b, c] = layout.columns
    expect(a!.x0).toBe(150)
    expect(b!.x0).toBeGreaterThan(a!.x0)
    expect(c!.x0).toBeGreaterThan(b!.x0)
    expect(c!.x1).toBe(1000 - 200)
    for (const col of layout.columns) expect(col.contentHeight).toBeCloseTo(300)
  })

  it('lays a left-labelled first column out in the left margin, ending before the bar', () => {
    const a1 = layout.nodeByKey.get('a1')!
    expect(a1.labelWidth).toBe(150 - 8)
    expect(a1.labelX).toBe(0)
    expect(a1.labelX + a1.labelWidth).toBe(a1.x0 - 8)
  })

  it('lays right-labelled columns out after the bar, up to the next column', () => {
    const b1 = layout.nodeByKey.get('b1')!
    const c = layout.columns[2]!
    expect(b1.labelX).toBe(b1.x1 + 8)
    expect(b1.labelX + b1.labelWidth).toBe(c.x0 - 12)
    const c1 = layout.nodeByKey.get('c1')!
    expect(c1.labelX + c1.labelWidth).toBe(1000)
  })

  it('stacks ribbons flush on both bars and drops unresolved links', () => {
    expect(layout.links).toHaveLength(4)
    const b1 = layout.nodeByKey.get('b1')!
    const incoming = layout.links.filter((l) => l.target === 'b1')
    const inThickness = incoming.reduce((acc, l) => acc + (l.ty1 - l.ty0), 0)
    expect(inThickness).toBeCloseTo(b1.height)
    const outgoing = layout.links.filter((l) => l.source === 'b1')
    const outThickness = outgoing.reduce((acc, l) => acc + (l.sy1 - l.sy0), 0)
    expect(outThickness).toBeCloseTo(b1.height)
    for (const l of layout.links) expect(l.path.startsWith('M')).toBe(true)

    const dangling = computeLayout(
      { ...DATA, links: [...DATA.links, { source: 'a1', target: 'nope', value: 5, percent: 1 }] },
      { width: 1000, height: 300 },
    )
    expect(dangling.links).toHaveLength(4)
  })
})

describe('ribbonPath', () => {
  it('draws a closed cubic band between the two edges', () => {
    const d = ribbonPath(10, 0, 20, 110, 5, 15)
    expect(d).toBe('M10,0C60,0 60,5 110,5L110,15C60,15 60,20 10,20Z')
  })
})
