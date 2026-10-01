import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import ConnectorsPage from '@/routes/ConnectorsPage'
import { chooseRowAction } from './rowActions'

const CONNECTOR = {
  id: 'c1',
  name: 'opencti',
  slug: 'opencti',
  endpoint: 'http://opencti-mcp:8001/mcp',
  timeout_ms: 30000,
  status: 'healthy',
  metadata: {
    headers: {
      'X-API-KEY': { type: 'static', value: '***' },
    },
  },
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
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
      <MemoryRouter initialEntries={['/connectors']}>
        <ConnectorsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('ConnectorsPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('renders the connector list', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/connectors'))
          return jsonResponse({ items: [CONNECTOR], total: 1 })
        if (/\/api\/v1\/connectors\/[^/]+\/tools$/.test(url))
          return jsonResponse({ items: [], total: 0 })
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()

    expect(await screen.findByText('opencti')).toBeInTheDocument()
    expect(screen.getByText('http://opencti-mcp:8001/mcp')).toBeInTheDocument()
    expect(screen.getByText('healthy')).toBeInTheDocument()
  })

  it('keeps masked header values untouched when editing', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/connectors') && init?.method === undefined)
        return jsonResponse({ items: [CONNECTOR], total: 1 })
      if (/\/api\/v1\/connectors\/[^/]+\/tools$/.test(url))
        return jsonResponse({ items: [], total: 0 })
      if (url.endsWith('/api/v1/headers/providers'))
        return jsonResponse({ providers: [], resolver_types: [] })
      if (url.endsWith(`/api/v1/connectors/${CONNECTOR.id}`) && init?.method === 'PUT')
        return jsonResponse({ ...CONNECTOR, name: 'opencti-renamed' })
      throw new Error(`Unhandled fetch in test: ${url} ${init?.method}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()

    await screen.findByText('opencti')
    await chooseRowAction(user, 'opencti', 'Edit')

    const nameInput = await screen.findByLabelText(/^name/i)
    await user.clear(nameInput)
    await user.type(nameInput, 'opencti-renamed')

    // The masked header value input is prefilled with "***" and is left
    // untouched by the person editing.
    expect(screen.getByDisplayValue('***')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /save changes/i }))

    await waitFor(() => {
      const putCall = fetchMock.mock.calls.find(
        ([reqInput, init]) =>
          String(reqInput).endsWith(`/api/v1/connectors/${CONNECTOR.id}`) &&
          (init as RequestInit | undefined)?.method === 'PUT',
      )
      expect(putCall).toBeTruthy()
      const body = JSON.parse((putCall![1] as RequestInit).body as string)
      expect(body.name).toBe('opencti-renamed')
      expect(body.metadata.headers['X-API-KEY']).toEqual({ type: 'static', value: '***' })
    })
  })
})
