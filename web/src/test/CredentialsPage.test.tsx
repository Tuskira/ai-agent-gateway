import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import CredentialsPage from '@/routes/CredentialsPage'
import { chooseRowAction } from './rowActions'

const CREDENTIAL = {
  id: 'cred1',
  name: 'opencti',
  type: 'api_key',
  field_names: ['token'],
  key_id: 'key_abc',
  used_by: [{ id: 'c1', name: 'opencti' }],
  created_at: '2026-01-01T00:00:00Z',
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
      <MemoryRouter initialEntries={['/credentials']}>
        <CredentialsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('CredentialsPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('renders the credential list without ever showing a value', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/credentials'))
          return jsonResponse({ items: [CREDENTIAL], total: 1 })
        if (url.endsWith('/api/v1/models')) return jsonResponse({ items: [], total: 0 })
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()

    expect((await screen.findAllByText('opencti')).length).toBeGreaterThan(0)
    expect(screen.getByText('Secret store')).toBeInTheDocument()
    expect(screen.getByText('1 field')).toBeInTheDocument()
    expect(screen.getByText('token')).toBeInTheDocument()
  })

  it('links a used credential to its connector, and shows (unused) otherwise', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/credentials'))
          return jsonResponse({
            items: [
              CREDENTIAL,
              { ...CREDENTIAL, id: 'cred2', name: 'splunk', used_by: [] },
            ],
            total: 2,
          })
        if (url.endsWith('/api/v1/models')) return jsonResponse({ items: [], total: 0 })
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()

    expect(await screen.findByRole('link', { name: /opencti mcp/i })).toHaveAttribute(
      'href',
      '/connectors',
    )
    expect(screen.getByText('(unused)')).toBeInTheDocument()
  })

  it('submits the right body from the create dialog', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/credentials') && init?.method === undefined)
        return jsonResponse({ items: [], total: 0 })
      if (url.endsWith('/api/v1/credentials') && init?.method === 'POST')
        return jsonResponse({
          id: 'cred2',
          name: 'splunk',
          type: 'basic_auth',
          field_names: ['username'],
          key_id: 'key_def',
          created_at: '2026-03-01T00:00:00Z',
        })
      if (url.endsWith('/api/v1/models')) return jsonResponse({ items: [], total: 0 })
      throw new Error(`Unhandled fetch in test: ${url} ${init?.method}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add credential/i }))
    await user.type(screen.getByPlaceholderText(/e\.g\. opencti/i), 'splunk')
    await user.type(screen.getByPlaceholderText(/e\.g\. api_key/i), 'basic_auth')
    await user.type(screen.getByPlaceholderText('field name'), 'username')
    await user.type(screen.getByPlaceholderText('value'), 'hunter2')
    await user.click(screen.getByRole('button', { name: /^add credential$/i }))

    await waitFor(() => {
      const postCall = fetchMock.mock.calls.find(
        ([reqInput, init]) =>
          String(reqInput).endsWith('/api/v1/credentials') &&
          (init as RequestInit | undefined)?.method === 'POST',
      )
      expect(postCall).toBeTruthy()
      const body = JSON.parse((postCall![1] as RequestInit).body as string)
      expect(body).toEqual({
        name: 'splunk',
        type: 'basic_auth',
        payload: { username: 'hunter2' },
      })
    })
  })

  it('asks for confirmation when Delete is chosen from the row menu', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/credentials'))
          return jsonResponse({ items: [CREDENTIAL], total: 1 })
        if (url.endsWith('/api/v1/models')) return jsonResponse({ items: [], total: 0 })
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    const user = userEvent.setup()
    renderPage()

    await chooseRowAction(user, 'opencti', 'Delete')

    expect(await screen.findByText('Delete "opencti"?')).toBeInTheDocument()
  })

  it('warns about models referencing the credential on delete', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/credentials'))
          return jsonResponse({ items: [CREDENTIAL], total: 1 })
        if (url.endsWith('/api/v1/models'))
          return jsonResponse({
            items: [
              {
                id: 'm1',
                name: 'glm-5.3-nebius',
                description: '',
                enabled: true,
                targets: [
                  { vendor: 'openai_compat', model: 'zai-org/GLM-5.3', credential: 'opencti' },
                ],
                price: null,
                limits: null,
                scope: 'tenant',
                metadata: {},
                created_at: '2026-01-01T00:00:00Z',
                updated_at: '2026-01-01T00:00:00Z',
              },
            ],
            total: 1,
          })
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    const user = userEvent.setup()
    renderPage()

    await chooseRowAction(user, 'opencti', 'Delete')

    expect(await screen.findByText(/also used by 1 model/i)).toBeInTheDocument()
    expect(screen.getByText(/glm-5\.3-nebius/)).toBeInTheDocument()
  })
})
