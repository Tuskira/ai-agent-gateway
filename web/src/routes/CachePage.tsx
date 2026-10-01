import { useState } from 'react'
import { Database, RefreshCw, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import {
  useCacheStats,
  useClearCache,
  useClearConnectorCache,
  useRefreshCache,
  useRefreshConnectorCache,
} from '@/lib/queries'
import type { CacheConnectorStat } from '@/lib/cache'
import { formatDate } from '@/lib/utils'
import { ConfirmDialog } from '@/components/app/ConfirmDialog'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'

export default function CachePage() {
  const cacheStatsQuery = useCacheStats()
  const refreshAll = useRefreshCache()
  const clearAll = useClearCache()
  const refreshOne = useRefreshConnectorCache()
  const clearOne = useClearConnectorCache()

  const [refreshAllOpen, setRefreshAllOpen] = useState(false)
  const [clearAllOpen, setClearAllOpen] = useState(false)
  const [clearTarget, setClearTarget] = useState<CacheConnectorStat | null>(null)

  const stats = cacheStatsQuery.data
  const items = stats?.connectors ?? []

  function handleRefreshAll() {
    refreshAll.mutate(undefined, {
      onSuccess: () => {
        toast.success('Refreshing every MCP’s cache')
        setRefreshAllOpen(false)
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to refresh cache')
      },
    })
  }

  function handleClearAll() {
    clearAll.mutate(undefined, {
      onSuccess: () => {
        toast.success('Cache cleared')
        setClearAllOpen(false)
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to clear cache')
      },
    })
  }

  function handleRefreshOne(connectorId: string) {
    refreshOne.mutate(connectorId, {
      onSuccess: () => toast.success('Refreshing MCP cache'),
      onError: (err) =>
        toast.error(err instanceof Error ? err.message : 'Failed to refresh cache'),
    })
  }

  function handleClearOne() {
    if (!clearTarget) return
    clearOne.mutate(clearTarget.connector_id, {
      onSuccess: () => {
        toast.success('MCP cache cleared')
        setClearTarget(null)
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to clear cache')
      },
    })
  }

  const notDeployed = !cacheStatsQuery.isLoading && stats === null

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
            Cache Management
          </h1>
          <p className="text-[13px] text-text-subtle">
            Tool manifests cached per MCP. Refresh after a server deploy; clear to force
            rediscovery.
          </p>
        </div>
        {!notDeployed ? (
          <div className="flex gap-2">
            <Button variant="outline" onClick={() => setRefreshAllOpen(true)}>
              <RefreshCw className="size-3.5" aria-hidden="true" />
              Refresh All
            </Button>
            <Button
              variant="outline"
              className="hover:bg-sev-high-bg hover:text-sev-high"
              onClick={() => setClearAllOpen(true)}
            >
              <Trash2 className="size-3.5" aria-hidden="true" />
              Clear All
            </Button>
          </div>
        ) : null}
      </div>

      {notDeployed ? (
        <div className="flex flex-col items-center gap-3 rounded-r-5 border border-border bg-card px-6 py-14 text-center">
          <span className="flex size-[52px] items-center justify-center rounded-r-5 bg-bg-muted text-text-muted">
            <Database className="size-[22px]" aria-hidden="true" />
          </span>
          <div className="text-base font-semibold text-foreground">
            Cache isn&apos;t available yet
          </div>
          <p className="max-w-[46ch] text-[13.5px] text-text-subtle">
            The cache management endpoints aren&apos;t deployed on this gateway yet.
          </p>
        </div>
      ) : cacheStatsQuery.isLoading ? (
        <div className="grid grid-cols-[repeat(auto-fill,minmax(280px,1fr))] gap-4">
          <Skeleton className="h-[168px] w-full" />
          <Skeleton className="h-[168px] w-full" />
          <Skeleton className="h-[168px] w-full" />
        </div>
      ) : cacheStatsQuery.isError ? (
        <div className="flex flex-col items-center gap-2 rounded-r-5 border border-border bg-card px-6 py-14 text-center text-sm text-sev-high">
          Couldn&apos;t load cache stats.
        </div>
      ) : items.length === 0 ? (
        <div className="flex flex-col items-center gap-2 rounded-r-5 border border-border bg-card px-6 py-14 text-center">
          <span className="text-sm font-medium text-foreground">Nothing cached yet</span>
          <p className="max-w-[320px] text-xs text-text-subtle">
            Tool catalogs appear here once you add an MCP.
          </p>
        </div>
      ) : (
        <div className="grid grid-cols-[repeat(auto-fill,minmax(280px,1fr))] gap-4">
          {items.map((c) => (
            <div
              key={c.connector_id}
              className="flex flex-col gap-3 rounded-r-5 border border-border bg-bg-elevated px-5 py-4"
            >
              <div className="flex items-center justify-between gap-2">
                <span className="font-mono text-[14.5px] font-semibold text-foreground">
                  {c.name}
                </span>
                <span className="inline-flex h-6 min-w-7 items-center justify-center rounded-r-pill bg-bg-muted px-2 text-xs font-bold text-text-muted">
                  {c.tools}
                </span>
              </div>
              <div className="flex flex-col gap-0.5 text-[12.5px] text-text-subtle">
                <span>
                  Last Cached:{' '}
                  <span className="text-text-muted">{formatDate(c.cached_at)}</span>
                </span>
                <span>
                  Avg Fetch: <span className="text-text-muted">—</span>
                </span>
              </div>
              <div className="grid grid-cols-2 gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={refreshOne.isPending}
                  onClick={() => handleRefreshOne(c.connector_id)}
                >
                  <RefreshCw className="size-3.5" aria-hidden="true" />
                  Refresh
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  className="hover:bg-sev-high-bg hover:text-sev-high"
                  onClick={() => setClearTarget(c)}
                >
                  <Trash2 className="size-3.5" aria-hidden="true" />
                  Clear
                </Button>
              </div>
            </div>
          ))}
        </div>
      )}

      <ConfirmDialog
        open={refreshAllOpen}
        onOpenChange={setRefreshAllOpen}
        title="Refresh every MCP's cache?"
        description="The gateway re-runs tool discovery for every MCP. This can take a moment."
        confirmLabel="Refresh all"
        isLoading={refreshAll.isPending}
        onConfirm={handleRefreshAll}
      />
      <ConfirmDialog
        open={clearAllOpen}
        onOpenChange={setClearAllOpen}
        title="Clear the entire cache?"
        description="Every MCP's cached tools are dropped. Agents lose tool access until the next discovery."
        confirmLabel="Clear all"
        destructive
        isLoading={clearAll.isPending}
        onConfirm={handleClearAll}
      />
      <ConfirmDialog
        open={clearTarget !== null}
        onOpenChange={(open) => !open && setClearTarget(null)}
        title={`Clear cache for "${clearTarget?.name}"?`}
        description="Its cached tools are dropped until the next discovery."
        confirmLabel="Clear"
        destructive
        isLoading={clearOne.isPending}
        onConfirm={handleClearOne}
      />
    </div>
  )
}
