import { apiFetch, ApiError } from '@/lib/api'

/**
 * Data contract for the Connectors page and its create/edit dialog + detail
 * drawer. See `web/README.md` / the phase-1 backend contract for the
 * `/connectors` family of endpoints.
 */

interface Page<T> {
  items: T[]
  total: number
}

export type ConnectorStatus = 'healthy' | 'unhealthy' | 'unknown' | string

/** A single authentication header sent with every request to the backend
 * MCP server. Secret-bearing values (`static.value`) come back masked as
 * `"***"` on GET/list — the edit form must leave a masked value untouched
 * unless the person types a new one. */
export type HeaderConfig =
  | { type: 'static'; value: string }
  | { type: 'token_field'; field: string }
  | { type: 'incoming_field'; header: string }
  | { type: 'external'; provider: string; config: Record<string, string> }

export interface TlsConfig {
  insecure_skip_verify?: boolean
}

/** `{ [toolName]: { [argName]: value } }` — pins argument values for a
 * specific cached tool so callers don't have to (or can't) supply them. */
export type ToolArgOverrides = Record<string, Record<string, string>>

/** Per-connector policy for server-initiated MCP requests: whether this
 * backend may ask the gateway to relay a `sampling/createMessage`,
 * `elicitation/create`, or `roots/list` request back to the agent. Every
 * field defaults to `false` when absent — these are trust decisions, off
 * until explicitly turned on. */
export interface ServerRequestPolicy {
  sampling?: boolean
  elicitation?: boolean
  roots?: boolean
}

export interface ConnectorMetadata {
  /** `false` turns the connector off: the gateway neither lists nor calls it.
   * Absent means enabled. */
  enabled?: boolean
  headers?: Record<string, HeaderConfig>
  retry?: Record<string, unknown>
  tool_arg_overrides?: ToolArgOverrides
  tls?: TlsConfig
  server_requests?: ServerRequestPolicy
}

/** MCP `initialize` handshake result, cached on the connector after the
 * gateway last talked to it. Any field can be missing on an older gateway
 * or before the first successful handshake — render "—", never fabricate. */
export interface ConnectorCapabilities {
  protocol_version?: string
  server_info?: { name?: string; version?: string }
  tools?: boolean | Record<string, unknown>
  resources?: boolean | Record<string, unknown>
  prompts?: boolean | Record<string, unknown>
}

export interface Connector {
  id: string
  name: string
  slug: string
  description?: string
  endpoint: string
  timeout_ms: number
  status: ConnectorStatus
  capabilities?: ConnectorCapabilities
  /** Set when this connector was added from the MCP catalog. */
  catalog_id?: string
  metadata: ConnectorMetadata
  created_at: string
  updated_at: string
}

export interface ConnectorTool {
  connector_id: string
  tool_name: string
  tool_namespace?: string
  description?: string
  input_schema?: unknown
}

export interface ConnectorHealth {
  status: ConnectorStatus
  latency_ms?: number
  capabilities?: Record<string, unknown>
  checked_at?: string
  error?: string
}

export interface ConnectorInput {
  name: string
  slug?: string
  description?: string
  endpoint: string
  timeout_ms?: number
  metadata?: ConnectorMetadata
}

/** `GET /api/v1/connectors` */
export function listConnectors(): Promise<Page<Connector>> {
  return apiFetch<Page<Connector>>('/connectors')
}

/** `GET /api/v1/connectors/{id}` */
export function getConnector(id: string): Promise<Connector> {
  return apiFetch<Connector>(`/connectors/${encodeURIComponent(id)}`)
}

/** `POST /api/v1/connectors` */
export function createConnector(input: ConnectorInput): Promise<Connector> {
  return apiFetch<Connector>('/connectors', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

/** `PUT /api/v1/connectors/{id}` */
export function updateConnector(id: string, input: ConnectorInput): Promise<Connector> {
  return apiFetch<Connector>(`/connectors/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

/** `DELETE /api/v1/connectors/{id}` */
export function deleteConnector(id: string): Promise<void> {
  return apiFetch<void>(`/connectors/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/** `GET /api/v1/connectors/{id}/tools` */
export function listConnectorTools(id: string): Promise<Page<ConnectorTool>> {
  return apiFetch<Page<ConnectorTool>>(`/connectors/${encodeURIComponent(id)}/tools`)
}

/**
 * `GET /api/v1/connectors/{id}/health` — may 501/503 until the ops PR
 * merges. Callers get `null` in that case instead of a thrown error, so the
 * "Check health" action can show a clear "not available yet" state rather
 * than crashing.
 */
export async function checkConnectorHealth(id: string): Promise<ConnectorHealth | null> {
  try {
    return await apiFetch<ConnectorHealth>(`/connectors/${encodeURIComponent(id)}/health`)
  } catch (err) {
    if (err instanceof ApiError && (err.status === 501 || err.status === 503)) return null
    throw err
  }
}

/** `POST /api/v1/connectors/{id}/discover` */
export function discoverConnectorTools(id: string): Promise<Page<ConnectorTool>> {
  return apiFetch<Page<ConnectorTool>>(`/connectors/${encodeURIComponent(id)}/discover`, {
    method: 'POST',
  })
}

/** The literal placeholder the gateway substitutes for a secret header value
 * it won't echo back. */
export const MASKED_HEADER_VALUE = '***'
