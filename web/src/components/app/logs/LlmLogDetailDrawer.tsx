import { useAuth } from '@/auth/AuthContext'
import { useLlmLogDetail } from '@/lib/queries'
import type { LlmLogItem } from '@/lib/llm-logs'
import { formatCompactNumber } from '@/lib/overview'
import { RelativeTime } from '@/components/app/RelativeTime'
import { DurationCell } from '@/components/app/logs/DurationCell'
import { KeyName } from '@/components/app/logs/KeyName'
import { LogStatusPill } from '@/components/app/logs/LogStatusPill'
import { Field } from '@/components/app/logs/Field'
import { SessionIdValue } from '@/components/app/logs/SessionIdValue'
import { HeadersTable } from '@/components/app/logs/HeadersTable'
import { BodiesSection, PromptSection } from '@/components/app/logs/BodiesSection'
import { AdminOnlyNotice } from '@/components/app/logs/AdminOnlyNotice'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'

interface LlmLogDetailDrawerProps {
  item: LlmLogItem | null
  onOpenChange: (open: boolean) => void
  /** Readable duration text ("3m 53.8s"); the log pages keep whole ms. */
  readableDuration?: boolean
}

export function LlmLogDetailDrawer({ item, onOpenChange, readableDuration = false }: LlmLogDetailDrawerProps) {
  const open = item !== null
  const { principal } = useAuth()
  const isAdmin = !!principal?.roles.includes('admin')
  const detailQuery = useLlmLogDetail(item?.request_id, open && isAdmin)

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="w-full sm:max-w-2xl">
        <SheetHeader>
          <SheetTitle className="font-mono">{item?.model}</SheetTitle>
          <SheetDescription className="font-mono">{item?.request_id}</SheetDescription>
        </SheetHeader>
        {item ? (
          <div className="flex flex-1 flex-col gap-6 overflow-y-auto px-4 pb-4">
            <div className="flex flex-wrap items-center gap-2">
              <LogStatusPill
                statusCode={item.status_code}
                errorLabel={item.error ? 'error' : null}
              />
              <DurationCell ms={item.duration_ms} readable={readableDuration} />
              {item.stream ? <Badge variant="outline">stream</Badge> : null}
            </div>

            <div className="grid grid-cols-2 gap-3 text-sm">
              <Field label="Timestamp">
                <RelativeTime iso={item.timestamp} />
              </Field>
              <Field label="Provider">{item.provider || '—'}</Field>
              <Field label="Upstream host">
                <span className="font-mono text-xs break-all">
                  {item.upstream_host || '—'}
                </span>
              </Field>
              <Field label="Path">
                <span className="font-mono text-xs break-all">{item.path || '—'}</span>
              </Field>
              <Field label="Session ID">
                <SessionIdValue sessionId={item.session_id} />
              </Field>
              <Field label="Principal">{item.principal || '—'}</Field>
              <Field label="API key">
                <KeyName keyId={item.key_id} />
              </Field>
              <Field label="Client IP">{item.client_ip || '—'}</Field>
              <Field label="Client">
                {item.client_name ? (
                  <Badge variant="outline">{item.client_name}</Badge>
                ) : (
                  '—'
                )}
              </Field>
              <Field label="Source">
                {item.source === 'interceptor' ? (
                  <Badge variant="secondary">Interceptor</Badge>
                ) : (
                  'Gateway'
                )}
              </Field>
              <Field label="User">{item.user || '—'}</Field>
              <Field label="User agent" wide>
                <span className="font-mono text-xs break-all">
                  {item.user_agent || '—'}
                </span>
              </Field>
              <Field label="Stop reason">{item.stop_reason || '—'}</Field>
              <Field label="Input tokens">{formatCompactNumber(item.input_tokens)}</Field>
              <Field label="Output tokens">
                {formatCompactNumber(item.output_tokens)}
              </Field>
              <Field label="Cache read tokens">
                {formatCompactNumber(item.cache_read_tokens)}
              </Field>
              <Field label="Cache write tokens">
                {formatCompactNumber(item.cache_creation_tokens)}
              </Field>
              <Field label="Provider request ID" wide>
                <span className="font-mono text-xs break-all">
                  {item.provider_request_id || '—'}
                </span>
              </Field>
              {item.error ? (
                <Field label="Error" wide>
                  <span className="text-sev-high">{item.error}</span>
                </Field>
              ) : null}
              <Field label="Request ID" wide>
                <span className="font-mono text-xs break-all">{item.request_id}</span>
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
              <>
                <PromptSection
                  messages={detailQuery.data.messages}
                  system={detailQuery.data.system}
                  tools={detailQuery.data.tools}
                  truncated={detailQuery.data.truncated}
                />
                <BodiesSection
                  requestBody={detailQuery.data.request_body}
                  responseBody={detailQuery.data.response_body}
                  truncated={detailQuery.data.truncated}
                  unavailableReason={detailQuery.data.bodies_unavailable}
                />
              </>
            ) : null}
          </div>
        ) : null}
      </SheetContent>
    </Sheet>
  )
}
