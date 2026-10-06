import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useAccessLogsList, useConnectors } from '@/lib/queries'
import {
  ACCESS_LOG_FILTER_FIELDS,
  formatBytes,
  type AccessLogFilters,
  type AccessLogItem,
} from '@/lib/logs'
import { paginationFromParams, withPagination } from '@/lib/pagination'
import { quickRangeToWindow, type QuickRange } from '@/lib/timeRange'
import { useDebouncedValue } from '@/hooks/use-debounced-value'
import { RelativeTime } from '@/components/app/RelativeTime'
import {
  DataTable,
  DataTablePage,
  type ColumnDef,
  type PaginationModel,
} from '@/components/app/data-table'
import { LogFilterBar } from '@/components/app/logs/LogFilterBar'
import { DurationCell } from '@/components/app/logs/DurationCell'
import { ConnectorName } from '@/components/app/logs/ConnectorName'
import { KeyName } from '@/components/app/logs/KeyName'
import { LogStatusPill } from '@/components/app/logs/LogStatusPill'
import { AccessLogDetailDrawer } from '@/components/app/logs/AccessLogDetailDrawer'
import { IngestUserCell } from '@/components/app/logs/IngestUserCell'

type FilterKey = (typeof ACCESS_LOG_FILTER_FIELDS)[number]['key']
const FILTER_KEYS = ACCESS_LOG_FILTER_FIELDS.map((f) => f.key)

function filtersFromParams(params: URLSearchParams): AccessLogFilters {
  const out: AccessLogFilters = {}
  for (const key of FILTER_KEYS) {
    const value = params.get(key)
    if (value) out[key] = value
  }
  const from = params.get('from')
  const to = params.get('to')
  if (from) out.from = from
  if (to) out.to = to
  return out
}

function draftFromFilters(filters: AccessLogFilters): Record<FilterKey, string> {
  const out = {} as Record<FilterKey, string>
  for (const key of FILTER_KEYS) out[key] = filters[key] ?? ''
  return out
}

