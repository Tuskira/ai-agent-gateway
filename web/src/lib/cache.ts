import { apiFetch, ApiError } from '@/lib/api'

/**
 * Data contract for the Cache Management and Tool Search pages.
 *
 * These endpoints ship with the ops PR and may not exist yet in every
 * environment (404), and can also come back 501/503 while the cache
 * subsystem itself is degraded. Callers resolve to `null` in those cases —
 * same "render an honest empty state, never fabricate data" convention as
 * `overview.ts`'s `fetchOverviewMetrics`.
 */

export interface CacheConnectorStat {
  connector_id: string
  name: string
  tools: number
  stale: number
  cached_at: string | null
  expires_at: string | null
}

export interface CacheStats {
  connectors: CacheConnectorStat[]
  total_tools: number
  stale_tools: number
}

export interface CacheSearchItem {
  connector_id: string
  connector_name: string
  tool_name: string
  description?: string
  input_schema?: unknown
}

export interface CacheSearchResult {
  items: CacheSearchItem[]
  total: number
}

function isNotDeployed(err: unknown): boolean {
  return (
    err instanceof ApiError &&
    (err.status === 404 || err.status === 501 || err.status === 503)
  )
}

/** `GET /api/v1/cache/stats` */
export async function fetchCacheStats(): Promise<CacheStats | null> {
  try {
    return await apiFetch<CacheStats>('/cache/stats')
  } catch (err) {
    if (isNotDeployed(err)) return null
    throw err
  }
}

/** `GET /api/v1/cache/search?q=&limit=&include_stale=` */
export async function searchCache(
  q: string,
  limit = 50,
  includeStale = false,
): Promise<CacheSearchResult | null> {
  const params = new URLSearchParams({ q, limit: String(limit) })
  if (includeStale) params.set('include_stale', 'true')
  try {
    return await apiFetch<CacheSearchResult>(`/cache/search?${params.toString()}`)
  } catch (err) {
    if (isNotDeployed(err)) return null
    throw err
  }
}

/** `POST /api/v1/cache/refresh` — refreshes every connector's cache. */
export function refreshAllCache(): Promise<void> {
  return apiFetch<void>('/cache/refresh', { method: 'POST' })
}

/** `POST /api/v1/cache/refresh/connectors/{id}` */
export function refreshConnectorCache(connectorId: string): Promise<void> {
  return apiFetch<void>(`/cache/refresh/connectors/${encodeURIComponent(connectorId)}`, {
    method: 'POST',
  })
}

/** `DELETE /api/v1/cache` — clears every connector's cache. */
export function clearAllCache(): Promise<void> {
  return apiFetch<void>('/cache', { method: 'DELETE' })
}

/** `DELETE /api/v1/cache/connectors/{id}` */
export function clearConnectorCache(connectorId: string): Promise<void> {
  return apiFetch<void>(`/cache/connectors/${encodeURIComponent(connectorId)}`, {
    method: 'DELETE',
  })
}
