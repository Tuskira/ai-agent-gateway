import { useEffect, useState } from 'react'
import { gridColumnCount } from '@/lib/token-monitoring'

/** The number of columns an auto-fill CSS grid currently lays out, kept up
 * to date as it resizes. Returns a callback ref for the grid element and the
 * count (`fallback` until the grid is measured, e.g. in tests). */
export function useGridColumns(
  fallback: number,
): [(el: HTMLElement | null) => void, number] {
  const [el, setEl] = useState<HTMLElement | null>(null)
  const [cols, setCols] = useState(fallback)
  useEffect(() => {
    if (!el || typeof ResizeObserver === 'undefined') return
    const measure = () =>
      setCols(gridColumnCount(getComputedStyle(el).gridTemplateColumns) ?? fallback)
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(el)
    return () => observer.disconnect()
  }, [el, fallback])
  return [setEl, cols]
}
