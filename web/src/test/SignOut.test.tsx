import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { AuthProvider } from '@/auth/AuthContext'
import { RequireAuth } from '@/auth/RequireAuth'
import { AppShell } from '@/components/layout/AppShell'
import { API_KEY_STORAGE_KEY, getCsrfToken, setCsrfToken } from '@/lib/api'
import LoginPage from '@/routes/LoginPage'
import ChangePasswordPage from '@/routes/ChangePasswordPage'
import {
  AUTH_CONFIG,
  CSRF,
  SESSION_ADMIN,
  errorResponse,
  jsonResponse,
  stubApi,
} from './authHelpers'

const API_KEY_PRINCIPAL = {
  subject: 'avinash@tuskira.ai',
  tenant_id: 'tuskira',
  email: 'avinash@tuskira.ai',
  roles: ['admin'],
  auth_method: 'api_key',
  key_id: 'key_123',
  kind: 'api_key',
}

const HEALTH = { status: 'ok', plane: 'api', version: '0.1.0' }

function renderAuthenticatedShell() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })

  // Mirrors the shape of src/routes/router.tsx (login route + a
  // RequireAuth-guarded shell), but built with createMemoryRouter so
  // navigation can be asserted without touching real browser history.
  const router = createMemoryRouter(
    [
      { path: '/login', element: <LoginPage /> },
      {
        element: <RequireAuth />,
        children: [
          { path: '/change-password', element: <ChangePasswordPage /> },
          {
            element: <AppShell />,
            children: [{ index: true, element: <div>Overview stub</div> }],
          },
        ],
      },
    ],
    { initialEntries: ['/'] },
  )

  render(
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        <RouterProvider router={router} />
      </AuthProvider>
    </QueryClientProvider>,
  )

  return router
}

describe('Log out', () => {
  beforeEach(() => {
    sessionStorage.clear()
    setCsrfToken(null)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('api-key mode: navigates to /login and clears the stored key', async () => {
    sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(API_KEY_PRINCIPAL),
      'GET /auth/config': () => jsonResponse(AUTH_CONFIG),
      'GET /health': () => jsonResponse(HEALTH),
    })
    const user = userEvent.setup()
    const router = renderAuthenticatedShell()

    await screen.findByText('Overview stub')
    await user.click(screen.getByRole('button', { name: /account menu/i }))
    // The menu names the credential kind and offers no password change.
    expect(await screen.findByText('API key')).toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: /change password/i })).toBeNull()
    await user.click(await screen.findByRole('menuitem', { name: /log out/i }))

    await waitFor(() => expect(router.state.location.pathname).toBe('/login'))
    expect(sessionStorage.getItem(API_KEY_STORAGE_KEY)).toBeNull()
    // API-key sessions have no server session to end.
    expect(calls.some((c) => c.path === '/auth/logout')).toBe(false)
  })

  it('session mode: shows the user, calls /auth/logout with the CSRF header, then clears it', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /auth/config': () => jsonResponse(AUTH_CONFIG),
      'GET /health': () => jsonResponse(HEALTH),
      'POST /auth/logout': () => new Response(null, { status: 204 }),
    })
    const user = userEvent.setup()
    const router = renderAuthenticatedShell()

    await screen.findByText('Overview stub')
    await user.click(screen.getByRole('button', { name: /account menu/i }))
    expect(await screen.findByText('jane')).toBeInTheDocument()
    expect(screen.getByText(/admin · acme/)).toBeInTheDocument()
    await user.click(await screen.findByRole('menuitem', { name: /log out/i }))

    await waitFor(() => expect(router.state.location.pathname).toBe('/login'))
    const logout = calls.find((c) => c.path === '/auth/logout')!
    expect(logout.headers['X-CSRF-Token']).toBe(CSRF)
    expect(getCsrfToken()).toBeNull()
  })

  it('session mode: Change password in the menu opens the page', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /auth/config': () => jsonResponse(AUTH_CONFIG),
      'GET /health': () => jsonResponse(HEALTH),
    })
    const user = userEvent.setup()
    const router = renderAuthenticatedShell()

    await screen.findByText('Overview stub')
    await user.click(screen.getByRole('button', { name: /account menu/i }))
    await user.click(await screen.findByRole('menuitem', { name: /change password/i }))

    await waitFor(() => expect(router.state.location.pathname).toBe('/change-password'))
  })

  it('a 401 on the session probe sends an unauthenticated visitor to /login', async () => {
    stubApi({
      'GET /auth/me': () => errorResponse(401, 'authentication_error', 'no session'),
      'GET /auth/config': () => jsonResponse(AUTH_CONFIG),
      'GET /health': () => jsonResponse(HEALTH),
    })
    const router = renderAuthenticatedShell()

    await waitFor(() => expect(router.state.location.pathname).toBe('/login'))
  })
})
