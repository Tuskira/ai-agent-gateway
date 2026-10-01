import { apiFetch } from '@/lib/api'

/** Data contract for the API Keys page (admin-only). */

interface Page<T> {
  items: T[]
  total: number
}

/** `interceptor` grants only `ingest.write` (`POST /api/v1/ingest`) — the
 * role to mint for the capture component that feeds ingest. See
 * docs/security-model.md, "Identity: keys, roles, permissions". */
export type ApiKeyRole = 'admin' | 'agent' | 'interceptor'

export interface ApiKey {
  id: string
  name: string
  role: ApiKeyRole
  prefix: string
  created_at: string
  last_used_at?: string | null
  expires_at?: string | null
  revoked_at?: string | null
  /** Agent profile the key is bound to (enforced by the MCP plane). */
  profile_id?: string
  profile_name?: string
}

/** Returned once, at creation or rotation — carries the full plaintext key. */
export interface ApiKeyCreated extends ApiKey {
  key: string
}

export interface CreateApiKeyInput {
  name: string
  role: ApiKeyRole
  expires_at?: string
  /** Bind the key to a profile of this tenant. */
  profile_id?: string
}

/** `GET /api/v1/api-keys` */
export function listApiKeys(): Promise<Page<ApiKey>> {
  return apiFetch<Page<ApiKey>>('/api-keys')
}

/** `POST /api/v1/api-keys` */
export function createApiKey(input: CreateApiKeyInput): Promise<ApiKeyCreated> {
  return apiFetch<ApiKeyCreated>('/api-keys', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

/** `DELETE /api/v1/api-keys/{id}` (revoke) */
export function deleteApiKey(id: string): Promise<void> {
  return apiFetch<void>(`/api-keys/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/** `POST /api/v1/api-keys/{id}/rotate` */
export function rotateApiKey(id: string): Promise<ApiKeyCreated> {
  return apiFetch<ApiKeyCreated>(`/api-keys/${encodeURIComponent(id)}/rotate`, {
    method: 'POST',
  })
}
