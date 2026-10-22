import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { AuthProvider } from '@/auth/AuthContext'
import { API_KEY_STORAGE_KEY, getCsrfToken, setCsrfToken } from '@/lib/api'
import LoginPage from '@/routes/LoginPage'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  AUTH_CONFIG,
  CSRF,
  SESSION_ADMIN,
  errorResponse,
  jsonResponse,
  stubApi,
} from './authHelpers'

const HEALTH = { status: 'ok', plane: 'api', version: '0.1.0' }

function renderLogin() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createMemoryRouter(
    [
      { path: '/login', element: <LoginPage /> },
      { path: '/', element: <div>Home stub</div> },
    ],
    { initialEntries: ['/login'] },
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

describe('LoginPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
    setCsrfToken(null)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('submits the exact login body and lands on the console', async () => {
    let signedIn = false
    const calls = stubApi({
      'GET /health': () => jsonResponse(HEALTH),
      'GET /auth/config': () => jsonResponse(AUTH_CONFIG),
      'GET /auth/me': () =>
        signedIn
          ? jsonResponse(SESSION_ADMIN)
          : errorResponse(401, 'authentication_error', 'no session'),
      'POST /auth/login': () => {
        signedIn = true
        return jsonResponse({
          user: SESSION_ADMIN.user,
          must_change_password: false,
          csrf_token: CSRF,
        })
      },
    })
    const user = userEvent.setup()
    const router = renderLogin()

    // Tenant is prefilled from the config default.
    expect(await screen.findByLabelText('Tenant')).toHaveValue('acme')
    await user.type(screen.getByLabelText('Username'), 'jane')
    await user.type(screen.getByLabelText('Password'), 'correct horse battery')
    await user.click(screen.getByRole('button', { name: /^sign in$/i }))

    await waitFor(() => expect(router.state.location.pathname).toBe('/'))
    const login = calls.find((c) => c.path === '/auth/login')!
    expect(login.method).toBe('POST')
    expect(JSON.parse(login.body!)).toEqual({
      tenant: 'acme',
      username: 'jane',
      password: 'correct horse battery',
    })
    expect(login.credentials).toBe('same-origin')
    // Nothing secret is persisted; the CSRF token lives in memory only.
    expect(sessionStorage.length).toBe(0)
    expect(localStorage.length).toBe(0)
    expect(getCsrfToken()).toBe(CSRF)
  })

  it('hides the tenant field on a single-tenant gateway and still sends the default', async () => {
    const calls = stubApi({
      'GET /health': () => jsonResponse(HEALTH),
      'GET /auth/config': () =>
        jsonResponse({ ...AUTH_CONFIG, single_tenant: true, default_tenant: 'solo' }),
      'GET /auth/me': () => errorResponse(401, 'authentication_error', 'no session'),
      'POST /auth/login': () => errorResponse(401, 'authentication_error', 'nope'),
    })
    const user = userEvent.setup()
    renderLogin()

    await screen.findByLabelText('Username')
    expect(screen.queryByLabelText('Tenant')).not.toBeInTheDocument()
    await user.type(screen.getByLabelText('Username'), 'jane')
    await user.type(screen.getByLabelText('Password'), 'pw')
    await user.click(screen.getByRole('button', { name: /^sign in$/i }))
    await screen.findByRole('alert')

    const login = calls.find((c) => c.path === '/auth/login')!
    expect(JSON.parse(login.body!)).toEqual({
      tenant: 'solo',
      username: 'jane',
      password: 'pw',
    })
  })

  it('shows one generic error whatever the server says', async () => {
    stubApi({
      'GET /health': () => jsonResponse(HEALTH),
      'GET /auth/config': () => jsonResponse(AUTH_CONFIG),
      'GET /auth/me': () => errorResponse(401, 'authentication_error', 'no session'),
      'POST /auth/login': () =>
        errorResponse(401, 'authentication_error', 'account jane is locked'),
    })
    const user = userEvent.setup()
    renderLogin()

    await user.type(await screen.findByLabelText('Username'), 'jane')
    await user.type(screen.getByLabelText('Password'), 'bad')
    await user.click(screen.getByRole('button', { name: /^sign in$/i }))

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('Invalid username or password')
    expect(alert).not.toHaveTextContent('locked')
  })

  it('offers the API-key toggle only when the config allows it', async () => {
    stubApi({
      'GET /health': () => jsonResponse(HEALTH),
      'GET /auth/config': () => jsonResponse({ ...AUTH_CONFIG, api_key_login: false }),
      'GET /auth/me': () => errorResponse(401, 'authentication_error', 'no session'),
    })
    renderLogin()

    await screen.findByLabelText('Username')
    expect(screen.queryByText(/use an api key/i)).not.toBeInTheDocument()
    expect(screen.queryByLabelText('API key')).not.toBeInTheDocument()
  })

  it('signs in with an API key through the emergency toggle and stores it per tab', async () => {
    const calls = stubApi({
      'GET /health': () => jsonResponse(HEALTH),
      'GET /auth/config': () => jsonResponse(AUTH_CONFIG),
      'GET /auth/me': (call) =>
        call.headers.Authorization
          ? jsonResponse({
              subject: 'k1',
              tenant_id: 't1',
              roles: ['admin'],
              auth_method: 'api_key',
              key_id: 'k1',
              kind: 'api_key',
            })
          : errorResponse(401, 'authentication_error', 'no session'),
    })
    const user = userEvent.setup()
    const router = renderLogin()

    await user.click(await screen.findByText(/use an api key \(emergency access\)/i))
    await user.type(screen.getByLabelText('API key'), 'gk_good_key')
    await user.click(screen.getByRole('button', { name: /^sign in$/i }))

    await waitFor(() => expect(router.state.location.pathname).toBe('/'))
    const keyCall = calls.find((c) => c.headers.Authorization)!
    expect(keyCall.headers.Authorization).toBe('Bearer gk_good_key')
    expect(sessionStorage.getItem(API_KEY_STORAGE_KEY)).toBe('gk_good_key')
  })

  it('shows the inline error for a bad API key', async () => {
    stubApi({
      'GET /health': () => jsonResponse(HEALTH),
      'GET /auth/config': () => jsonResponse(AUTH_CONFIG),
      'GET /auth/me': () =>
        errorResponse(401, 'authentication_error', 'Invalid API key.'),
    })
    const user = userEvent.setup()
    renderLogin()

    await user.click(await screen.findByText(/use an api key \(emergency access\)/i))
    await user.type(screen.getByLabelText('API key'), 'gk_bad_test_key')
    await user.click(screen.getByRole('button', { name: /^sign in$/i }))

    expect(await screen.findByRole('alert')).toHaveTextContent('Invalid API key.')
    expect(sessionStorage.length).toBe(0)
  })

  it('refuses a valid non-admin (agent) key and stores nothing', async () => {
    stubApi({
      'GET /health': () => jsonResponse(HEALTH),
      'GET /auth/config': () => jsonResponse(AUTH_CONFIG),
      'GET /auth/me': (call) =>
        call.headers.Authorization
          ? jsonResponse({
              subject: 'k1',
              tenant_id: 't1',
              roles: ['agent'],
              auth_method: 'apikey',
              key_id: 'k1',
            })
          : errorResponse(401, 'authentication_error', 'no session'),
    })
    const user = userEvent.setup()
    renderLogin()

    await user.click(await screen.findByText(/use an api key \(emergency access\)/i))
    await user.type(screen.getByLabelText('API key'), 'gk_agent_test_key')
    await user.click(screen.getByRole('button', { name: /^sign in$/i }))

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Sign in with an admin key.',
    )
    expect(sessionStorage.length).toBe(0)
  })

  describe('optional tenant', () => {
    const OPTIONAL = { ...AUTH_CONFIG, tenant_optional: true }

    it('hides the tenant field and sends no tenant', async () => {
      const calls = stubApi({
        'GET /health': () => jsonResponse(HEALTH),
        'GET /auth/config': () => jsonResponse(OPTIONAL),
        'GET /auth/me': () => errorResponse(401, 'authentication_error', 'no session'),
        'POST /auth/login': () => errorResponse(401, 'authentication_error', 'nope'),
      })
      const user = userEvent.setup()
      renderLogin()

      await screen.findByLabelText('Username')
      expect(screen.queryByLabelText('Tenant')).not.toBeInTheDocument()
      await user.type(screen.getByLabelText('Username'), 'jane@acme.test')
      await user.type(screen.getByLabelText('Password'), 'pw')
      await user.click(screen.getByRole('button', { name: /^sign in$/i }))
      await screen.findByRole('alert')

      const login = calls.find((c) => c.path === '/auth/login')!
      expect(JSON.parse(login.body!)).toEqual({
        username: 'jane@acme.test',
        password: 'pw',
      })
    })

    it('reveals the tenant field on 409 tenant_required and retries with it', async () => {
      let signedIn = false
      const calls = stubApi({
        'GET /health': () => jsonResponse(HEALTH),
        'GET /auth/config': () => jsonResponse(OPTIONAL),
        'GET /auth/me': () =>
          signedIn
            ? jsonResponse(SESSION_ADMIN)
            : errorResponse(401, 'authentication_error', 'no session'),
        'POST /auth/login': (call) => {
          const body = JSON.parse(call.body!) as { tenant?: string }
          if (!body.tenant) {
            return errorResponse(
              409,
              'tenant_required',
              'username exists in several tenants',
            )
          }
          signedIn = true
          return jsonResponse({
            user: SESSION_ADMIN.user,
            must_change_password: false,
            csrf_token: CSRF,
          })
        },
      })
      const user = userEvent.setup()
      const router = renderLogin()

      await user.type(await screen.findByLabelText('Username'), 'jane')
      await user.type(screen.getByLabelText('Password'), 'correct horse battery')
      await user.click(screen.getByRole('button', { name: /^sign in$/i }))

      // Not an error: the field appears, empty, with an explanation, and the
      // password is kept for the retry.
      const tenantField = await screen.findByLabelText('Tenant')
      expect(tenantField).toHaveValue('')
      expect(screen.getByTestId('tenant-required-notice')).toHaveTextContent(
        'This username exists in more than one tenant.',
      )
      expect(screen.queryByRole('alert')).not.toBeInTheDocument()
      expect(screen.getByLabelText('Password')).toHaveValue('correct horse battery')

      // Submitting without a tenant is caught client-side.
      await user.click(screen.getByRole('button', { name: /^sign in$/i }))
      expect(await screen.findByRole('alert')).toHaveTextContent('Enter your tenant.')
      expect(calls.filter((c) => c.path === '/auth/login')).toHaveLength(1)

      await user.type(tenantField, 'globex')
      await user.click(screen.getByRole('button', { name: /^sign in$/i }))

      await waitFor(() => expect(router.state.location.pathname).toBe('/'))
      const logins = calls.filter((c) => c.path === '/auth/login')
      expect(logins).toHaveLength(2)
      expect(JSON.parse(logins[0]!.body!)).toEqual({
        username: 'jane',
        password: 'correct horse battery',
      })
      expect(JSON.parse(logins[1]!.body!)).toEqual({
        tenant: 'globex',
        username: 'jane',
        password: 'correct horse battery',
      })
    })

    it('still shows the prefilled tenant field when the gateway requires one', async () => {
      stubApi({
        'GET /health': () => jsonResponse(HEALTH),
        'GET /auth/config': () =>
          jsonResponse({ ...AUTH_CONFIG, tenant_optional: false }),
        'GET /auth/me': () => errorResponse(401, 'authentication_error', 'no session'),
      })
      renderLogin()
      expect(await screen.findByLabelText('Tenant')).toHaveValue('acme')
    })
  })

  describe('first run (no console users yet)', () => {
    const FIRST_RUN = {
      ...AUTH_CONFIG,
      single_tenant: true,
      default_tenant: 'solo',
      has_users: false,
    }
    function stubConfig(config: unknown) {
      stubApi({
        'GET /health': () => jsonResponse(HEALTH),
        'GET /auth/config': () => jsonResponse(config),
        'GET /auth/me': () => errorResponse(401, 'authentication_error', 'no session'),
      })
    }

    it('shows the API-key form first, with the notice, when key login is on', async () => {
      stubConfig(FIRST_RUN)
      renderLogin()
      expect(await screen.findByLabelText('API key')).toBeInTheDocument()
      expect(screen.getByTestId('first-run-notice')).toHaveTextContent(
        'No console users yet. Sign in with an admin API key to create one.',
      )
      expect(screen.queryByLabelText('Username')).not.toBeInTheDocument()
      // The username form is still one click away.
      await userEvent
        .setup()
        .click(screen.getByRole('button', { name: /back to username sign-in/i }))
      expect(await screen.findByLabelText('Username')).toBeInTheDocument()
    })

    it('points at `gateway create-user`, with no key form, when key login is off', async () => {
      stubConfig({ ...FIRST_RUN, api_key_login: false })
      renderLogin()
      expect(await screen.findByLabelText('Username')).toBeInTheDocument()
      const notice = screen.getByTestId('first-run-notice')
      expect(notice).toHaveTextContent(
        'No console users yet. Ask your operator to run gateway create-user.',
      )
      expect(screen.queryByLabelText('API key')).not.toBeInTheDocument()
      expect(screen.queryByRole('button', { name: /api key/i })).not.toBeInTheDocument()
    })

    it('shows no notice and the username form when users exist', async () => {
      stubConfig({ ...FIRST_RUN, has_users: true })
      renderLogin()
      expect(await screen.findByLabelText('Username')).toBeInTheDocument()
      expect(screen.queryByTestId('first-run-notice')).not.toBeInTheDocument()
    })
  })
})
