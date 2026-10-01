import { useAuth } from '@/auth/AuthContext'
import { useAccessLogDetail } from '@/lib/queries'
import type { AccessLogItem } from '@/lib/logs'
import { formatBytes } from '@/lib/logs'
import { RelativeTime } from '@/components/app/RelativeTime'
import { DurationCell } from '@/components/app/logs/DurationCell'
import { ConnectorName } from '@/components/app/logs/ConnectorName'
import { KeyName } from '@/components/app/logs/KeyName'
import { LogStatusPill } from '@/components/app/logs/LogStatusPill'
import { Field } from '@/components/app/logs/Field'
import { SessionIdValue } from '@/components/app/logs/SessionIdValue'
import { HeadersTable } from '@/components/app/logs/HeadersTable'
import { BodiesSection } from '@/components/app/logs/BodiesSection'
import { AdminOnlyNotice } from '@/components/app/logs/AdminOnlyNotice'
import { Skeleton } from '@/components/ui/skeleton'
import { Badge } from '@/components/ui/badge'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'

interface AccessLogDetailDrawerProps {
  item: AccessLogItem | null
  connectorName: string | undefined
  onOpenChange: (open: boolean) => void
  /** Readable duration text ("3m 53.8s"); the log pages keep whole ms. */
  readableDuration?: boolean
}

export function AccessLogDetailDrawer({
  item,
  connectorName,
  onOpenChange,
  readableDuration = false,
}: AccessLogDetailDrawerProps) {
  const open = item !== null
  const { principal } = useAuth()
  const isAdmin = !!principal?.roles.includes('admin')
  const detailQuery = useAccessLogDetail(item?.request_id, open && isAdmin)

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="w-full sm:max-w-2xl">
        <SheetHeader>
          <SheetTitle className="font-mono">
            {item?.method} · {item?.tool_name || '—'}
          </SheetTitle>
          <SheetDescription className="font-mono">{item?.request_id}</SheetDescription>
        </SheetHeader>
        {item ? (
          <div className="flex flex-1 flex-col gap-6 overflow-y-auto px-4 pb-4">
            <div className="flex flex-wrap items-center gap-2">
              <LogStatusPill statusCode={item.status_code} errorLabel={item.error_code} />
              <DurationCell ms={item.duration_ms} readable={readableDuration} />
            </div>

            <div className="grid grid-cols-2 gap-3 text-sm">
              <Field label="Timestamp">
                <RelativeTime iso={item.timestamp} />
              </Field>
              <Field label="MCP">
                <ConnectorName connectorId={item.connector_id} name={connectorName} />
              </Field>
              <Field label="Tool">
                <span className="font-mono text-xs">{item.tool_name || '—'}</span>
              </Field>
              <Field label="Profile">{item.profile || '—'}</Field>
              <Field label="Session ID">
                <SessionIdValue sessionId={item.session_id} />
              </Field>
              <Field label="Principal">{item.principal || '—'}</Field>
              <Field label="API key">
                <KeyName keyId={item.key_id} />
              </Field>
              <Field label="JSON-RPC ID">{item.json_rpc_id ?? '—'}</Field>
              <Field label="Error code">{item.error_code || '—'}</Field>
              <Field label="Client IP">{item.client_ip || '—'}</Field>
              <Field label="Source">
                {item.source === 'interceptor' ? (
                  <Badge variant="secondary">Interceptor</Badge>
                ) : (
                  'Gateway'
                )}
              </Field>
              <Field label="User">{item.user || '—'}</Field>
              <Field label="Request size">{formatBytes(item.bytes_in)}</Field>
              <Field label="Response size">{formatBytes(item.bytes)}</Field>
              <Field label="Request ID" wide>
                <span className="font-mono text-xs break-all">{item.request_id}</span>
              </Field>
              <Field label="Correlation ID" wide>
                <span className="font-mono text-xs break-all">
                  {item.correlation_id || '—'}
                </span>
              </Field>
              <Field label="Trace ID" wide>
                <span className="font-mono text-xs break-all">
                  {item.trace_id || '—'}
                </span>
              </Field>
              <Field label="User agent" wide>
                <span className="text-xs break-all">{item.user_agent || '—'}</span>
              </Field>
            </div>

            <div>
              <div className="mb-2 text-xs font-semibold text-text-subtle uppercase">
                Headers
              </div>
              {!isAdmin ? (
                <AdminOnlyNotice />
              ) : detailQuery.isLoading ? (
                <div className="flex flex-col gap-1.5">
                  <Skeleton className="h-8 w-full" />
                  <Skeleton className="h-8 w-full" />
                </div>
              ) : detailQuery.isError ? (
                <p className="text-xs text-sev-high">Couldn&apos;t load headers.</p>
              ) : (
                <HeadersTable headers={detailQuery.data?.headers} />
              )}
            </div>

            {isAdmin && detailQuery.data ? (
              <BodiesSection
                requestBody={detailQuery.data.request_body}
                responseBody={detailQuery.data.response_body}
                truncated={detailQuery.data.truncated}
              />
            ) : null}
          </div>
        ) : null}
      </SheetContent>
    </Sheet>
  )
}
