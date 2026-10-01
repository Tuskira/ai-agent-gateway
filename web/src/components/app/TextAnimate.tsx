import { motion, useReducedMotion, type Variants } from 'motion/react'
import { cn } from '@/lib/utils'

interface TextAnimateProps {
  children: string
  className?: string
  /** Seconds for the whole string to finish arriving, however many words. */
  duration?: number
}

const wordVariants: Variants = {
  hidden: { opacity: 0, filter: 'blur(10px)', y: 20 },
  show: {
    opacity: 1,
    filter: 'blur(0px)',
    y: 0,
    transition: {
      y: { duration: 0.3 },
      opacity: { duration: 0.4 },
      filter: { duration: 0.3 },
    },
  },
}

/**
 * Word-by-word "blur in up" reveal for card and KPI titles — the
 * `blurInUp` preset of Magic UI's TextAnimate, trimmed to the one animation
 * the dashboard uses. Plays once, when the title scrolls into
 * view. Renders the plain string when the user prefers reduced motion.
 *
 * Screen readers get a multi-word string whole, from a visually hidden
 * copy, with the animated per-word spans hidden from them. A single word
 * needs no copy: it is one span either way.
 */
export function TextAnimate({ children, className, duration = 0.3 }: TextAnimateProps) {
  const reduceMotion = useReducedMotion()
  if (reduceMotion) return <span className={className}>{children}</span>

  const segments = children.split(/(\s+)/)
  const split = segments.length > 1
  const container: Variants = {
    hidden: {},
    show: { transition: { staggerChildren: duration / segments.length } },
  }
  // Without IntersectionObserver there is no viewport to wait for.
  const trigger =
    typeof IntersectionObserver === 'undefined'
      ? { animate: 'show' }
      : { whileInView: 'show', viewport: { once: true } }

  return (
    <motion.span
      variants={container}
      initial="hidden"
      className={cn('whitespace-pre-wrap', className)}
      {...trigger}
    >
      {split ? <span className="sr-only">{children}</span> : null}
      {segments.map((segment, i) => (
        <motion.span
          key={`${segment}-${i}`}
          variants={wordVariants}
          className="inline-block whitespace-pre"
          aria-hidden={split ? true : undefined}
        >
          {segment}
        </motion.span>
      ))}
    </motion.span>
  )
}
