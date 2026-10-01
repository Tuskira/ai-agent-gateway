import type { ApiErrorBody } from '@/lib/types'

/**
 * All API calls are relative to `/api/v1`. In dev, Vite proxies `/api` to
 * the gateway on :8081 (see vite.config.ts). In production the built app is
 * served by the gateway itself at `/`, so the same relative path is
 * same-origin — no environment-specific base URL is needed.
 */
export const API_BASE = '/api/v1'

export const API_KEY_STORAGE_KEY = 'gateway.api_key'

/** Thrown for any non-2xx API response. */
export class ApiError extends Error {
  readonly status: number
  readonly type: string
  /** The full parsed JSON error body, when there was one -- beyond the
   * common `error.{type,message}` shape a handler may include extra
   * fields (e.g. a usage count alongside a 409). `undefined` when the body
   * wasn't JSON. */
  readonly details?: unknown

  constructor(status: number, type: string, message: string, details?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.type = type
    this.details = details
  }
}

export function getStoredApiKey(): string | null {
  try {
    return sessionStorage.getItem(API_KEY_STORAGE_KEY)
  } catch {
    // sessionStorage can throw in locked-down environments (e.g. private
    // browsing with storage disabled). Treat as "no key".
    return null
  }
}

export function setStoredApiKey(key: string): void {
  try {
    sessionStorage.setItem(API_KEY_STORAGE_KEY, key)
  } catch {
    // best effort — if storage is unavailable the session simply won't
    // persist across reloads.
  }
}

export function clearStoredApiKey(): void {
  try {
    sessionStorage.removeItem(API_KEY_STORAGE_KEY)
  } catch {
    // ignore
  }
}

/* ------------------------------------------------------------------------ */
/* Session (cookie) auth state                                              */
/* ------------------------------------------------------------------------ */

const MUTATING_METHODS = new Set(['POST', 'PUT', 'PATCH', 'DELETE'])

/** Double-submit CSRF token of the current console session. Held in memory
 * only (never in storage); AuthContext sets it from the login response and
 * `/auth/me`. */
let csrfToken: string | null = null

export function setCsrfToken(token: string | null): void {
  csrfToken = token
}

export function getCsrfToken(): string | null {
  return csrfToken
}

let passwordChangeRequiredHandler: (() => void) | null = null

/** Registers the callback run when the API answers 403
 * `password_change_required`. Returns an unsubscribe function. */
export function onPasswordChangeRequired(handler: () => void): () => void {
  passwordChangeRequiredHandler = handler
  return () => {
    if (passwordChangeRequiredHandler === handler) passwordChangeRequiredHandler = null
  }
}

interface ApiFetchOptions extends Omit<RequestInit, 'headers'> {
  headers?: Record<string, string>
  /** Use this key instead of the one in sessionStorage (sign-in flow). */
  apiKey?: string
  /** Skip the clear-key + redirect-to-login behavior on a 401 (sign-in flow,
   * where a bad key should surface as an inline form error instead). */
  skipAuthRedirect?: boolean
}

/**
 * Fetch wrapper for the gateway API. In session mode the `gw_session` cookie
 * travels automatically (`same-origin`) and mutating requests carry the
 * `X-CSRF-Token` header. The bearer token is sent only in the emergency
 * API-key mode, which is exempt from CSRF. A 401 signs the user out to
 * `/login`; a 403 `password_change_required` notifies the auth layer so it
 * can route to the change-password page.
 */
export async function apiFetch<T>(
  path: string,
  options: ApiFetchOptions = {},
): Promise<T> {
  const { apiKey, skipAuthRedirect, headers, ...rest } = options
  const key = apiKey ?? getStoredApiKey()
  const method = (rest.method ?? 'GET').toUpperCase()
  const sendCsrf = !key && csrfToken !== null && MUTATING_METHODS.has(method)

  const response = await fetch(`${API_BASE}${path}`, {
    credentials: 'same-origin',
    ...rest,
    headers: {
      Accept: 'application/json',
      ...(key ? { Authorization: `Bearer ${key}` } : {}),
      ...(sendCsrf ? { 'X-CSRF-Token': csrfToken as string } : {}),
      ...headers,
    },
  })

  if (response.status === 401 && !skipAuthRedirect) {
    clearStoredApiKey()
    csrfToken = null
    if (typeof window !== 'undefined' && window.location.pathname !== '/login') {
      window.location.assign('/login')
    }
  }

  if (!response.ok) {
    let type = 'error'
    let message = `Request failed with status ${response.status}`
    let details: unknown
    try {
      const body = (await response.json()) as Partial<ApiErrorBody>
      if (body.error?.message) message = body.error.message
      if (body.error?.type) type = body.error.type
      details = body
    } catch {
      // non-JSON error body — fall back to the generic message above.
    }
    if (response.status === 403 && type === 'password_change_required') {
      passwordChangeRequiredHandler?.()
    }
    throw new ApiError(response.status, type, message, details)
  }

  if (response.status === 204) {
    return undefined as T
  }

  return (await response.json()) as T
}
