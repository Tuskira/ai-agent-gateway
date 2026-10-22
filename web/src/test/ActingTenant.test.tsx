import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor } from '@testing-library/react'
import { useQuery } from '@tanstack/react-query'
import { useAuth } from '@/auth/AuthContext'
import {
  ACTING_TENANT_HEADER,
  ACTING_TENANT_STORAGE_PREFIX,
  apiFetch,
  getActingTenant,
  setActingTenant,
  setCsrfToken,
} from '@/lib/api'
import {
  SESSION_ADMIN,
  SESSION_PLATFORM_ADMIN,
  TENANTS,
  jsonResponse,
  renderWithAuth,
  stubApi,
  type RecordedCall,
} from './authHelpers'

const STORAGE_KEY = `${ACTING_TENANT_STORAGE_PREFIX}${SESSION_PLATFORM_ADMIN.subject}`

/** Mounts only once the session is restored (like RequireAuth), then issues
 * one tenant-scoped query so the header on it can be asserted. */
function Probe() {
  const { principal, isLoading, actingTenantId, isPlatformAdmin, switchTenant, signOut } =
    useAuth()
  const users = useQuery({
    queryKey: ['users', 'list'],
    queryFn: () => apiFetch<{ items: unknown[] }>('/users'),
    enabled: !isLoading && principal !== null,
    retry: false,
  })
  if (isLoading) return <div>loading</div>
  if (!principal) return <div>signed out</div>
  return (
    <div>
      <div data-testid="acting">{actingTenantId ?? 'none'}</div>
      <div data-testid="platform">{String(isPlatformAdmin)}</div>
      <div data-testid="users">{users.isSuccess ? 'users loaded' : 'users pending'}</div>
      <button onClick={() => switchTenant('tenant-uuid-2')}>switch</button>
      <button onClick={() => signOut()}>sign out</button>
    </div>
  )
}

function stubPlatformApi(): RecordedCall[] {
  return stubApi({
    'GET /auth/me': () => jsonResponse(SESSION_PLATFORM_ADMIN),
    'GET /tenants?limit=200': () => jsonResponse(TENANTS),
    'GET /users': () => jsonResponse({ items: [] }),
    'POST /auth/logout': () => new Response(null, { status: 204 }),
  })
}

function header(call: RecordedCall | undefined) {
  return call?.headers[ACTING_TENANT_HEADER]
}

