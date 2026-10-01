import { apiFetch } from '@/lib/api'
import type { Connector } from '@/lib/connectors'

/**
 * Data contract for the MCP catalog: platform-curated MCP servers a tenant
 * admin can add to their tenant (`GET /api/v1/mcp-catalog`,
 * `POST /api/v1/mcp-catalog/{slug}/add`). Nothing here is callable until it
 * is added, which creates an ordinary connector.
 */

export type CatalogAuthKind = 'none' | 'bearer' | 'basic' | 'header' | 'oauth'

export interface CatalogField {
  name: string
  label: string
  secret?: boolean
  required?: boolean
  placeholder?: string
  help?: string
  /** Set when the value goes into the URL query instead of a credential. */
  query?: string
}

export interface CatalogHeaderTemplate {
  name?: string
  prefix?: string
}

export interface CatalogAuth {
  kind: CatalogAuthKind
  fields: CatalogField[]
  header_template?: CatalogHeaderTemplate
}

export interface CatalogEntry {
  id: string
  slug: string
  name: string
  description: string
  icon: string
  category: string
  url: string
  url_overridable: boolean
  transport: string
  auth: CatalogAuth
  default_headers?: Record<string, string>
  suggested_tools: string[]
  docs_url?: string
  enabled: boolean
  /** `platform` entries are shared by every tenant and read-only here. */
  scope: 'platform' | 'tenant'
  /** False when the gateway cannot add this entry yet (OAuth). */
  supported: boolean
  unsupported_reason?: string
  /** True when this tenant already has a connector from this entry. */
  added: boolean
  connector_id?: string
}

/** Body of `POST /mcp-catalog` (with `slug`) and `PUT /mcp-catalog/{slug}`. */
export interface CatalogEntryInput {
  slug?: string
  name: string
  description: string
  icon: string
  category: string
  url: string
  url_overridable: boolean
  auth: CatalogAuth
  default_headers: Record<string, string>
  suggested_tools: string[]
  docs_url: string
  enabled: boolean
}

export interface AddCatalogInput {
  name?: string
  slug?: string
  fields?: Record<string, string>
  url?: string
}

export interface AddCatalogResult {
  connector: Connector
  discovery: {
    status?: string
    error?: string
    tools_discovered: number
    discovery_error?: string
  }
}

interface Page<T> {
  items: T[]
  total: number
}

/** `GET /api/v1/mcp-catalog` */
export function listCatalog(): Promise<Page<CatalogEntry>> {
  return apiFetch<Page<CatalogEntry>>('/mcp-catalog')
}

/** `POST /api/v1/mcp-catalog/{slug}/add` */
export function addCatalogEntry(
  slug: string,
  input: AddCatalogInput,
): Promise<AddCatalogResult> {
  return apiFetch<AddCatalogResult>(`/mcp-catalog/${encodeURIComponent(slug)}/add`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

/** Human label for an auth kind, for the catalog cards. */
export function authKindLabel(kind: CatalogAuthKind): string {
  switch (kind) {
    case 'none':
      return 'No auth'
    case 'bearer':
      return 'Bearer token'
    case 'basic':
      return 'Basic auth'
    case 'header':
      return 'API key header'
    case 'oauth':
      return 'OAuth'
  }
}

/**
 * Builds the add request body from the dialog's state: blank field values
 * are dropped (the server treats them as omitted optional fields), and the
 * URL is sent only when the person changed it from the entry's default.
 */
export function buildAddBody(
  entry: CatalogEntry,
  values: Record<string, string>,
  name: string,
  url: string,
): AddCatalogInput {
  const fields: Record<string, string> = {}
  for (const f of entry.auth.fields) {
    const v = (values[f.name] ?? '').trim()
    if (v !== '') fields[f.name] = v
  }
  const body: AddCatalogInput = { fields }
  const trimmedName = name.trim()
  if (trimmedName !== '' && trimmedName !== entry.name) body.name = trimmedName
  const trimmedUrl = url.trim()
  if (entry.url_overridable && trimmedUrl !== '' && trimmedUrl !== entry.url) {
    body.url = trimmedUrl
  }
  return body
}

/** `POST /api/v1/mcp-catalog`: a new entry in this tenant's catalog. */
export function createCatalogEntry(input: CatalogEntryInput): Promise<CatalogEntry> {
  return apiFetch<CatalogEntry>('/mcp-catalog', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

/** `PUT /api/v1/mcp-catalog/{slug}?scope=tenant`: full replace of a tenant entry. */
export function updateCatalogEntry(
  slug: string,
  input: CatalogEntryInput,
): Promise<CatalogEntry> {
  const { slug: _ignored, ...body } = input
  void _ignored
  return apiFetch<CatalogEntry>(`/mcp-catalog/${encodeURIComponent(slug)}?scope=tenant`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

/** `DELETE /api/v1/mcp-catalog/{slug}?scope=tenant` */
export function deleteCatalogEntry(slug: string): Promise<void> {
  return apiFetch<void>(`/mcp-catalog/${encodeURIComponent(slug)}?scope=tenant`, {
    method: 'DELETE',
  })
}

/** Starting point for the edit dialog: the entry's own values as a request body. */
export function entryToInput(entry: CatalogEntry): CatalogEntryInput {
  return {
    name: entry.name,
    description: entry.description,
    icon: entry.icon,
    category: entry.category,
    url: entry.url,
    url_overridable: entry.url_overridable,
    auth: entry.auth,
    default_headers: entry.default_headers ?? {},
    suggested_tools: entry.suggested_tools,
    docs_url: entry.docs_url ?? '',
    enabled: entry.enabled,
  }
}

/** How many credential fields an auth kind takes (query fields excluded). */
export function credentialFieldCount(kind: CatalogAuthKind): number {
  switch (kind) {
    case 'bearer':
    case 'header':
      return 1
    case 'basic':
      return 2
    default:
      return 0
  }
}
