import type { ReactNode } from 'react'
import { motion, useReducedMotion, type HTMLMotionProps } from 'motion/react'
import { cn } from '@/lib/utils'

interface ShinyButtonProps extends HTMLMotionProps<'button'> {
  children: ReactNode
  className?: string
}

// The glint travels with the `--x` custom property, which this spring
// sweeps from right to left; `.shiny-label` and `.shiny-border` in
// index.css draw it from that property.
const shine = {
  initial: { '--x': '100%', scale: 0.98 },
  animate: { '--x': '-100%', scale: 1 },
  whileTap: { scale: 0.96 },
  transition: {
    repeat: Infinity,
    repeatType: 'loop',
    repeatDelay: 1,
    type: 'spring',
    stiffness: 20,
    damping: 15,
    mass: 2,
    scale: { type: 'spring', stiffness: 200, damping: 5, mass: 0.5 },
  },
} satisfies HTMLMotionProps<'button'>

/**
 * The primary call to action: a button in the theme's primary colour with a
 * glint that sweeps across its label and border every second or so. Based
 * on Magic UI's shiny button. Under reduced motion it is a plain button.
 */
export function ShinyButton({ children, className, ...props }: ShinyButtonProps) {
  const reduceMotion = useReducedMotion()
  const moving = !reduceMotion

  return (
    <motion.button
      className={cn(
        'relative cursor-pointer rounded-lg border border-transparent bg-primary px-6 py-2 font-medium text-primary-foreground transition-shadow duration-300 ease-in-out outline-none hover:shadow-2 focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:pointer-events-none disabled:opacity-50',
        className,
      )}
      {...(moving ? shine : {})}
      {...props}
    >
      <span
        className={cn(
          'relative block size-full text-sm tracking-wide uppercase',
          moving && 'shiny-label',
        )}
      >
        {children}
      </span>
      {moving ? (
        <span
          aria-hidden="true"
          className="shiny-border absolute inset-0 z-10 block rounded-[inherit] p-px"
        />
      ) : null}
    </motion.button>
  )
}
