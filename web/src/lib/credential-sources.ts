import { apiFetch } from '@/lib/api'
import type { Page } from '@/lib/credentials'

/** Data contract for credential sources: a tenant's registrations of
 * outside credential services that connector headers resolve secrets from.
 * A source holds only non-secret settings. */

export interface CredentialSourceUsedBy {
  id: string
  name: string
}

export interface CredentialSource {
  id: string
  name: string
  provider: string
  /** Non-secret settings shaped by the provider's `source_schema`. */
  config: Record<string, unknown>
  enabled: boolean
  /** Connectors whose headers name this source. */
  used_by: CredentialSourceUsedBy[]
  created_at: string
  updated_at: string
}

export interface CreateCredentialSourceInput {
  name: string
  provider: string
  config: Record<string, string>
  enabled?: boolean
}

export interface UpdateCredentialSourceInput {
  name?: string
  config?: Record<string, string>
  enabled?: boolean
}

/** `POST /credential-sources/{id}/test` — a failure is `ok: false`, not an
 * HTTP error. */
export interface CredentialSourceTestResult {
  ok: boolean
  error?: string
}

export interface CredentialSourceOption {
  value: string
  label: string
}

/** `GET /credential-sources/{id}/options` — like the test route, a source
 * that cannot be asked answers `error` with no options. */
export interface CredentialSourceOptions {
  options: CredentialSourceOption[]
  error?: string
}

const JSON_HEADERS = { 'Content-Type': 'application/json' }

/** `GET /api/v1/credential-sources` */
export function listCredentialSources(): Promise<Page<CredentialSource>> {
  return apiFetch<Page<CredentialSource>>('/credential-sources')
}

/** `POST /api/v1/credential-sources` */
export function createCredentialSource(
  input: CreateCredentialSourceInput,
): Promise<CredentialSource> {
  return apiFetch<CredentialSource>('/credential-sources', {
    method: 'POST',
    headers: JSON_HEADERS,
    body: JSON.stringify(input),
  })
}

/** `PUT /api/v1/credential-sources/{id}` — name, config and enabled are each
 * optional; the provider cannot change. */
export function updateCredentialSource(
  id: string,
  input: UpdateCredentialSourceInput,
): Promise<CredentialSource> {
  return apiFetch<CredentialSource>(`/credential-sources/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: JSON_HEADERS,
    body: JSON.stringify(input),
  })
}

/** `DELETE /api/v1/credential-sources/{id}` — 409 while a connector names it. */
export function deleteCredentialSource(id: string): Promise<void> {
  return apiFetch<void>(`/credential-sources/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  })
}

/** `POST /api/v1/credential-sources/{id}/test` */
export function testCredentialSource(id: string): Promise<CredentialSourceTestResult> {
  return apiFetch<CredentialSourceTestResult>(
    `/credential-sources/${encodeURIComponent(id)}/test`,
    { method: 'POST' },
  )
}

/** `GET /api/v1/credential-sources/{id}/options?field=…` */
export function fetchCredentialSourceOptions(
  id: string,
  field: string,
): Promise<CredentialSourceOptions> {
  return apiFetch<CredentialSourceOptions>(
    `/credential-sources/${encodeURIComponent(id)}/options?field=${encodeURIComponent(field)}`,
  )
}
