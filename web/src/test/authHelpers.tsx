import { vi } from 'vitest'
import type { ReactNode } from 'react'
import { render } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { AuthProvider } from '@/auth/AuthContext'

export function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

export function errorResponse(status: number, type: string, message: string) {
  return jsonResponse({ error: { type, message } }, status)
}

export const CSRF = 'csrf-token-123'

/** A cookie-session admin, as `GET /auth/me` returns it. */
export const SESSION_ADMIN = {
  subject: 'jane',
  tenant_id: 'tenant-uuid-1',
  email: '',
  roles: ['admin'],
  auth_method: 'session',
  key_id: '',
  kind: 'user',
  user: {
    id: 'u-jane',
    username: 'jane',
    display_name: 'Jane',
    role: 'admin',
    tenant: 'acme',
  },
  must_change_password: false,
  csrf_token: CSRF,
}

export const SESSION_VIEWER = {
  ...SESSION_ADMIN,
  subject: 'vic',
  roles: ['viewer'],
  user: { ...SESSION_ADMIN.user, id: 'u-vic', username: 'vic', role: 'viewer' },
}

export const AUTH_CONFIG = {
  password_login: true,
  api_key_login: true,
  single_tenant: false,
  default_tenant: 'acme',
}

export interface RecordedCall {
  method: string
  path: string
  headers: Record<string, string>
  body: string | null
  credentials?: RequestCredentials
}

type Handler = (call: RecordedCall) => Response | Promise<Response>

/** Stubs `fetch` with `"METHOD /path"` handlers (path is relative to /api/v1).
 * Unhandled requests reject, like a dead network. Returns the recorded calls. */
export function stubApi(handlers: Record<string, Handler>): RecordedCall[] {
  const calls: RecordedCall[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const path = url.replace(/^.*\/api\/v1/, '')
      const method = (init?.method ?? 'GET').toUpperCase()
      const call: RecordedCall = {
        method,
        path,
        headers: (init?.headers ?? {}) as Record<string, string>,
        body: typeof init?.body === 'string' ? init.body : null,
        credentials: init?.credentials,
      }
      calls.push(call)
      const handler = handlers[`${method} ${path}`]
      if (!handler) throw new Error(`Unhandled fetch in test: ${method} ${path}`)
      return handler(call)
    }),
  )
  return calls
}

export function renderWithAuth(ui: ReactNode) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <AuthProvider>{ui}</AuthProvider>
    </QueryClientProvider>,
  )
}
