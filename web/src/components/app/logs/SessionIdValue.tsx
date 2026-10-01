import { Link } from 'react-router-dom'
import { CopyButton } from '@/components/app/CopyButton'

/** A log's session id as a link into the Session Timeline page (which reads
 * `?session_id=` on load), with a copy button beside it. Renders an em dash
 * when the call carried no session. */
export function SessionIdValue({ sessionId }: { sessionId: string }) {
  if (!sessionId) return <span className="text-text-subtle">—</span>
  return (
    <span className="inline-flex max-w-full items-center gap-1.5">
      <Link
        to={`/session-timeline?session_id=${encodeURIComponent(sessionId)}`}
        title="Open session timeline"
        className="min-w-0 font-mono text-xs break-all text-text-link hover:underline"
      >
        {sessionId}
      </Link>
      <CopyButton
        value={sessionId}
        label="Copy session ID"
        size="icon-xs"
        variant="ghost"
      />
    </span>
  )
}
