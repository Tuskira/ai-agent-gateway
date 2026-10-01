import { apiFetch, ApiError } from '@/lib/api'

/**
 * Data contract for the Access Logs page and its detail drawer.
 *
 * The `/analytics/logs` family ships alongside the ClickHouse sink and may
 * not be deployed yet in every environment — a 404 there means exactly
 * that, not a real error. `fetchAccessLogs` resolves to `null` in that case
 * so the page can render an honest "requires the ClickHouse sink" empty
 * state instead of throwing, same convention as `overview.ts` / `cache.ts`.
 */

export interface AccessLogItem {
  timestamp: string
  tenant_id: string
  principal: string
  key_id: string
  session_id: string
  request_id: string
  correlation_id: string
  trace_id: string
  method: string
  json_rpc_id?: string | number | null
  connector_id: string
  tool_name: string
  status_code: number
  error_code?: string | null
  duration_ms: number
  bytes_in: number
  bytes: number
  client_ip: string
  user_agent: string
  profile: string
  /** "gateway" (the gateway proxied the call itself) or "interceptor" (the
   * row was pushed in by a companion capture component via
   * `POST /api/v1/ingest`). Empty on a row written before this field
   * existed -- treat as "gateway". */
  source?: string
  /** Self-reported caller identity on an ingested row (the Claude account
   * email the interceptor captured); empty on a gateway-proxied row. */
  user?: string
}

export interface AccessLogDetail extends AccessLogItem {
  headers: Record<string, string>
  request_body?: string | null
  response_body?: string | null
  truncated: boolean
}

export interface AccessLogsPage {
  items: AccessLogItem[]
  total: number
}

/** Text-input filter-bar fields (excludes `from`/`to`, which are set via
 * the time-range quick picks instead). */
export type AccessLogFilterKey =
  | 'method'
  | 'connector_id'
  | 'tool'
  | 'session_id'
  | 'principal'
  | 'status'
  | 'source'

export interface AccessLogFilters extends Partial<Record<AccessLogFilterKey, string>> {
  from?: string
  to?: string
}

/** The gateway speaks JSON-RPC (MCP) over HTTP, so `method` is one of these
 * — never an HTTP verb — plus "Any" to clear the filter. */
export const ACCESS_LOG_METHOD_OPTIONS: { value: string; label: string }[] = [
  { value: '', label: 'Any' },
  { value: 'initialize', label: 'initialize' },
  { value: 'tools/list', label: 'tools/list' },
  { value: 'tools/call', label: 'tools/call' },
]

/** "All", "Gateway" (proxied traffic), or "Interceptor" (ingested via
 * `POST /api/v1/ingest`) -- same value set the User column's badge uses. */
export const SOURCE_OPTIONS: { value: string; label: string }[] = [
  { value: '', label: 'All sources' },
  { value: 'gateway', label: 'Gateway' },
  { value: 'interceptor', label: 'Interceptor' },
]

/** Filter bar field order + labels for the Access Logs page (mock parity). */
export const ACCESS_LOG_FILTER_FIELDS: {
  key: AccessLogFilterKey
  label: string
  options?: { value: string; label: string }[]
}[] = [
  { key: 'method', label: 'Method', options: ACCESS_LOG_METHOD_OPTIONS },
  { key: 'connector_id', label: 'MCP ID' },
  { key: 'tool', label: 'Tool Name' },
  { key: 'session_id', label: 'Session ID' },
  { key: 'principal', label: 'User ID' },
  { key: 'status', label: 'Status Code' },
  { key: 'source', label: 'Source', options: SOURCE_OPTIONS },
]

function isNotDeployed(err: unknown): boolean {
  return err instanceof ApiError && err.status === 404
}

/** `GET /api/v1/analytics/logs?...` */
export async function fetchAccessLogs(
  filters: AccessLogFilters,
  limit: number,
  offset: number,
): Promise<AccessLogsPage | null> {
  const params = new URLSearchParams()
  params.set('limit', String(limit))
  params.set('offset', String(offset))
  for (const [key, value] of Object.entries(filters)) {
    if (value) params.set(key, value)
  }
  try {
    return await apiFetch<AccessLogsPage>(`/analytics/logs?${params.toString()}`)
  } catch (err) {
    if (isNotDeployed(err)) return null
    throw err
  }
}

