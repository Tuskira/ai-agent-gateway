import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { AuthProvider } from '@/auth/AuthContext'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import ApiKeysPage from '@/routes/ApiKeysPage'
import { chooseRowAction } from './rowActions'

const PRINCIPAL = {
  subject: 'avinash@tuskira.ai',
  tenant_id: 'tuskira-dev-tenant-0123456789',
  email: 'avinash@tuskira.ai',
  roles: ['admin'],
  auth_method: 'api_key',
  key_id: 'key_123',
}

const KEYS = {
  items: [
    {
      id: 'k1',
      name: 'CI pipeline',
      role: 'agent',
      prefix: 'gk_ab12',
      created_at: '2026-01-01T00:00:00Z',
      last_used_at: '2026-02-01T00:00:00Z',
    },
    {
      id: 'k2',
      name: 'Old key',
      role: 'agent',
      prefix: 'gk_old1',
      created_at: '2026-01-01T00:00:00Z',
      revoked_at: '2026-02-01T00:00:00Z',
    },
  ],
  total: 2,
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function renderPage() {
  sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        <MemoryRouter initialEntries={['/api-keys']}>
          <ApiKeysPage />
        </MemoryRouter>
      </AuthProvider>
    </QueryClientProvider>,
  )
}

describe('ApiKeysPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('renders the key list with a tenant column and Active/Revoked status', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
        if (url.endsWith('/api/v1/api-keys')) return jsonResponse(KEYS)
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()

    expect(await screen.findByText('CI pipeline')).toBeInTheDocument()
    expect(screen.getByText('gk_ab12')).toBeInTheDocument()
    expect(screen.getByText('Active')).toBeInTheDocument()
    expect(screen.getByText('Revoked')).toBeInTheDocument()
    // Tenant column shows a shortened id, derived from the signed-in principal.
    expect(screen.getAllByText('tuskira-…456789').length).toBe(2)
    expect(
      screen.getByText('Keys are shown once at creation and stored hashed.', {
        exact: false,
      }),
    ).toBeInTheDocument()
  })

  it('submits the right body from the create dialog', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
      if (url.endsWith('/api/v1/api-keys') && (!init || init.method === undefined)) {
        return jsonResponse({ items: [], total: 0 })
      }
      if (url.endsWith('/api/v1/api-keys') && init?.method === 'POST') {
        return jsonResponse({
          id: 'k2',
          name: 'New key',
          role: 'admin',
          prefix: 'gk_new1',
          created_at: '2026-03-01T00:00:00Z',
          key: 'gk_new1_fullsecretvalue',
        })
      }
      throw new Error(`Unhandled fetch in test: ${url} ${init?.method}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /create key/i }))
    await user.type(screen.getByPlaceholderText(/CI pipeline/i), 'New key')

    // Role select defaults to "agent" — submit without changing it.
    await user.click(screen.getByRole('button', { name: /^create key$/i }))

    await waitFor(() => {
      const postCall = fetchMock.mock.calls.find(
        ([reqInput, init]) =>
          String(reqInput).endsWith('/api/v1/api-keys') &&
          (init as RequestInit | undefined)?.method === 'POST',
      )
      expect(postCall).toBeTruthy()
      const body = JSON.parse((postCall![1] as RequestInit).body as string)
      expect(body).toEqual({ name: 'New key', role: 'agent' })
    })

    // The freshly created key is revealed once.
    expect(await screen.findByText('gk_new1_fullsecretvalue')).toBeInTheDocument()
  })

  it('shows an admin-required empty state on a 403', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
        if (url.endsWith('/api/v1/api-keys')) {
          return jsonResponse(
            { error: { type: 'forbidden', message: 'admin role required' } },
            403,
          )
        }
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()

    expect(await screen.findByText('Admin key required')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /create key/i })).not.toBeInTheDocument()
  })

  it('offers rotate and revoke from the row menu, but not on a revoked key', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
        if (url.endsWith('/api/v1/api-keys')) return jsonResponse(KEYS)
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('CI pipeline')
    expect(
      screen.queryByRole('button', { name: 'Actions for Old key' }),
    ).not.toBeInTheDocument()
    expect(screen.getByText('Old key').closest('tr')).toHaveClass('opacity-55')

    await chooseRowAction(user, 'CI pipeline', 'Revoke')

    expect(await screen.findByText('Revoke "CI pipeline"?')).toBeInTheDocument()
  })
})