export default function AccessLogsPage() {
  const [searchParams, setSearchParams] = useSearchParams()

  const committedFilters = useMemo(() => filtersFromParams(searchParams), [searchParams])
  const pagination = useMemo(() => paginationFromParams(searchParams), [searchParams])
  const limit = pagination.pageSize
  const offset = pagination.page * pagination.pageSize

  const [draft, setDraft] = useState<Record<FilterKey, string>>(() =>
    draftFromFilters(committedFilters),
  )
  const debouncedDraft = useDebouncedValue(draft, 400)
  const [activeRange, setActiveRange] = useState<QuickRange | null>(null)
  const isFirstRender = useRef(true)

  const logsQuery = useAccessLogsList(committedFilters, limit, offset)
  const connectorsQuery = useConnectors()
  const connectorMap = useMemo(
    () => new Map((connectorsQuery.data?.items ?? []).map((c) => [c.id, c.name])),
    [connectorsQuery.data],
  )

  const notDeployed = logsQuery.data === null && !logsQuery.isLoading
  const items = logsQuery.data?.items ?? []
  const total = logsQuery.data?.total ?? 0

  const [selected, setSelected] = useState<AccessLogItem | null>(null)

  function commit(next: Record<FilterKey, string>) {
    setSearchParams(
      (prev) => {
        const p = new URLSearchParams(prev)
        for (const key of FILTER_KEYS) {
          if (next[key]) p.set(key, next[key])
          else p.delete(key)
        }
        p.delete('offset')
        return p
      },
      { replace: true },
    )
  }

  // Typing debounces to the URL on its own; the effect skips the mount so
  // loading a shared link doesn't immediately rewrite it.
  useEffect(() => {
    if (isFirstRender.current) {
      isFirstRender.current = false
      return
    }
    commit(debouncedDraft)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [debouncedDraft])

  function handleRangeSelect(range: QuickRange) {
    const { from, to } = quickRangeToWindow(range)
    setActiveRange(range)
    setSearchParams(
      (prev) => {
        const p = new URLSearchParams(prev)
        p.set('from', from)
        p.set('to', to)
        p.delete('offset')
        return p
      },
      { replace: true },
    )
  }

  function handlePaginationChange(next: PaginationModel) {
    setSearchParams((prev) => withPagination(prev, next), { replace: true })
  }

  const columns: ColumnDef<AccessLogItem>[] = [
    {
      id: 'timestamp',
      accessorKey: 'timestamp',
      header: 'Timestamp',
      size: 160,
      meta: { tooltip: false },
      cell: ({ row }) => <RelativeTime iso={row.original.timestamp} />,
    },
    {
      id: 'method',
      accessorKey: 'method',
      header: 'Method',
      size: 130,
      cell: ({ row }) => <span className="font-mono text-xs">{row.original.method}</span>,
    },
    {
      id: 'connector',
      accessorFn: (l) => connectorMap.get(l.connector_id) ?? l.connector_id,
      header: 'MCP',
      // The name brings its own tooltip, which shows the connector id.
      meta: { tooltip: false },
      cell: ({ row }) => (
        <ConnectorName
          connectorId={row.original.connector_id}
          name={connectorMap.get(row.original.connector_id)}
        />
      ),
    },
    {
      id: 'tool',
      accessorKey: 'tool_name',
      header: 'Tool',
      size: 200,
      cell: ({ row }) => (
        <span className="font-mono text-xs font-medium">
          {row.original.tool_name || '—'}
        </span>
      ),
    },
    {
      id: 'key',
      accessorKey: 'key_id',
      header: 'Key',
      meta: { tooltip: false },
      cell: ({ row }) => <KeyName keyId={row.original.key_id} />,
    },
    {
      id: 'user',
      accessorFn: (l) => l.user ?? '',
      header: 'User',
      size: 200,
      meta: { tooltip: false },
      cell: ({ row }) => <IngestUserCell source={row.original.source} user={row.original.user} />,
    },
    {
      id: 'duration',
      accessorKey: 'duration_ms',
      header: 'Duration',
      size: 110,
      cell: ({ row }) => <DurationCell ms={row.original.duration_ms} />,
    },
    {
      id: 'status',
      accessorKey: 'status_code',
      header: 'Status',
      size: 130,
      cell: ({ row }) => (
        <LogStatusPill
          statusCode={row.original.status_code}
          errorLabel={row.original.error_code}
        />
      ),
    },
    {
      id: 'req_size',
      accessorKey: 'bytes_in',
      header: 'Request size',
      size: 120,
      cell: ({ row }) => (
        <span className="text-text-muted">{formatBytes(row.original.bytes_in)}</span>
      ),
    },
    {
      id: 'res_size',
      accessorKey: 'bytes',
      header: 'Response size',
      size: 130,
      cell: ({ row }) => (
        <span className="text-text-muted">{formatBytes(row.original.bytes)}</span>
      ),
    },
  ]

  return (
    <DataTablePage>
      <div>
        <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
          Access Logs ({total})
        </h1>
        <p className="text-[13px] text-text-subtle">
          Every request the gateway routed to an MCP.
        </p>
      </div>

      <LogFilterBar
        fields={ACCESS_LOG_FILTER_FIELDS}
        xlColumns={7}
        values={draft}
        onFieldChange={(key, value) => setDraft((d) => ({ ...d, [key]: value }))}
        onApply={() => commit(draft)}
        onRefresh={() => logsQuery.refetch()}
        isFetching={logsQuery.isFetching}
        activeRange={activeRange}
        onRangeSelect={handleRangeSelect}
      />

      <DataTable
        storageKey="access-logs"
        columns={columns}
        data={items}
        getRowId={(l) => l.request_id}
        onRowClick={setSelected}
        loading={logsQuery.isLoading}
        error={logsQuery.isError && "Couldn't load access logs."}
        emptyTitle={
          notDeployed ? "Analytics isn't available yet" : 'No matching requests'
        }
        emptyDescription={
          notDeployed
            ? 'Analytics requires the ClickHouse sink.'
            : 'Try widening your filters or time range.'
        }
        pagination={pagination}
        onPaginationChange={handlePaginationChange}
        totalRows={total}
        skeletonRows={6}
      />

      <AccessLogDetailDrawer
        item={selected}
        connectorName={selected ? connectorMap.get(selected.connector_id) : undefined}
        onOpenChange={(open) => !open && setSelected(null)}
      />
    </DataTablePage>
  )
}
