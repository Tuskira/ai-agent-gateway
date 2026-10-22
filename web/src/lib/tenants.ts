import { apiFetch } from '@/lib/api'

/** Data contract for the platform-level tenant routes (`platform.admin`).
 * These handlers are not tenant-scoped, so the acting-tenant header is
 * harmless on them. */

export interface Tenant {
  id: string
  slug: string
  name: string
  created_at: string
}

export interface TenantsPage {
  items: Tenant[]
  total: number
}

export const TENANT_NAME_MAX = 120

export interface CreateTenantInput {
  slug: string
  name: string
}

/** Shared cache key for the tenant list, used by `useTenants` and by the
 * acting-tenant restore in AuthContext. */
export const TENANTS_QUERY_KEY = ['tenants'] as const

/** `GET /tenants` pages in memory; the switcher asks for one page of this
 * size and shows `total` so an operator notices when there are more. */
export const TENANTS_PAGE_LIMIT = 200

export function listTenants(): Promise<TenantsPage> {
  return apiFetch<TenantsPage>(`/tenants?limit=${TENANTS_PAGE_LIMIT}`)
}

export function createTenant(input: CreateTenantInput): Promise<Tenant> {
  return apiFetch<Tenant>('/tenants', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

/** `PATCH /tenants/{id}`: rename a tenant. The slug and id never change. */
export function renameTenant(id: string, name: string): Promise<Tenant> {
  return apiFetch<Tenant>(`/tenants/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  })
}
