import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  ACTING_TENANT_HEADER,
  API_KEY_STORAGE_KEY,
  ApiError,
  apiFetch,
  getActingTenant,
  onPasswordChangeRequired,
  setActingTenant,
  setCsrfToken,
} from '@/lib/api'
import { errorResponse, stubApi } from './authHelpers'

function ok() {
  return new Response('{}', {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

describe('apiFetch session auth', () => {
  beforeEach(() => {
    sessionStorage.clear()
    setCsrfToken(null)
    setActingTenant(null)
  })
  afterEach(() => vi.unstubAllGlobals())

  it('sends the cookie same-origin and the CSRF header on every mutating method', async () => {
    setCsrfToken('tok')
    const calls = stubApi({
      'POST /x': ok,
      'PUT /x': ok,
      'PATCH /x': ok,
      'DELETE /x': ok,
      'GET /x': ok,
    })

    for (const method of ['POST', 'PUT', 'PATCH', 'DELETE', 'GET']) {
      await apiFetch('/x', { method })
    }

    expect(calls.map((c) => c.credentials)).toEqual(Array(5).fill('same-origin'))
    for (const call of calls.filter((c) => c.method !== 'GET')) {
      expect(call.headers['X-CSRF-Token']).toBe('tok')
      expect(call.headers.Authorization).toBeUndefined()
    }
    // Safe methods don't carry it.
    expect(calls.find((c) => c.method === 'GET')!.headers['X-CSRF-Token']).toBeUndefined()
  })

  it('api-key mode sends the bearer token and no CSRF header', async () => {
    sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_key')
    setCsrfToken('tok')
    const calls = stubApi({ 'POST /x': ok })

    await apiFetch('/x', { method: 'POST' })

    expect(calls[0]!.headers.Authorization).toBe('Bearer gk_key')
    expect(calls[0]!.headers['X-CSRF-Token']).toBeUndefined()
  })

  it('sends no CSRF header before there is a session', async () => {
    const calls = stubApi({ 'POST /x': ok })

    await apiFetch('/x', { method: 'POST' })

    expect(calls[0]!.headers['X-CSRF-Token']).toBeUndefined()
    expect(calls[0]!.headers.Authorization).toBeUndefined()
  })

  it('notifies the auth layer on 403 password_change_required and still throws', async () => {
    const handler = vi.fn()
    const off = onPasswordChangeRequired(handler)
    stubApi({
      'GET /x': () => errorResponse(403, 'password_change_required', 'change it'),
    })

    await expect(apiFetch('/x')).rejects.toBeInstanceOf(ApiError)
    expect(handler).toHaveBeenCalledTimes(1)
    off()
  })

  it('does not notify on other 403s', async () => {
    const handler = vi.fn()
    const off = onPasswordChangeRequired(handler)
    stubApi({ 'GET /x': () => errorResponse(403, 'permission_denied', 'no') })

    await expect(apiFetch('/x')).rejects.toBeInstanceOf(ApiError)
    expect(handler).not.toHaveBeenCalled()
    off()
  })

  it('a 401 clears the stored key and the CSRF token and goes to /login', async () => {
    sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_key')
    setCsrfToken('tok')
    const assign = vi.fn()
    vi.stubGlobal('location', { ...window.location, pathname: '/users', assign })
    stubApi({ 'GET /x': () => errorResponse(401, 'authentication_error', 'expired') })

    await expect(apiFetch('/x')).rejects.toBeInstanceOf(ApiError)

    expect(assign).toHaveBeenCalledWith('/login')
    expect(sessionStorage.getItem(API_KEY_STORAGE_KEY)).toBeNull()
    const calls = stubApi({ 'POST /y': ok })
    await apiFetch('/y', { method: 'POST' })
    expect(calls[0]!.headers['X-CSRF-Token']).toBeUndefined()
  })

  it('sends no acting-tenant header while no tenant is selected', async () => {
    const calls = stubApi({ 'GET /users': ok, 'POST /users': ok })

    await apiFetch('/users')
    await apiFetch('/users', { method: 'POST' })

    expect(getActingTenant()).toBeNull()
    for (const call of calls) expect(call.headers[ACTING_TENANT_HEADER]).toBeUndefined()
  })

  it('sends the acting-tenant header on every method once a tenant is selected', async () => {
    setActingTenant('tenant-uuid-2')
    setCsrfToken('tok')
    const calls = stubApi({
      'GET /users': ok,
      'POST /users': ok,
      'PATCH /users': ok,
      'DELETE /users': ok,
      'GET /tenants?limit=200': ok,
    })

    for (const method of ['GET', 'POST', 'PATCH', 'DELETE']) {
      await apiFetch('/users', { method })
    }
    // Platform routes are not tenant-scoped; sending it there is harmless.
    await apiFetch('/tenants?limit=200')

    expect(getActingTenant()).toBe('tenant-uuid-2')
    expect(calls).toHaveLength(5)
    for (const call of calls)
      expect(call.headers[ACTING_TENANT_HEADER]).toBe('tenant-uuid-2')
    // It rides alongside the CSRF header, not instead of it.
    expect(calls.find((c) => c.method === 'POST')!.headers['X-CSRF-Token']).toBe('tok')
  })

  it('never sends the acting-tenant header to the self-service /auth/* routes', async () => {
    setActingTenant('tenant-uuid-2')
    const calls = stubApi({
      'GET /auth/me': ok,
      'POST /auth/login': ok,
      'POST /auth/logout': ok,
      'POST /auth/password': ok,
      'GET /auth/config': ok,
      'GET /authors': ok,
    })

    await apiFetch('/auth/me')
    await apiFetch('/auth/login', { method: 'POST' })
    await apiFetch('/auth/logout', { method: 'POST' })
    await apiFetch('/auth/password', { method: 'POST' })
    await apiFetch('/auth/config')
    // Only the `/auth/` prefix is exempt, not every path starting with "auth".
    await apiFetch('/authors')

    for (const call of calls.filter((c) => c.path.startsWith('/auth/'))) {
      expect(call.headers[ACTING_TENANT_HEADER]).toBeUndefined()
    }
    expect(calls.find((c) => c.path === '/authors')!.headers[ACTING_TENANT_HEADER]).toBe(
      'tenant-uuid-2',
    )
  })

  it('clearing the selection stops the header on the next call', async () => {
    setActingTenant('tenant-uuid-2')
    const calls = stubApi({ 'GET /users': ok })

    await apiFetch('/users')
    setActingTenant(null)
    await apiFetch('/users')

    expect(calls[0]!.headers[ACTING_TENANT_HEADER]).toBe('tenant-uuid-2')
    expect(calls[1]!.headers[ACTING_TENANT_HEADER]).toBeUndefined()
  })

  it('a 401 drops the acting-tenant selection along with the session', async () => {
    setActingTenant('tenant-uuid-2')
    vi.stubGlobal('location', { ...window.location, pathname: '/login', assign: vi.fn() })
    stubApi({ 'GET /x': () => errorResponse(401, 'authentication_error', 'expired') })

    await expect(apiFetch('/x')).rejects.toBeInstanceOf(ApiError)

    expect(getActingTenant()).toBeNull()
  })
})
