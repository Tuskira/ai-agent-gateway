import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { setCsrfToken } from '@/lib/api'
import UsersPage from '@/routes/UsersPage'
import { chooseRowAction } from './rowActions'
import {
  CSRF,
  SESSION_ADMIN,
  errorResponse,
  jsonResponse,
  renderWithAuth,
  stubApi,
} from './authHelpers'

const USERS = {
  items: [
    {
      id: 'u-jane',
      username: 'jane',
      display_name: 'Jane',
      role: 'admin',
      disabled: false,
      last_login_at: '2026-09-30T10:00:00Z',
    },
    {
      id: 'u-bob',
      username: 'bob',
      display_name: '',
      role: 'viewer',
      disabled: true,
      last_login_at: null,
    },
  ],
  total: 2,
}

const AUDIT = {
  items: [
    {
      id: 7,
      at: '2026-09-30T10:00:00Z',
      actor_kind: 'user',
      actor_id: 'u-jane',
      action: 'password_reset',
      target_user_id: 'u-bob',
      ip: '10.1.2.3',
      detail: '',
    },
  ],
  total: 1,
}

function renderPage() {
  return renderWithAuth(
    <MemoryRouter initialEntries={['/users']}>
      <UsersPage />
    </MemoryRouter>,
  )
}

