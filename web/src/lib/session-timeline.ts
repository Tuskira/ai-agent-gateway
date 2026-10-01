import { apiFetch, ApiError } from '@/lib/api'

/**
 * Data contract for the Session Timeline page.
 *
 * `GET /api/v1/analytics/sessions/{session_id}/timeline` merges the MCP and
 * LLM planes server-side under the ownership rule (see
 * docs/observability.md#session-timeline-ownership): the returned `events`
 * already exclude any other key's calls, so this page never has to
 * reconstruct a session from raw `/analytics/logs` + `/analytics/llm-logs`
 * rows (and can't be tricked into showing another key's traffic by doing
 * so).
 *
 * Like the rest of the analytics surface, a 404 here means "this instance
 * has no ClickHouse sink configured", not a real error — `fetchSessionTimeline`
 * resolves to `null` in that case, same convention as `overview.ts` / `logs.ts`.
 */

export type TimelinePlane = 'mcp' | 'llm'
export type TimelineStatus = 'success' | 'error' | 'notification'

export interface TimelineEvent {
  ts: string
  plane: TimelinePlane
  /** MCP: the JSON-RPC method (tools/call, initialize, ...). LLM: "llm_call". */
  kind: string
  /** MCP: the tool name (tools/call only, else ""). LLM: the model. */
  name: string
  status: TimelineStatus
  duration_ms: number
  key_id: string
  /** The event's request_id. */
  id: string
}

/** Direction the server sorts events in, by (ts, id). The UI never sorts. */
export type TimelineOrder = 'asc' | 'desc'

export const TIMELINE_ORDER_STORAGE_KEY = 'tusk.sessionTimeline.order'

/** The last order the viewer chose in this browser ('asc' when none or when
 * storage is unavailable). */
export function readStoredTimelineOrder(): TimelineOrder {
  try {
    return window.localStorage.getItem(TIMELINE_ORDER_STORAGE_KEY) === 'desc' ? 'desc' : 'asc'
  } catch {
    return 'asc'
  }
}

export function storeTimelineOrder(order: TimelineOrder): void {
  try {
    window.localStorage.setItem(TIMELINE_ORDER_STORAGE_KEY, order)
  } catch {
    // Storage blocked (private window): the URL param still carries it.
  }
}

export interface SessionTimeline {
  session_id: string
  owner_key_id: string
  /** Echo of the order the server applied. */
  order?: TimelineOrder
  events: TimelineEvent[]
  total_events: number
  /** Events that matched this session by tag but belonged to a different
   * key, withheld from `events` rather than relabeled — see the ownership
   * rule linked above. */
  excluded_foreign_events: number
}

function isNotDeployed(err: unknown): boolean {
  return err instanceof ApiError && err.status === 404
}

/** `GET /api/v1/analytics/sessions/{session_id}/timeline` */
export async function fetchSessionTimeline(
  sessionId: string,
  order: TimelineOrder = 'asc',
): Promise<SessionTimeline | null> {
  try {
    return await apiFetch<SessionTimeline>(
      `/analytics/sessions/${encodeURIComponent(sessionId)}/timeline?order=${order}`,
    )
  } catch (err) {
    if (isNotDeployed(err)) return null
    throw err
  }
}
