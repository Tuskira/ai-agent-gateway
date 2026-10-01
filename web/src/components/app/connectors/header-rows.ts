import type { HeaderConfig } from '@/lib/connectors'

export type HeaderRowType = HeaderConfig['type']

/** Local, non-react-hook-form editable row — the shape of a single
 * connector auth header while it's being edited. `config` is only used
 * when `type === 'external'`, keyed by the selected provider's
 * `config_schema.properties`. */
export interface HeaderRowState {
  id: string
  headerName: string
  type: HeaderRowType
  value: string
  field: string
  incomingHeader: string
  provider: string
  config: Record<string, string>
}

export function newHeaderRow(): HeaderRowState {
  return {
    id: crypto.randomUUID(),
    headerName: '',
    type: 'static',
    value: '',
    field: '',
    incomingHeader: '',
    provider: '',
    config: {},
  }
}

/** Converts a connector's `metadata.headers` map (API shape) into editable
 * rows. Masked secret values (`"***"`) are carried through as-is — the
 * value input round-trips them unchanged unless the person types over it. */
export function headersToRows(headers?: Record<string, HeaderConfig>): HeaderRowState[] {
  if (!headers) return []
  return Object.entries(headers).map(([headerName, cfg]) => {
    const base = newHeaderRow()
    base.headerName = headerName
    base.type = cfg.type
    if (cfg.type === 'static') base.value = cfg.value
    if (cfg.type === 'token_field') base.field = cfg.field
    if (cfg.type === 'incoming_field') base.incomingHeader = cfg.header
    if (cfg.type === 'external') {
      base.provider = cfg.provider
      base.config = { ...cfg.config }
    }
    return base
  })
}

export function rowsToHeaders(
  rows: HeaderRowState[],
): Record<string, HeaderConfig> | undefined {
  const headers: Record<string, HeaderConfig> = {}
  for (const row of rows) {
    const name = row.headerName.trim()
    if (!name) continue
    if (row.type === 'static') headers[name] = { type: 'static', value: row.value }
    else if (row.type === 'token_field')
      headers[name] = { type: 'token_field', field: row.field }
    else if (row.type === 'incoming_field')
      headers[name] = { type: 'incoming_field', header: row.incomingHeader }
    else headers[name] = { type: 'external', provider: row.provider, config: row.config }
  }
  return Object.keys(headers).length > 0 ? headers : undefined
}

/** Whether a row has everything it needs to be sent to the API. Used to
 * block submit and show inline errors, without a full zod schema for this
 * genuinely heterogeneous, provider-driven shape. */
export function headerRowError(row: HeaderRowState): string | null {
  if (!row.headerName.trim()) return 'Header name is required'
  if (row.type === 'static' && !row.value.trim()) return 'Value is required'
  if (row.type === 'token_field' && !row.field.trim()) return 'Token field is required'
  if (row.type === 'incoming_field' && !row.incomingHeader.trim())
    return 'Incoming header name is required'
  if (row.type === 'external' && !row.provider.trim()) return 'Provider is required'
  return null
}
