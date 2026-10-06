import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useLlmLogsList } from '@/lib/queries'
import {
  LLM_LOG_FILTER_FIELDS,
  type LlmLogFilters,
  type LlmLogItem,
} from '@/lib/llm-logs'
import { formatCompactNumber } from '@/lib/overview'
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
import { KeyName } from '@/components/app/logs/KeyName'
import { LogStatusPill } from '@/components/app/logs/LogStatusPill'
import { LlmLogDetailDrawer } from '@/components/app/logs/LlmLogDetailDrawer'
import { IngestUserCell } from '@/components/app/logs/IngestUserCell'
import { Badge } from '@/components/ui/badge'

type FilterKey = (typeof LLM_LOG_FILTER_FIELDS)[number]['key']
const FILTER_KEYS = LLM_LOG_FILTER_FIELDS.map((f) => f.key)

function filtersFromParams(params: URLSearchParams): LlmLogFilters {
  const out: LlmLogFilters = {}
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

function draftFromFilters(filters: LlmLogFilters): Record<FilterKey, string> {
  const out = {} as Record<FilterKey, string>
  for (const key of FILTER_KEYS) out[key] = filters[key] ?? ''
  return out
}

export default function LlmLogsPage() {
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

  const logsQuery = useLlmLogsList(committedFilters, limit, offset)

  const notDeployed = logsQuery.data === null && !logsQuery.isLoading
  const items = logsQuery.data?.items ?? []
  const total = logsQuery.data?.total ?? 0

  const [selected, setSelected] = useState<LlmLogItem | null>(null)

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

  const columns: ColumnDef<LlmLogItem>[] = [
    {
      id: 'timestamp',
      accessorKey: 'timestamp',
      header: 'Timestamp',
      size: 160,
      meta: { tooltip: false },
      cell: ({ row }) => <RelativeTime iso={row.original.timestamp} />,
    },
    {
      id: 'model',
      accessorKey: 'model',
      header: 'Model',
      size: 200,
      cell: ({ row }) => (
        <span className="font-mono text-xs font-medium">{row.original.model}</span>
      ),
    },
    {
      id: 'provider',
      accessorKey: 'provider',
      header: 'Provider',
      cell: ({ row }) => (
        <span className="text-text-muted">{row.original.provider || '—'}</span>
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
      id: 'client',
      accessorKey: 'client_name',
      header: 'Client',
      cell: ({ row }) =>
        row.original.client_name ? (
          <Badge variant="outline">{row.original.client_name}</Badge>
        ) : (
          <span className="text-text-subtle">—</span>
        ),
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
      id: 'path',
      accessorKey: 'path',
      header: 'Path',
      size: 200,
      cell: ({ row }) => (
        <span className="font-mono text-xs text-text-muted">{row.original.path}</span>
      ),
    },
    {
      id: 'status',
      accessorKey: 'status_code',
      header: 'Status',
      size: 130,
      cell: ({ row }) => (
        <LogStatusPill
          statusCode={row.original.status_code}
          errorLabel={row.original.error ? 'error' : null}
        />
      ),
    },
    {
      id: 'duration',
      accessorKey: 'duration_ms',
      header: 'Duration',
      size: 110,
      cell: ({ row }) => <DurationCell ms={row.original.duration_ms} />,
    },
    {
      id: 'tokens',
      accessorFn: (l) => l.input_tokens + l.output_tokens,
      header: 'Tokens in / out',
      size: 140,
      cell: ({ row }) => (
        <span className="text-text-muted tabular-nums">
          {formatCompactNumber(row.original.input_tokens)} /{' '}
          {formatCompactNumber(row.original.output_tokens)}
        </span>
      ),
    },
    {
      id: 'cache',
      accessorFn: (l) => l.cache_read_tokens + l.cache_creation_tokens,
      header: 'Cache read / write',
      size: 160,
      cell: ({ row }) => (
        <span className="text-text-muted tabular-nums">
          {formatCompactNumber(row.original.cache_read_tokens)} /{' '}
          {formatCompactNumber(row.original.cache_creation_tokens)}
        </span>
      ),
    },
    {
      id: 'stop_reason',
      accessorFn: (l) => l.stop_reason ?? '',
      header: 'Stop reason',
      cell: ({ row }) => (
        <span className="text-text-muted">{row.original.stop_reason || '—'}</span>
      ),
    },
    {
      id: 'stream',
      accessorFn: (l) => (l.stream ? 'stream' : ''),
      header: 'Stream',
      size: 100,
      cell: ({ row }) =>
        row.original.stream ? (
          <Badge variant="outline">stream</Badge>
        ) : (
          <span className="text-text-subtle">—</span>
        ),
    },
  ]

  return (
    <DataTablePage>
      <div>
        <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
          LLM Logs ({total})
        </h1>
        <p className="text-[13px] text-text-subtle">
          Every prompt and completion the gateway proxied to a model provider.
        </p>
      </div>

      <LogFilterBar
        fields={LLM_LOG_FILTER_FIELDS}
        xlColumns={6}
        values={draft}
        onFieldChange={(key, value) => setDraft((d) => ({ ...d, [key]: value }))}
        onApply={() => commit(draft)}
        onRefresh={() => logsQuery.refetch()}
        isFetching={logsQuery.isFetching}
        activeRange={activeRange}
        onRangeSelect={handleRangeSelect}
      />

      <DataTable
        storageKey="llm-logs"
        columns={columns}
        data={items}
        getRowId={(l) => l.request_id}
        onRowClick={setSelected}
        loading={logsQuery.isLoading}
        error={logsQuery.isError && "Couldn't load LLM logs."}
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

      <LlmLogDetailDrawer
        item={selected}
        onOpenChange={(open) => !open && setSelected(null)}
      />
    </DataTablePage>
  )
}
