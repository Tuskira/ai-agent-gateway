import { Badge } from '@/components/ui/badge'

/**
 * The "User" column shared by the LLM Logs and Access Logs tables: the
 * self-reported caller identity on an ingested row (`sink.AccessLog`/
 * `LLMCall.User`, set by the capture component that feeds
 * `POST /api/v1/ingest`), plus a small "Interceptor" badge marking the row
 * as ingested rather than gateway-proxied. A gateway-proxied row (source
 * "gateway", or "" on a row written before the source column existed)
 * shows neither -- there is no per-caller identity for those, only the
 * key/principal columns that already exist.
 */
export function IngestUserCell({
  source,
  user,
}: {
  source?: string
  user?: string
}) {
  const isInterceptor = source === 'interceptor'
  if (!isInterceptor) return <span className="text-text-subtle">—</span>
  return (
    <span className="flex items-center gap-1.5">
      <Badge variant="secondary">Interceptor</Badge>
      {user ? (
        <span className="truncate text-xs text-text-muted" title={user}>
          {user}
        </span>
      ) : null}
    </span>
  )
}
