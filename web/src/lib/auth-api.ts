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
  /** True when the login request may omit `tenant`: the gateway looks the
   * username up across tenants and answers 409 `tenant_required` only when
   * it exists in more than one. Absent on older gateways (tenant required). */
  tenant_optional?: boolean
  /** Single sign-on (OIDC) providers, one "Sign in with ..." button each.
   * Absent on older gateways. */
  sso_providers?: SsoProvider[]
}

/** One `sso_providers` entry. `start_url` is relative to the console origin;
 * the console appends `?return_to=<path>`. */
export interface SsoProvider {
  name: string
  display_name: string
  start_url: string
}

/** The failure codes a single sign-on callback redirects back with
 * (`/login?sso_error=<code>`), and what the login page says for each. */
export const SSO_ERROR_MESSAGES: Record<string, string> = {
  state: 'The sign-in took too long or was interrupted. Try again.',
  token: 'The identity provider’s answer could not be verified. Try again.',
  domain: 'Your email domain is not allowed to sign in here.',
  unverified: 'Your email address is not verified with the identity provider.',
  idp: 'Sign in through your organization’s identity provider.',
  no_user: 'No gateway account matches your email. Ask a gateway admin to create one.',
  disabled: 'Your gateway account is disabled. Ask a gateway admin.',
  linked: 'Your gateway account is linked to a different identity. Ask a gateway admin.',
  link_required:
    'Your gateway account has a password, so it cannot be linked to this sign-in automatically. Ask a gateway admin.',
  role: 'This account cannot use single sign-on. Sign in with your password.',
  provider:
    'Single sign-on is unavailable right now. Try again, or use another sign-in method.',
}

/** The message for an `sso_error` code; unknown codes get a generic one, so
 * nothing from the URL is ever shown verbatim. */
export function ssoErrorMessage(code: string): string {
  const known = Object.hasOwn(SSO_ERROR_MESSAGES, code)
    ? SSO_ERROR_MESSAGES[code]
    : undefined
  return known ?? 'Single sign-on failed. Try again.'
}

/** The link that starts a login with provider p and comes back to returnTo
 * (a console path). */
export function ssoStartHref(p: SsoProvider, returnTo: string): string {
  const path = returnTo.startsWith('/') && !returnTo.startsWith('//') ? returnTo : '/'
  return `${p.start_url}?return_to=${encodeURIComponent(path)}`
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
