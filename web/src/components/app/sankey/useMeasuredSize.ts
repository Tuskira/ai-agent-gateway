import { useLayoutEffect, useRef, useState } from 'react'

/**
 * Tracks the content-box size of the element bound to `ref`.
 *
 * Both dimensions are 0 until the first measurement (taken synchronously in
 * a layout effect so the first paint already has a real size), then follow
 * a ResizeObserver. State only updates when a dimension actually changes,
 * so observers firing with the same size don't re-render consumers.
 */
export const useMeasuredSize = () => {
  const ref = useRef<HTMLDivElement | null>(null)
  const [size, setSize] = useState({ width: 0, height: 0 })

  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const update = (w: number, h: number) =>
      setSize((s) => (s.width === w && s.height === h ? s : { width: w, height: h }))
    const r = el.getBoundingClientRect()
    update(Math.floor(r.width), Math.floor(r.height))
    const ro = new ResizeObserver((entries) => {
      for (const e of entries) {
        update(Math.floor(e.contentRect.width), Math.floor(e.contentRect.height))
      }
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  return { ref, width: size.width, height: size.height }
}
