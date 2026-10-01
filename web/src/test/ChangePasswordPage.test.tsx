import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { AuthProvider } from '@/auth/AuthContext'
import { RequireAuth } from '@/auth/RequireAuth'
import { setCsrfToken } from '@/lib/api'
import ChangePasswordPage from '@/routes/ChangePasswordPage'
import { CSRF, SESSION_ADMIN, errorResponse, jsonResponse, stubApi } from './authHelpers'

const FORCED = { ...SESSION_ADMIN, must_change_password: true }

function renderApp(initial: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createMemoryRouter(
    [
      {
        element: <RequireAuth />,
        children: [
          { path: '/change-password', element: <ChangePasswordPage /> },
          { path: '/', element: <div>Console stub</div> },
          { path: '/connectors', element: <div>Connectors stub</div> },
        ],
      },
      { path: '/login', element: <div>Login stub</div> },
    ],
    { initialEntries: [initial] },
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

async function fill(
  user: ReturnType<typeof userEvent.setup>,
  current: string,
  next: string,
  confirm: string,
) {
  await user.type(await screen.findByLabelText('Current password'), current)
  await user.type(screen.getByLabelText('New password'), next)
  await user.type(screen.getByLabelText('Confirm new password'), confirm)
  await user.click(screen.getByRole('button', { name: /^change password$/i }))
}

describe('ChangePasswordPage', () => {
  beforeEach(() => setCsrfToken(null))
  afterEach(() => vi.unstubAllGlobals())

  it('forces a must-change session onto the page and keeps it there', async () => {
    stubApi({ 'GET /auth/me': () => jsonResponse(FORCED) })
    const router = renderApp('/connectors')

    await screen.findByLabelText('Current password')
    expect(router.state.location.pathname).toBe('/change-password')
    expect(screen.getByText(/temporary password/i)).toBeInTheDocument()
    // The only way out is logging out.
    expect(screen.getByRole('button', { name: /log out/i })).toBeInTheDocument()
    expect(screen.queryByText(/back to the console/i)).not.toBeInTheDocument()

    await router.navigate('/connectors')
    await waitFor(() => expect(router.state.location.pathname).toBe('/change-password'))
  })

  it('redirects to the page when the API answers 403 password_change_required', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () =>
        errorResponse(403, 'password_change_required', 'Change your password first.'),
    })
    const router = renderApp('/')
    await screen.findByText('Console stub')

    const { listUsers } = await import('@/lib/users')
    await expect(listUsers()).rejects.toThrow('Change your password first.')

    await waitFor(() => expect(router.state.location.pathname).toBe('/change-password'))
  })

  it('validates length, username and confirmation on the client', async () => {
    const calls = stubApi({ 'GET /auth/me': () => jsonResponse(FORCED) })
    const user = userEvent.setup()
    renderApp('/change-password')

    await fill(user, 'temp-pass', 'short', 'different')
    expect(await screen.findByText('Use at least 12 characters')).toBeInTheDocument()
    expect(screen.getByText('Passwords don’t match')).toBeInTheDocument()

    await user.clear(screen.getByLabelText('New password'))
    await user.clear(screen.getByLabelText('Confirm new password'))
    await user.type(screen.getByLabelText('New password'), 'JANE')
    await user.click(screen.getByRole('button', { name: /^change password$/i }))
    expect(await screen.findByText('Use at least 12 characters')).toBeInTheDocument()

    // Over the 128-character ceiling.
    await user.clear(screen.getByLabelText('New password'))
    await user.type(screen.getByLabelText('New password'), 'x'.repeat(129))
    await user.click(screen.getByRole('button', { name: /^change password$/i }))
    expect(await screen.findByText('Use at most 128 characters')).toBeInTheDocument()

    expect(calls.some((c) => c.path === '/auth/password')).toBe(false)
  })

  it('rejects a password equal to the username', async () => {
    // Username "jane.the.admin" is 14 chars, so only the equality rule fires.
    stubApi({
      'GET /auth/me': () =>
        jsonResponse({
          ...FORCED,
          user: { ...SESSION_ADMIN.user, username: 'jane.the.admin' },
        }),
    })
    const user = userEvent.setup()
    renderApp('/change-password')

    await fill(user, 'temp-pass', 'Jane.The.Admin', 'Jane.The.Admin')
    expect(
      await screen.findByText('Password can’t be the same as your username'),
    ).toBeInTheDocument()
  })

  it('posts the exact body with the CSRF header, then returns to the console', async () => {
    let changed = false
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(changed ? SESSION_ADMIN : FORCED),
      'POST /auth/password': () => {
        changed = true
        return new Response(null, { status: 204 })
      },
    })
    const user = userEvent.setup()
    const router = renderApp('/change-password')

    await fill(user, 'temp-pass', 'a brand new passphrase', 'a brand new passphrase')

    await waitFor(() => expect(router.state.location.pathname).toBe('/'))
    const post = calls.find((c) => c.path === '/auth/password')!
    expect(JSON.parse(post.body!)).toEqual({
      current_password: 'temp-pass',
      new_password: 'a brand new passphrase',
    })
    expect(post.headers['X-CSRF-Token']).toBe(CSRF)
    expect(await screen.findByText('Console stub')).toBeInTheDocument()
  })

  it('shows the server error inline', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(FORCED),
      'POST /auth/password': () =>
        errorResponse(400, 'invalid_request', 'current password is incorrect'),
    })
    const user = userEvent.setup()
    renderApp('/change-password')

    await fill(user, 'wrong', 'a brand new passphrase', 'a brand new passphrase')

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'current password is incorrect',
    )
  })
})
