import { apiFetch } from '@/lib/api'
import type { Principal, SessionUser } from '@/lib/types'

/** Session-auth endpoints. Kept in one file so contract tweaks are a
 * one-place change. See docs/authentication.md. */

/** `GET /api/v1/auth/config` (public) */
export interface AuthConfig {
  password_login: boolean
  api_key_login: boolean
  single_tenant: boolean
  /** False only on a single-tenant install with no active console user; absent on older gateways. */
  has_users?: boolean
  default_tenant: string
}

/** `POST /api/v1/auth/login` response. */
export interface LoginResponse {
  user: SessionUser
  must_change_password: boolean
  csrf_token: string
}

export interface LoginInput {
  tenant?: string
  username: string
  password: string
}

export interface ChangePasswordInput {
  current_password: string
  new_password: string
}

export function getAuthConfig(): Promise<AuthConfig> {
  return apiFetch<AuthConfig>('/auth/config', { skipAuthRedirect: true })
}

/** A wrong password must surface inline, so this never triggers the global
 * 401 redirect. */
export function login(input: LoginInput): Promise<LoginResponse> {
  const body: LoginInput = {
    ...(input.tenant ? { tenant: input.tenant } : {}),
    username: input.username,
    password: input.password,
  }
  return apiFetch<LoginResponse>('/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
    skipAuthRedirect: true,
  })
}

export function logout(): Promise<void> {
  return apiFetch<void>('/auth/logout', { method: 'POST', skipAuthRedirect: true })
}

/** `GET /api/v1/auth/me`. The session restore probe: a 401 here just means
 * "not signed in". */
export function me(): Promise<Principal> {
  return apiFetch<Principal>('/auth/me', { skipAuthRedirect: true })
}

/** A wrong current password may come back as 401; show it inline instead
 * of bouncing to /login. */
export function changePassword(input: ChangePasswordInput): Promise<void> {
  return apiFetch<void>('/auth/password', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
    skipAuthRedirect: true,
  })
}
