import { apiFetch, ApiError } from '@/lib/api'

/**
 * Data contract for the LLM Logs page and its detail drawer. Same
 * "404 means not deployed yet" convention as `logs.ts` / `overview.ts`.
 */

export interface LlmLogItem {
  timestamp: string
  tenant_id: string
  principal: string
  key_id: string
  session_id: string
  request_id: string
  provider: string
  upstream_host: string
  model: string
  path: string
  status_code: number
  duration_ms: number
  stream: boolean
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_creation_tokens: number
  stop_reason?: string | null
  provider_request_id?: string | null
  error?: string | null
  client_ip?: string
  /** Which client family made the call ("claude-code", "cursor", "vscode",
   * "codex", "claude-desktop", "other"), or "" when the caller sent no
   * User-Agent at all. */
  client_name?: string
  /** The caller's raw User-Agent header, capped server-side at 256 chars. */
  user_agent?: string
  /** "gateway" (the gateway proxied the call itself) or "interceptor" (the
   * row was pushed in by a companion capture component via
   * `POST /api/v1/ingest`). Empty on a row written before this field
   * existed -- treat as "gateway". */
  source?: string
  /** Self-reported caller identity on an ingested row (the Claude account
   * email the interceptor captured); empty on a gateway-proxied row. */
  user?: string
}

export interface LlmLogDetail extends LlmLogItem {
  headers: Record<string, string>
  request_body?: string | null
  response_body?: string | null
  messages?: unknown
  system?: unknown
  tools?: unknown
  truncated: boolean
  /** Set when the bodies were offloaded to a body store (object storage). */
  body_ref?: string | null
  /** Why an offloaded call's bodies could not be fetched back, if they couldn't. */
  bodies_unavailable?: string | null
}

export interface LlmLogsPage {
  items: LlmLogItem[]
  total: number
}

/** Text-input filter-bar fields (excludes `from`/`to`, which are set via
 * the time-range quick picks instead). */
export type LlmLogFilterKey =
  'model' | 'session_id' | 'principal' | 'client_name' | 'status' | 'source'

export interface LlmLogFilters extends Partial<Record<LlmLogFilterKey, string>> {
  from?: string
  to?: string
}

/** The gateway's named client families (pkg/sink.ClientFamily), plus "Any"
 * to clear the filter -- same value set the Client column's badge uses. */
export const CLIENT_NAME_OPTIONS: { value: string; label: string }[] = [
  { value: '', label: 'Any' },
  { value: 'claude-code', label: 'Claude Code' },
  { value: 'claude-desktop', label: 'Claude Desktop' },
  { value: 'cursor', label: 'Cursor' },
  { value: 'vscode', label: 'VS Code' },
  { value: 'codex', label: 'Codex' },
  { value: 'other', label: 'Other' },
]

/** "All", "Gateway" (proxied traffic), or "Interceptor" (ingested via
 * `POST /api/v1/ingest`) -- same value set the User column's badge uses. */
export const SOURCE_OPTIONS: { value: string; label: string }[] = [
  { value: '', label: 'All sources' },
  { value: 'gateway', label: 'Gateway' },
  { value: 'interceptor', label: 'Interceptor' },
]

/** Filter bar field order + labels for the LLM Logs page. */
export const LLM_LOG_FILTER_FIELDS: {
  key: LlmLogFilterKey
  label: string
  options?: { value: string; label: string }[]
}[] = [
  { key: 'model', label: 'Model' },
  { key: 'session_id', label: 'Session ID' },
  { key: 'principal', label: 'Principal' },
  { key: 'client_name', label: 'Client', options: CLIENT_NAME_OPTIONS },
  { key: 'status', label: 'Status' },
  { key: 'source', label: 'Source', options: SOURCE_OPTIONS },
]

function isNotDeployed(err: unknown): boolean {
  return err instanceof ApiError && err.status === 404
}

/** `GET /api/v1/analytics/llm-logs?...` */
export async function fetchLlmLogs(
  filters: LlmLogFilters,
  limit: number,
  offset: number,
): Promise<LlmLogsPage | null> {
  const params = new URLSearchParams()
  params.set('limit', String(limit))
  params.set('offset', String(offset))
  for (const [key, value] of Object.entries(filters)) {
    if (value) params.set(key, value)
  }
  try {
    return await apiFetch<LlmLogsPage>(`/analytics/llm-logs?${params.toString()}`)
  } catch (err) {
    if (isNotDeployed(err)) return null
    throw err
  }
}

/** `GET /api/v1/analytics/llm-logs/{request_id}` — admin only (403 for agents). */
export function fetchLlmLogDetail(requestId: string): Promise<LlmLogDetail> {
  return apiFetch<LlmLogDetail>(`/analytics/llm-logs/${encodeURIComponent(requestId)}`)
}
