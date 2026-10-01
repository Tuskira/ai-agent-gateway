import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  API_KEY_STORAGE_KEY,
  ApiError,
  apiFetch,
  onPasswordChangeRequired,
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
})
