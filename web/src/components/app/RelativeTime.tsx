import { cn } from '@/lib/utils'

interface RelativeTimeProps {
  iso: string | null | undefined
  className?: string
}

function formatRelative(date: Date): string {
  const diffSec = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000))
  if (diffSec < 5) return 'just now'
  if (diffSec < 60) return `${diffSec}s ago`
  const diffMin = Math.round(diffSec / 60)
  if (diffMin < 60) return `${diffMin}m ago`
  const diffHr = Math.round(diffMin / 60)
  if (diffHr < 24) return `${diffHr}h ago`
  const diffDay = Math.round(diffHr / 24)
  if (diffDay < 30) return `${diffDay}d ago`
  const diffMonth = Math.round(diffDay / 30)
  if (diffMonth < 12) return `${diffMonth}mo ago`
  return `${Math.round(diffMonth / 12)}y ago`
}

/** "3m ago" with the full local timestamp in a `title` tooltip — used in
 * every log table's Timestamp column instead of a raw ISO string. */
export function RelativeTime({ iso, className }: RelativeTimeProps) {
  if (!iso) return <span className={className}>—</span>

  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return <span className={className}>—</span>

  const full = date.toLocaleString(undefined, {
    dateStyle: 'medium',
    timeStyle: 'medium',
  })

  return (
    <span className={cn('tabular-nums whitespace-nowrap', className)} title={full}>
      {formatRelative(date)}
    </span>
  )
}
