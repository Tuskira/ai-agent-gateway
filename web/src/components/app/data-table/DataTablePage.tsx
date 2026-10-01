import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'

interface DataTablePageProps {
  children: ReactNode
  className?: string
}

/**
 * Page column for a list page. It takes the height of the app's content area,
 * so a `DataTable` inside it scrolls its own rows under a sticky header
 * instead of growing and scrolling the whole page.
 *
 * The minimum height keeps the table usable in a short window: below it, the
 * column stops shrinking and the page scrolls as a last resort.
 */
export function DataTablePage({ children, className }: DataTablePageProps) {
  return (
    <div className={cn('flex h-full min-h-[32rem] flex-col gap-4', className)}>
      {children}
    </div>
  )
}
