import { Lock } from 'lucide-react'

/** Shown in place of headers/bodies when the signed-in principal is an
 * agent, not an admin — the detail endpoint 403s for anyone else, so the
 * drawer never even attempts the request. */
export function AdminOnlyNotice() {
  return (
    <div className="flex items-center gap-2.5 rounded-r-4 border border-dashed border-border bg-bg-subtle p-3 text-xs text-text-subtle">
      <Lock className="size-4 shrink-0" aria-hidden="true" />
      Admin only — sign in with an admin API key to view headers and captured bodies.
    </div>
  )
}
