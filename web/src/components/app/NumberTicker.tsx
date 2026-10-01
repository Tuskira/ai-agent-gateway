import { useLayoutEffect, useRef } from 'react'
import { animate } from 'motion/react'
import { cn } from '@/lib/utils'

interface NumberTickerProps {
  value: number
  /** Turns the running number into display text. Must be a stable
   * reference (a module-level formatter), or the count-up restarts on every
   * render. */
  format: (n: number) => string
  className?: string
}

function canAnimate(): boolean {
  if (typeof window === 'undefined') return false
  if (typeof IntersectionObserver === 'undefined') return false
  return !window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
}

/**
 * A number that springs up to `value` the first time it scrolls into view,
 * then from the previous value whenever `value` changes. Modelled on Magic
 * UI's number ticker (same spring), with one difference: where motion isn't
 * available or wanted (reduced motion, no IntersectionObserver) the final
 * formatted value is written straight away, so the text is never a
 * placeholder "0".
 *
 * The span's text is written imperatively on every frame, which keeps the
 * count-up out of React's render cycle.
 */
export function NumberTicker({ value, format, className }: NumberTickerProps) {
  const ref = useRef<HTMLSpanElement>(null)
  const shownRef = useRef(0)

  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return

    if (!canAnimate()) {
      shownRef.current = value
      el.textContent = format(value)
      return
    }

    el.textContent = format(shownRef.current)
    let controls: ReturnType<typeof animate> | undefined
    const observer = new IntersectionObserver((entries) => {
      if (!entries.some((e) => e.isIntersecting)) return
      observer.disconnect()
      controls = animate(shownRef.current, value, {
        type: 'spring',
        stiffness: 1000,
        damping: 60,
        onUpdate: (latest) => {
          shownRef.current = latest
          el.textContent = format(latest)
        },
        onComplete: () => {
          shownRef.current = value
          el.textContent = format(value)
        },
      })
    })
    observer.observe(el)

    return () => {
      observer.disconnect()
      controls?.stop()
    }
  }, [value, format])

  return <span ref={ref} className={cn('inline-block tabular-nums', className)} />
}
