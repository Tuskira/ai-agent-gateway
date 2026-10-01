import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'
import { Card, CardContent } from '@/components/ui/card'

interface StatCardProps {
  /** Tailwind background-color class for the small square indicator dot. */
  dotClassName: string
  title: string
  children: ReactNode
}

/**
 * Matches the mock's stat-tile pattern: a 12px muted label with a small
 * colored square dot, then a large bold value and an optional 12px delta
 * line — both supplied by the caller via `children`.
 */
export function StatCard({ dotClassName, title, children }: StatCardProps) {
  return (
    <Card>
      <CardContent className="flex flex-col gap-1.5">
        <div className="flex items-center gap-2 text-xs font-medium text-text-muted">
          <span
            className={cn('size-2 shrink-0 rounded-[2px]', dotClassName)}
            aria-hidden="true"
          />
          <span className="truncate">{title}</span>
        </div>
        {children}
      </CardContent>
    </Card>
  )
}
