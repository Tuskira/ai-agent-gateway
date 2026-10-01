import { ApiError, apiFetch } from '@/lib/api'
import type { UserRole } from '@/lib/types'

/** Data contract for the Users page (admin-only, `users.manage`). */

interface Page<T> {
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
  last_login_at?: string | null
  created_at?: string
}

export interface CreateUserInput {
  username: string
  display_name?: string
  role: UserRole
  /** Omit to have the server generate a temporary password. */
  password?: string
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

export function listUsers(): Promise<Page<User>> {
  return apiFetch<Page<User>>('/users')
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