describe('UsersPage', () => {
  beforeEach(() => setCsrfToken(null))
  afterEach(() => vi.unstubAllGlobals())

  it('lists users with role, status and last login', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () => jsonResponse(USERS),
    })
    renderPage()

    expect(await screen.findByText('jane')).toBeInTheDocument()
    expect(screen.getByText('bob')).toBeInTheDocument()
    expect(screen.getByText('Active')).toBeInTheDocument()
    expect(screen.getByText('Disabled')).toBeInTheDocument()
    expect(screen.getByText('Never')).toBeInTheDocument()
  })

  it('creates a user with a generated password and shows it exactly once', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () => jsonResponse(USERS),
      'POST /users': () =>
        jsonResponse(
          {
            user: {
              id: 'u-new',
              username: 'newbie',
              display_name: '',
              role: 'viewer',
              disabled: false,
            },
            temporary_password: 'Tmp-Pass-2345-xyz',
          },
          201,
        ),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('jane')
    await user.click(screen.getByRole('button', { name: /create user/i }))
    await user.type(await screen.findByLabelText('Username'), 'newbie')
    await user.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: /create user/i }),
    )

    const revealed = await screen.findByTestId('temporary-password')
    expect(revealed).toHaveTextContent('Tmp-Pass-2345-xyz')
    expect(screen.getByText(/won.t be shown again/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /copy password/i })).toBeInTheDocument()

    const post = calls.find((c) => c.method === 'POST' && c.path === '/users')!
    expect(JSON.parse(post.body!)).toEqual({ username: 'newbie', role: 'viewer' })
    expect(post.headers['X-CSRF-Token']).toBe(CSRF)

    // Closing the dialog drops it; it is not shown anywhere else.
    await user.click(screen.getByRole('button', { name: /^done$/i }))
    await waitFor(() =>
      expect(screen.queryByTestId('temporary-password')).not.toBeInTheDocument(),
    )
    expect(screen.queryByText('Tmp-Pass-2345-xyz')).not.toBeInTheDocument()
  })

  it('sends the chosen password when the admin sets one, and shows no reveal', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () => jsonResponse(USERS),
      'POST /users': () =>
        jsonResponse(
          {
            user: {
              id: 'u-new',
              username: 'carol',
              display_name: 'Carol',
              role: 'admin',
              disabled: false,
            },
          },
          201,
        ),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('jane')
    await user.click(screen.getByRole('button', { name: /create user/i }))
    await user.type(await screen.findByLabelText('Username'), 'carol')
    await user.type(screen.getByLabelText(/display name/i), 'Carol')
    await user.click(screen.getByRole('switch'))
    await user.type(await screen.findByLabelText('Password'), 'short')
    await user.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: /create user/i }),
    )
    expect(await screen.findByText('Use at least 12 characters')).toBeInTheDocument()

    await user.clear(screen.getByLabelText('Password'))
    await user.type(screen.getByLabelText('Password'), 'a long enough passphrase')
    await user.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: /create user/i }),
    )

    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    const post = calls.find((c) => c.method === 'POST')!
    expect(JSON.parse(post.body!)).toEqual({
      username: 'carol',
      display_name: 'Carol',
      role: 'viewer',
      password: 'a long enough passphrase',
    })
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(screen.queryByTestId('temporary-password')).not.toBeInTheDocument()
  })

  it('reset password: confirms, then shows the temporary password once', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () => jsonResponse(USERS),
      'POST /users/u-bob/reset-password': () =>
        jsonResponse({ temporary_password: 'Reset-Me-9876-abc' }),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('bob')
    await chooseRowAction(user, 'bob', 'Reset password')
    expect(await screen.findByText(/reset password for "bob"/i)).toBeInTheDocument()
    expect(calls.some((c) => c.path.endsWith('/reset-password'))).toBe(false)
    await user.click(screen.getByRole('button', { name: /^reset password$/i }))

    expect(await screen.findByTestId('temporary-password')).toHaveTextContent(
      'Reset-Me-9876-abc',
    )
    const post = calls.find((c) => c.path === '/users/u-bob/reset-password')!
    expect(post.headers['X-CSRF-Token']).toBe(CSRF)

    await user.click(screen.getByRole('button', { name: /^done$/i }))
    await waitFor(() =>
      expect(screen.queryByTestId('temporary-password')).not.toBeInTheDocument(),
    )
  })

  it('explains a last-admin 409 inline', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () => jsonResponse(USERS),
      'DELETE /users/u-jane': () =>
        errorResponse(409, 'last_admin', 'cannot remove the last admin'),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('jane')
    await chooseRowAction(user, 'jane', 'Delete')
    await user.click(await screen.findByRole('button', { name: /^delete$/i }))

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/last active admin/i)
  })

  it('shows a self-action 409 message from the server', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () => jsonResponse(USERS),
      'PATCH /users/u-jane': () =>
        errorResponse(409, 'self_action', 'you cannot disable yourself'),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('jane')
    await chooseRowAction(user, 'jane', 'Disable')

    expect(await screen.findByRole('alert')).toHaveTextContent(/your own account/i)
  })

  it('disables and enables with a PATCH', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () => jsonResponse(USERS),
      'PATCH /users/u-bob': () => jsonResponse({ ...USERS.items[1], disabled: false }),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('bob')
    await chooseRowAction(user, 'bob', 'Enable')

    await waitFor(() => expect(calls.some((c) => c.method === 'PATCH')).toBe(true))
    const patch = calls.find((c) => c.method === 'PATCH')!
    expect(JSON.parse(patch.body!)).toEqual({ disabled: false })
    expect(patch.headers['X-CSRF-Token']).toBe(CSRF)
  })

  it('edits display name and role', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () => jsonResponse(USERS),
      'PATCH /users/u-bob': () => jsonResponse(USERS.items[1]),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('bob')
    await chooseRowAction(user, 'bob', 'Edit')
    await user.type(await screen.findByLabelText('Display name'), 'Bobby')
    await user.click(screen.getByRole('button', { name: /^save$/i }))

    await waitFor(() => expect(calls.some((c) => c.method === 'PATCH')).toBe(true))
    expect(JSON.parse(calls.find((c) => c.method === 'PATCH')!.body!)).toEqual({
      display_name: 'Bobby',
    })
  })

  it('revokes sessions after a confirmation', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () => jsonResponse(USERS),
      'POST /users/u-bob/revoke-sessions': () => new Response(null, { status: 204 }),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('bob')
    await chooseRowAction(user, 'bob', 'Revoke sessions')
    await user.click(await screen.findByRole('button', { name: /^revoke sessions$/i }))

    await waitFor(() =>
      expect(calls.some((c) => c.path === '/users/u-bob/revoke-sessions')).toBe(true),
    )
  })

  it('lists the audit log on its own tab', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () => jsonResponse(USERS),
      'GET /auth/audit': () => jsonResponse(AUDIT),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('jane')
    await user.click(screen.getByRole('tab', { name: /audit log/i }))

    expect(await screen.findByText('password_reset')).toBeInTheDocument()
    expect(screen.getByText('10.1.2.3')).toBeInTheDocument()
    // Actor and target are resolved to usernames.
    expect(screen.getByText('user:jane')).toBeInTheDocument()
    expect(screen.getByText('bob')).toBeInTheDocument()
  })

  it('shows the admin-required state on 403', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () => errorResponse(403, 'permission_denied', 'forbidden'),
    })
    renderPage()

    expect(await screen.findByText('Admin required')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /create user/i })).not.toBeInTheDocument()
  })

  describe('no-admin lockout warning', () => {
    const KEY_PRINCIPAL = {
      ...SESSION_ADMIN,
      kind: 'api_key',
      user: undefined,
      auth_method: 'api_key',
      key_id: 'k1',
    }
    const ONLY_VIEWERS = {
      items: [{ ...USERS.items[1], disabled: false }],
      total: 1,
    }

    function stub(me: unknown, users: unknown) {
      stubApi({
        'GET /auth/me': () => jsonResponse(me),
        'GET /users': () => jsonResponse(users),
        'GET /auth/audit': () => jsonResponse(AUDIT),
      })
    }

    afterEach(() => sessionStorage.clear())

    it('warns an API-key session when no active admin exists', async () => {
      sessionStorage.setItem('gateway.api_key', 'gk_test')
      stub(KEY_PRINCIPAL, ONLY_VIEWERS)
      renderPage()
      expect(await screen.findByTestId('no-admin-warning')).toHaveTextContent(
        'No active admin user exists. If API-key sign-in is turned off, nobody can sign in to this console. Create an admin user now.',
      )
    })

    it('does not warn an API-key session when an active admin exists', async () => {
      sessionStorage.setItem('gateway.api_key', 'gk_test')
      stub(KEY_PRINCIPAL, USERS)
      renderPage()
      await screen.findByText('jane')
      expect(screen.queryByTestId('no-admin-warning')).not.toBeInTheDocument()
    })

    it('does not warn a user session', async () => {
      stub(SESSION_ADMIN, ONLY_VIEWERS)
      renderPage()
      await screen.findByText('bob')
      expect(screen.queryByTestId('no-admin-warning')).not.toBeInTheDocument()
    })
  })
})
