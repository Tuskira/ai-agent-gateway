import { apiFetch } from '@/lib/api'

/** Data contract for the Credentials page. Values are write-only — the API
 * never echoes them back, only field names and metadata. */

interface Page<T> {
  items: T[]
  total: number
}

export interface CredentialUsedBy {
  id: string
  name: string
}

export interface Credential {
  id: string
  name: string
  type: string
  field_names: string[]
  key_id: string
  used_by?: CredentialUsedBy[]
  created_at: string
  rotated_at?: string | null
}

export interface CreateCredentialInput {
  name: string
  type: string
  payload: Record<string, string>
}

export interface UpdateCredentialInput {
  payload: Record<string, string>
}

/** `GET /api/v1/credentials` */
export function listCredentials(): Promise<Page<Credential>> {
  return apiFetch<Page<Credential>>('/credentials')
}

/** `GET /api/v1/credentials/{name}` */
export function getCredential(name: string): Promise<Credential> {
  return apiFetch<Credential>(`/credentials/${encodeURIComponent(name)}`)
}

/** `POST /api/v1/credentials` */
export function createCredential(input: CreateCredentialInput): Promise<Credential> {
  return apiFetch<Credential>('/credentials', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

/** `PUT /api/v1/credentials/{name}` — rotates the stored value(s). */
export function updateCredential(
  name: string,
  input: UpdateCredentialInput,
): Promise<Credential> {
  return apiFetch<Credential>(`/credentials/${encodeURIComponent(name)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

/** `DELETE /api/v1/credentials/{name}` */
export function deleteCredential(name: string): Promise<void> {
  return apiFetch<void>(`/credentials/${encodeURIComponent(name)}`, { method: 'DELETE' })
}
