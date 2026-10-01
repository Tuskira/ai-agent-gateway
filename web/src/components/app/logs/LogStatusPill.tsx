import { httpStatusTone } from '@/lib/httpStatus'
import { StatusPill } from '@/components/app/StatusPill'

interface LogStatusPillProps {
  statusCode: number
  /**
   * A JSON-RPC tool call can be denied (or otherwise fail) while still
   * riding back on HTTP 200 — a green "200" pill would hide that entirely.
   * When set, this overrides the pill with a red one instead of the
   * HTTP-status tone: the literal error code for Access Logs, or the fixed
   * word "error" for LLM Logs (whose error text is unbounded free text).
   */
  errorLabel?: string | null
}

/** Status pill shared by the Access Logs / LLM Logs tables and their
 * detail-drawer headers, so a denied call reads as an error everywhere,
 * not just once you open the drawer. */
export function LogStatusPill({ statusCode, errorLabel }: LogStatusPillProps) {
  if (errorLabel) {
    return <StatusPill tone="negative" label={errorLabel} />
  }
  return <StatusPill tone={httpStatusTone(statusCode)} label={String(statusCode)} />
}
