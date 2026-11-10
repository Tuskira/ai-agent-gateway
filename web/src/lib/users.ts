import { ApiError, apiFetch } from '@/lib/api'
import type { UserRole } from '@/lib/types'

/** Data contract for the Users page (admin-only, `users.manage`). */

export interface Page<T> {
  items: T[]
  total?: number
}

export interface User {
  id: string
  username: string
  display_name: string
  role: UserRole
  disabled: boolean
  must_change_password?: boolean
  /** No usable password: signs in through single sign-on only. */
  sso_only?: boolean
  last_login_at?: string | null
  created_at?: string
  /** A platform-admin row the caller cannot change from this tenant (it
   * lives in another tenant, or the caller lacks platform.admin). */
  read_only?: boolean
  /** A platform-admin row listed here from another tenant. */
  other_tenant?: boolean
  /** Which tenant the row belongs to; sent to platform admins only. */
  home_tenant_id?: string
}

/** A platform-admin row from another tenant as a caller without
 * platform.admin sees it: a name and the role only. There is no id,
 * username or status, and the row never has actions. */
export interface ForeignAdminRow {
  id?: undefined
  username?: undefined
  display_name: string
  role: UserRole
  disabled?: undefined
  last_login_at?: undefined
  read_only: true
  other_tenant: true
  home_tenant_id?: undefined
}

/** One row of the Users list. */
export type UserRow = User | ForeignAdminRow

/** A row that carries an id and username (everything but a foreign admin
 * seen without platform.admin). */
export function isFullUser(u: UserRow): u is User {
  return typeof u.id === 'string'
}

export interface CreateUserInput {
  username: string
  display_name?: string
  role: UserRole
  /** Omit to have the server generate a temporary password. */
  password?: string
  /** No password at all: the user signs in through single sign-on only.
   * Excludes `password`. */
  sso_only?: boolean
}

export interface CreateUserResponse {
  user: User
  temporary_password?: string
}

export interface UpdateUserInput {
  display_name?: string
  role?: UserRole
  disabled?: boolean
}

export interface AuditEntry {
  id: number | string
  at: string
  actor_kind: string
  actor_id: string
  action: string
  target_user_id?: string | null
  ip?: string
  detail?: string
}

const JSON_HEADERS = { 'Content-Type': 'application/json' }

function userPath(id: string): string {
  return `/users/${encodeURIComponent(id)}`
}

export function listUsers(): Promise<Page<UserRow>> {
  return apiFetch<Page<UserRow>>('/users')
}

export function getUser(id: string): Promise<User> {
  return apiFetch<User>(userPath(id))
}

export function createUser(input: CreateUserInput): Promise<CreateUserResponse> {
  return apiFetch<CreateUserResponse>('/users', {
    method: 'POST',
    headers: JSON_HEADERS,
    body: JSON.stringify(input),
  })
}

export function updateUser(id: string, input: UpdateUserInput): Promise<User> {
  return apiFetch<User>(userPath(id), {
    method: 'PATCH',
    headers: JSON_HEADERS,
    body: JSON.stringify(input),
  })
}

export function deleteUser(id: string): Promise<void> {
  return apiFetch<void>(userPath(id), { method: 'DELETE' })
}

export function resetUserPassword(id: string): Promise<{ temporary_password: string }> {
  return apiFetch<{ temporary_password: string }>(`${userPath(id)}/reset-password`, {
    method: 'POST',
  })
}

export function revokeUserSessions(id: string): Promise<void> {
  return apiFetch<void>(`${userPath(id)}/revoke-sessions`, { method: 'POST' })
}

export function listAudit(): Promise<Page<AuditEntry>> {
  return apiFetch<Page<AuditEntry>>('/auth/audit')
}

/** Turns a failed user-management call into inline text. The 409 guards
 * (last admin, acting on yourself) are explained rather than shown raw. */
export function describeUserError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    if (err.status === 409 && err.type === 'last_admin') {
      return 'This is the last active admin. Make another user an admin first.'
    }
    if (err.status === 409 && err.type.includes('self')) {
      return 'You can’t do this to your own account. Ask another admin.'
    }
    if (err.status === 403) {
      return 'You don’t have permission to manage users.'
    }
    return err.message
  }
  return fallback
}
