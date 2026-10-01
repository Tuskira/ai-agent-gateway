import { lazy, Suspense, useMemo } from 'react'
import type { DialogRootChangeEventDetails } from '@base-ui/react/dialog'
import { decodeBodyText, formatBytes, prettyJson } from '@/lib/logs'
import { CopyButton } from '@/components/app/CopyButton'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { Skeleton } from '@/components/ui/skeleton'

// The viewer brings in CodeMirror; load it only once a body is opened.
const JsonViewer = lazy(() => import('@/components/app/logs/JsonViewer'))

interface BodyDialogProps {
  title: string
  value: string
  truncated: boolean
}

/** Escape clears the viewer's search first, and only then closes the dialog. */
function keepOpenWhileFinding(_open: boolean, details: DialogRootChangeEventDetails) {
  if (details.reason !== 'escape-key') return
  const { target } = details.event
  if (
    target instanceof HTMLInputElement &&
    'jsonFind' in target.dataset &&
    target.value
  ) {
    details.cancel()
  }
}

/** Button that opens one body in a dialog; disabled when there's nothing. */
function BodyDialogButton({ title, value, truncated }: BodyDialogProps) {
  return (
    <Dialog onOpenChange={keepOpenWhileFinding}>
      <DialogTrigger
        render={
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={!value}
            className="uppercase"
          />
        }
      >
        {title}
      </DialogTrigger>
      <DialogContent className="flex h-[85vh] flex-col sm:max-w-5xl">
        <BodyDialogContent title={title} value={value} truncated={truncated} />
      </DialogContent>
    </Dialog>
  )
}

/** Only mounted while the dialog is open, so multi-MB bodies aren't
 * re-parsed on every drawer render. Copy yields the body exactly as
 * captured, not the indented view. */
function BodyDialogContent({ title, value, truncated }: BodyDialogProps) {
  const text = useMemo(() => prettyJson(value, truncated), [value, truncated])
  const bytes = useMemo(() => new TextEncoder().encode(value).length, [value])
  return (
    <>
      <DialogHeader className="flex-row items-center gap-2 pr-8">
        <DialogTitle>{title}</DialogTitle>
        <Badge variant="outline" className="text-text-muted">
          {formatBytes(bytes)}
        </Badge>
        <CopyButton value={value} label={`Copy ${title.toLowerCase()}`} />
      </DialogHeader>
      <Suspense fallback={<Skeleton className="min-h-0 flex-1" />}>
        <JsonViewer value={text} label={title} />
      </Suspense>
    </>
  )
}

/** Captured text of one prompt part. String values are base64 from the
 * API, so decode them first. */
function promptText(value: unknown): string {
  if (value === undefined || value === null) return ''
  return typeof value === 'string' ? decodeBodyText(value) : JSON.stringify(value)
}

interface PromptSectionProps {
  messages: unknown
  system: unknown
  tools: unknown
  truncated: boolean
}

/** The parsed-out prompt parts of an LLM call, each behind a button like
 * the bodies. Left out entirely when none were captured. */
export function PromptSection({
  messages,
  system,
  tools,
  truncated,
}: PromptSectionProps) {
  const messagesText = useMemo(() => promptText(messages), [messages])
  const systemText = useMemo(() => promptText(system), [system])
  const toolsText = useMemo(() => promptText(tools), [tools])
  if (!messagesText && !systemText && !toolsText) return null

  return (
    <div>
      <div className="mb-2 text-xs font-semibold text-text-subtle uppercase">Prompt</div>
      <div className="flex flex-wrap gap-2">
        <BodyDialogButton title="Messages" value={messagesText} truncated={truncated} />
        <BodyDialogButton title="System" value={systemText} truncated={truncated} />
        <BodyDialogButton title="Tools" value={toolsText} truncated={truncated} />
      </div>
    </div>
  )
}

interface BodiesSectionProps {
  requestBody: string | null | undefined
  responseBody: string | null | undefined
  truncated: boolean
  /** Replaces the "not captured" sentence when bodies were captured but
   * can't be shown (e.g. offloaded to a body store this instance can't read). */
  unavailableReason?: string | null
}

/** Request/response body buttons shown last in every log detail drawer.
 * Shows the "not captured" sentence once when neither side has anything,
 * per the `capture.store_bodies` toggle. */
export function BodiesSection({
  requestBody,
  responseBody,
  truncated,
  unavailableReason,
}: BodiesSectionProps) {
  const reqText = useMemo(() => decodeBodyText(requestBody), [requestBody])
  const resText = useMemo(() => decodeBodyText(responseBody), [responseBody])
  const bothEmpty = !reqText && !resText

  return (
    <div>
      <div className="mb-2 flex items-center justify-between">
        <span className="text-xs font-semibold text-text-subtle uppercase">Bodies</span>
        {truncated ? (
          <Badge variant="outline" className="border-sev-medium text-sev-medium-fg">
            Truncated
          </Badge>
        ) : null}
      </div>
      {bothEmpty && unavailableReason ? (
        <p className="mb-2 text-xs text-text-subtle">
          Bodies unavailable: {unavailableReason}.
        </p>
      ) : bothEmpty ? (
        <p className="mb-2 text-xs text-text-subtle">
          Bodies not captured (capture.store_bodies is off).
        </p>
      ) : null}
      <div className="flex flex-wrap gap-2">
        <BodyDialogButton title="Request body" value={reqText} truncated={truncated} />
        <BodyDialogButton title="Response body" value={resText} truncated={truncated} />
      </div>
    </div>
  )
}
