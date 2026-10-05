import { useMemo, useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import type { ColumnDef } from '@tanstack/react-table'
import { ArrowLeft } from 'lucide-react'
import { DashboardCard } from '@/components/app/DashboardCard'
import { DataTable, type PaginationModel } from '@/components/app/data-table'
import { RankedBarList } from '@/components/app/RankedBarList'
import { RolePill, WindowSwitch } from '@/components/app/token-monitoring/parts'
import { TokenAreaChart } from '@/components/app/token-monitoring/TokenAreaChart'
import { useTokenMonitoringDetail } from '@/lib/queries'
import { formatCompactNumber } from '@/lib/overview'
import {
  callerBars,
  formatTokenCost,
  modelBars,
  NO_USAGE_LABEL,
  type SessionTokens,
} from '@/lib/token-monitoring'
import {
  AnalyticsOffNotice,
  TotalsTiles,
} from '@/components/app/token-monitoring/TotalsTiles'
import { useMonitoringWindow } from '@/hooks/use-monitoring-window'

const PAGE_SIZE = 25

const TOKEN_COLUMNS = [
  ['tokens', 'Tokens'],
  ['prompt_tokens', 'Input'],
  ['completion_tokens', 'Output'],
  ['cache_read_tokens', 'Cache read'],
  ['cache_write_tokens', 'Cache write'],
] as const

function when(ts: string): string {
  const d = new Date(ts)
  return Number.isNaN(d.getTime())
    ? '—'
    : `${d.toISOString().slice(0, 16).replace('T', ' ')} UTC`
}

/** One model (?model=) broken down by caller, or one API key (/keys/:id)
 * broken down by model; both with the sessions behind the usage. */
export default function TokenMonitoringDetailPage({ kind }: { kind: 'model' | 'key' }) {
  const [window, setWindowParam] = useMonitoringWindow()
  const [params] = useSearchParams()
  const { id = '' } = useParams()
  const subject = kind === 'model' ? (params.get('model') ?? '') : id
  const [page, setPage] = useState<PaginationModel>({ page: 0, pageSize: PAGE_SIZE })
  const query = useTokenMonitoringDetail(
    kind,
    subject,
    window,
    page.pageSize,
    page.page * page.pageSize,
  )
  const navigate = useNavigate()
  const data = query.data

  const setWindow: typeof setWindowParam = (w) => {
    setPage((p) => ({ ...p, page: 0 }))
    setWindowParam(w)
  }

  const title =
    kind === 'model' ? subject : data?.key?.name || (subject ? 'Unknown key' : '')

  const columns = useMemo<ColumnDef<SessionTokens>[]>(
    () => [
      {
        id: 'session',
        header: 'Session',
        accessorKey: 'session_id',
        cell: ({ row }) =>
          row.original.session_id ? (
            <Link
              to={`/session-timeline?session_id=${encodeURIComponent(row.original.session_id)}`}
              className="font-mono text-[12px] text-text-link"
              onClick={(e) => e.stopPropagation()}
            >
              {row.original.session_id}
            </Link>
          ) : (
            <span className="text-text-subtle">No session</span>
          ),
      },
      kind === 'model'
        ? {
            id: 'caller',
            header: 'Caller',
            accessorFn: (s) => s.key_name || s.key_id,
            cell: ({ row }) => row.original.key_name || row.original.key_id || '—',
          }
        : { id: 'models', header: 'Models', accessorFn: (s) => s.models.join(', ') },
      ...TOKEN_COLUMNS.map(([key, header]): ColumnDef<SessionTokens> => ({
        id: key,
        header,
        accessorKey: key,
        cell: ({ row }) => formatCompactNumber(row.original[key]),
      })),
      {
        id: 'cost',
        header: 'Cost',
        accessorFn: (s) => s.cost_usd ?? -1,
        cell: ({ row }) => formatTokenCost(row.original.cost_usd),
      },
      { id: 'calls', header: 'Calls', accessorKey: 'calls' },
      {
        id: 'last',
        header: 'Last seen',
        accessorKey: 'last_seen',
        cell: ({ row }) => when(row.original.last_seen),
      },
    ],
    [kind],
  )

  return (
    <div className="flex flex-col gap-4">
      <Link
        to={`/token-monitoring?window=${window}`}
        className="inline-flex w-fit items-center gap-1 text-[13px] font-medium text-text-link"
      >
        <ArrowLeft className="size-3.5" aria-hidden="true" />
        Token Monitoring
      </Link>
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div className="flex min-w-0 flex-col gap-1">
          <div className="flex items-center gap-2">
            <h1 className="truncate font-mono text-[20px] leading-[1.25] font-bold text-foreground">
              {title}
            </h1>
            {data?.key ? <RolePill role={data.key.role} /> : null}
          </div>
          <p className="text-[13px] text-text-subtle">
            {kind === 'model'
              ? 'Model: usage by caller and by session.'
              : `API key ${subject}: usage by model and by session.`}{' '}
            Times in UTC.
          </p>
        </div>
        <WindowSwitch value={window} onChange={setWindow} />
      </div>

      {data === null ? (
        <AnalyticsOffNotice />
      ) : (
        <>
          <TotalsTiles totals={data?.totals} loading={query.isLoading} />
          <div className="grid grid-cols-[repeat(auto-fit,minmax(min(100%,340px),1fr))] gap-4">
            <section aria-label={kind === 'model' ? 'By caller' : 'By model'}>
              <DashboardCard
                title={kind === 'model' ? 'By caller' : 'By model'}
                className="h-[348px]"
              >
                <RankedBarList
                  variant="stacked"
                  emptyLabel={NO_USAGE_LABEL}
                  items={
                    !data
                      ? null
                      : kind === 'model'
                        ? callerBars(data.by_key)
                        : modelBars(data.by_model)
                  }
                />
              </DashboardCard>
            </section>
            <DashboardCard title="Usage over time (UTC)">
              <TokenAreaChart
                points={data?.burn ?? null}
                granularity={data?.granularity ?? 'hour'}
                emptyLabel={NO_USAGE_LABEL}
              />
            </DashboardCard>
          </div>
          <section aria-label="Sessions" className="flex flex-col gap-3">
            <h2 className="text-[15px] font-semibold text-foreground">Sessions</h2>
            <DataTable
              storageKey={`token-monitoring-sessions-${kind}`}
              columns={columns}
              data={data?.sessions ?? []}
              getRowId={(s) => `${s.session_id}|${s.key_id}`}
              getRowLabel={(s) => s.session_id || 'No session'}
              onRowClick={(s) => {
                if (kind === 'model' && s.key_id)
                  navigate(
                    `/token-monitoring/keys/${encodeURIComponent(s.key_id)}?window=${window}`,
                  )
              }}
              pagination={page}
              onPaginationChange={setPage}
              totalRows={data?.sessions_total ?? 0}
              loading={query.isLoading}
              error={query.isError && "Couldn't load sessions."}
              emptyTitle={NO_USAGE_LABEL}
            />
          </section>
        </>
      )}
    </div>
  )
}
