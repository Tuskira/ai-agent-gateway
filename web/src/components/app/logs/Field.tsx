import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'

interface FieldProps {
  label: string
  children: ReactNode
  className?: string
  /** Span both grid columns — for long values like a trace id or user agent. */
  wide?: boolean
}

/** One label/value entry in a detail drawer's definition list, matching
 * `ConnectorDetailDrawer`'s grid pattern. */
export function Field({ label, children, className, wide }: FieldProps) {
  return (
    <div className={cn(wide && 'col-span-2', className)}>
      <div className="text-xs text-text-subtle">{label}</div>
      <div className="text-foreground">{children}</div>
    </div>
  )
}
