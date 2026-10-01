import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { setCsrfToken } from '@/lib/api'
import { ApiKeySessionBanner } from '@/components/app/ApiKeySessionBanner'
import { SESSION_ADMIN, jsonResponse, renderWithAuth, stubApi } from './authHelpers'

const KEY_ADMIN = {
  ...SESSION_ADMIN,
  kind: 'api_key',
  user: undefined,
  auth_method: 'api_key',
}
const KEY_VIEWER = { ...KEY_ADMIN, roles: ['viewer'] }

function renderBanner(me: unknown, apiKey: boolean) {
  if (apiKey) sessionStorage.setItem('gateway.api_key', 'gk_test')
  stubApi({ 'GET /auth/me': () => jsonResponse(me) })
  return renderWithAuth(
    <MemoryRouter>
      <ApiKeySessionBanner />
    </MemoryRouter>,
  )
}

describe('ApiKeySessionBanner', () => {
  beforeEach(() => {
    sessionStorage.clear()
    setCsrfToken(null)
  })
  afterEach(() => vi.unstubAllGlobals())

  it('shows for an admin API-key session, with a link to Users', async () => {
    renderBanner(KEY_ADMIN, true)
    const banner = await screen.findByTestId('api-key-banner')
    expect(banner).toHaveTextContent("You're signed in with an API key.")
    expect(banner).toHaveTextContent(
      'API-key sign-in will be removed in a later release.',
    )
    expect(screen.getByRole('link', { name: 'Go to Users' })).toHaveAttribute(
      'href',
      '/users',
    )
  })

  it('stays dismissed for the tab session', async () => {
    const user = userEvent.setup()
    renderBanner(KEY_ADMIN, true)
    await screen.findByTestId('api-key-banner')
    await user.click(screen.getByRole('button', { name: 'Dismiss' }))
    expect(screen.queryByTestId('api-key-banner')).not.toBeInTheDocument()
    expect(sessionStorage.getItem('gateway.apiKeyBannerDismissed')).toBe('1')
  })

  it('is hidden when already dismissed in this tab session', async () => {
    sessionStorage.setItem('gateway.apiKeyBannerDismissed', '1')
    renderBanner(KEY_ADMIN, true)
    await new Promise((r) => setTimeout(r, 50))
    expect(screen.queryByTestId('api-key-banner')).not.toBeInTheDocument()
  })

  it('is not shown for a user session', async () => {
    renderBanner(SESSION_ADMIN, false)
    await new Promise((r) => setTimeout(r, 50))
    expect(screen.queryByTestId('api-key-banner')).not.toBeInTheDocument()
  })

  it('is not shown for a non-admin key', async () => {
    renderBanner(KEY_VIEWER, true)
    await new Promise((r) => setTimeout(r, 50))
    expect(screen.queryByTestId('api-key-banner')).not.toBeInTheDocument()
  })
})