describe('AuthContext acting tenant', () => {
  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    setCsrfToken(null)
    setActingTenant(null)
  })
  afterEach(() => vi.unstubAllGlobals())

  it('a plain admin session never selects a tenant or lists tenants', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () => jsonResponse({ items: [] }),
    })
    renderWithAuth(<Probe />)

    await screen.findByText('users loaded')
    expect(screen.getByTestId('acting')).toHaveTextContent('none')
    expect(screen.getByTestId('platform')).toHaveTextContent('false')
    expect(getActingTenant()).toBeNull()
    expect(calls.some((c) => c.path.startsWith('/tenants'))).toBe(false)
    expect(header(calls.find((c) => c.path === '/users'))).toBeUndefined()
  })

  it('a platform session with nothing remembered acts in its home tenant', async () => {
    const calls = stubPlatformApi()
    renderWithAuth(<Probe />)

    await screen.findByText('users loaded')
    expect(screen.getByTestId('acting')).toHaveTextContent('tenant-uuid-1')
    expect(screen.getByTestId('platform')).toHaveTextContent('true')
    expect(getActingTenant()).toBe('tenant-uuid-1')
    // No remembered choice, so no tenant list was needed to validate one.
    expect(calls.some((c) => c.path.startsWith('/tenants'))).toBe(false)
    expect(header(calls.find((c) => c.path === '/users'))).toBe('tenant-uuid-1')
  })

  it('restores a remembered tenant after validating it against the tenant list', async () => {
    localStorage.setItem(STORAGE_KEY, 'tenant-uuid-3')
    const calls = stubPlatformApi()
    renderWithAuth(<Probe />)

    await screen.findByText('users loaded')
    expect(screen.getByTestId('acting')).toHaveTextContent('tenant-uuid-3')
    expect(getActingTenant()).toBe('tenant-uuid-3')
    // The validating list call went out before any header was set …
    const tenants = calls.find((c) => c.path === '/tenants?limit=200')!
    expect(header(tenants)).toBeUndefined()
    // … and the first page query already carried the restored selection, so
    // nothing of the home tenant was ever fetched or cached.
    const users = calls.filter((c) => c.path === '/users')
    expect(users).toHaveLength(1)
    expect(header(users[0])).toBe('tenant-uuid-3')
    expect(localStorage.getItem(STORAGE_KEY)).toBe('tenant-uuid-3')
  })

  it('falls back to the home tenant and forgets a remembered tenant that no longer exists', async () => {
    localStorage.setItem(STORAGE_KEY, 'tenant-uuid-gone')
    const calls = stubPlatformApi()
    renderWithAuth(<Probe />)

    await screen.findByText('users loaded')
    expect(screen.getByTestId('acting')).toHaveTextContent('tenant-uuid-1')
    expect(header(calls.find((c) => c.path === '/users'))).toBe('tenant-uuid-1')
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })

  it('falls back to the home tenant when the tenant list cannot be fetched', async () => {
    localStorage.setItem(STORAGE_KEY, 'tenant-uuid-3')
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_PLATFORM_ADMIN),
      'GET /tenants?limit=200': () => new Response('oops', { status: 500 }),
      'GET /users': () => jsonResponse({ items: [] }),
    })
    renderWithAuth(<Probe />)

    await screen.findByText('users loaded')
    expect(screen.getByTestId('acting')).toHaveTextContent('tenant-uuid-1')
  })

  it('does not apply a tenant remembered for another subject', async () => {
    localStorage.setItem(`${ACTING_TENANT_STORAGE_PREFIX}someone-else`, 'tenant-uuid-3')
    const calls = stubPlatformApi()
    renderWithAuth(<Probe />)

    await screen.findByText('users loaded')
    expect(screen.getByTestId('acting')).toHaveTextContent('tenant-uuid-1')
    expect(header(calls.find((c) => c.path === '/users'))).toBe('tenant-uuid-1')
  })

  it('switchTenant remembers the choice, drops cached tenant data, keeps the session and re-queries with the new header', async () => {
    const calls = stubPlatformApi()
    const { container } = renderWithAuth(<Probe />)

    await screen.findByText('users loaded')
    const before = calls.filter((c) => c.path === '/users').length

    act(() => {
      screen.getByRole('button', { name: 'switch' }).click()
    })

    expect(getActingTenant()).toBe('tenant-uuid-2')
    expect(localStorage.getItem(STORAGE_KEY)).toBe('tenant-uuid-2')
    // The shell stayed mounted: no "loading" flash on switch.
    expect(container).not.toHaveTextContent('loading')
    await waitFor(() =>
      expect(screen.getByTestId('acting')).toHaveTextContent('tenant-uuid-2'),
    )
    expect(container).not.toHaveTextContent('loading')
    await waitFor(() =>
      expect(calls.filter((c) => c.path === '/users').length).toBe(before + 1),
    )
    expect(header(calls.filter((c) => c.path === '/users').at(-1))).toBe('tenant-uuid-2')
    // /auth/me was kept, not refetched.
    expect(calls.filter((c) => c.path === '/auth/me')).toHaveLength(1)
  })

  it('signOut clears the selection in memory and in storage', async () => {
    localStorage.setItem(STORAGE_KEY, 'tenant-uuid-3')
    stubPlatformApi()
    renderWithAuth(<Probe />)

    await screen.findByText('users loaded')
    expect(getActingTenant()).toBe('tenant-uuid-3')

    act(() => {
      screen.getByRole('button', { name: 'sign out' }).click()
    })

    expect(await screen.findByText('signed out')).toBeInTheDocument()
    expect(getActingTenant()).toBeNull()
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })
})