/** `GET /api/v1/analytics/logs/{request_id}` — admin only (403 for agents). */
export function fetchAccessLogDetail(requestId: string): Promise<AccessLogDetail> {
  return apiFetch<AccessLogDetail>(`/analytics/logs/${encodeURIComponent(requestId)}`)
}

/** Duration above this is flagged "slow" everywhere, per the mock's legend. */
export const SLOW_DURATION_MS = 5000

/** "412 B" / "3.4 KB" / "1.28 MB" */
export function formatBytes(n: number | null | undefined): string {
  if (n === null || n === undefined || Number.isNaN(n)) return '—'
  if (n < 1024) return `${n} B`
  const kb = n / 1024
  if (kb < 1024) return `${kb.toFixed(1)} KB`
  return `${(kb / 1024).toFixed(2)} MB`
}

/** True unless `s` contains an ASCII control character other than
 * tab/newline/carriage-return — a cheap "is this actually text" check for
 * a base64 decode that might really be binary. */
function isPrintableText(s: string): boolean {
  for (let i = 0; i < s.length; i++) {
    const code = s.charCodeAt(i)
    const isControl = code < 0x20 && code !== 0x09 && code !== 0x0a && code !== 0x0d
    if (isControl || code === 0x7f) return false
  }
  return true
}

/**
 * The gateway may send captured bodies as raw text or base64. There's no
 * explicit flag distinguishing the two, so decode defensively: only trust
 * the base64 decode when it round-trips to printable text, otherwise show
 * the original string untouched.
 */
export function decodeBodyText(raw: string | null | undefined): string {
  if (!raw) return ''
  try {
    // atob yields bytes; fatal UTF-8 decode keeps non-ASCII prompts intact
    // and rejects binary. stream: a byte-limit cut can split the last char.
    const bytes = Uint8Array.from(atob(raw), (c) => c.charCodeAt(0))
    const decoded = new TextDecoder('utf-8', { fatal: true }).decode(bytes, {
      stream: true,
    })
    if (isPrintableText(decoded)) return decoded
  } catch {
    // not valid base64 / UTF-8 — fall through to the raw string
  }
  return raw
}

const isJsonSpace = (c: string | undefined) =>
  c === ' ' || c === '\n' || c === '\r' || c === '\t'

/** Re-indents valid JSON text by rewriting whitespace only. Unlike a
 * JSON.parse/stringify round trip it keeps large integers exact, key order
 * and duplicate keys as captured. */
export function reindentJson(text: string): string {
  let out = ''
  let depth = 0
  let inString = false
  const newline = () => '\n' + '  '.repeat(depth)
  for (let i = 0; i < text.length; i++) {
    const c = text[i]
    if (inString) {
      out += c
      if (c === '\\') out += text[++i] ?? ''
      else if (c === '"') inString = false
      continue
    }
    if (isJsonSpace(c)) continue
    if (c === '{' || c === '[') {
      let j = i + 1
      while (isJsonSpace(text[j])) j++
      if (text[j] === '}' || text[j] === ']') {
        out += c + text[j]
        i = j
        continue
      }
      depth++
      out += c + newline()
    } else if (c === '}' || c === ']') {
      depth--
      out += newline() + c
    } else if (c === ',') {
      out += ',' + newline()
    } else if (c === ':') {
      out += ': '
    } else {
      if (c === '"') inString = true
      out += c
    }
  }
  return out
}

/** Pretty-prints JSON text; anything that doesn't parse is shown as-is. A
 * JSON string (e.g. a plain `system` prompt) is shown unquoted. Parsing
 * only validates; the output is re-indented from the original text. A
 * `truncated` capture is cut mid-document and never parses, so one that
 * opens like JSON is re-indented anyway rather than left on one line. */
export function prettyJson(text: string, truncated = false): string {
  try {
    const v: unknown = JSON.parse(text)
    return typeof v === 'string' ? v : reindentJson(text)
  } catch {
    const first = text.trimStart()[0]
    return truncated && (first === '{' || first === '[') ? reindentJson(text) : text
  }
}
