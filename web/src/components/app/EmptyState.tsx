import { cn } from '@/lib/utils'

interface EmptyStateProps {
  label?: string
  className?: string
}

/**
 * Shared "no data yet" caption used by every chart/list on the Overview
 * page when the analytics endpoint isn't available. Kept as one component
 * so the honest-empty-state copy stays identical everywhere it appears.
 */
export function EmptyState({
  label = 'No data yet · enable the ClickHouse sink',
  className,
}: EmptyStateProps) {
  return (
    <span className={cn('max-w-[200px] text-center text-xs text-text-subtle', className)}>
      {label}
    </span>
  )
}
