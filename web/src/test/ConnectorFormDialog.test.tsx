import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import ConnectorsPage from '@/routes/ConnectorsPage'
import { chooseRowAction } from './rowActions'

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

describe('ConnectorFormDialog (v8 two-column create/edit)', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('derives the slug from the name, tracks the live summary, and submits overrides', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/connectors') && (!init || init.method === undefined))
        return jsonResponse({ items: [], total: 0 })
      if (url.endsWith('/api/v1/headers/providers'))
        return jsonResponse({ providers: [], resolver_types: [] })
      if (url.endsWith('/api/v1/connectors') && init?.method === 'POST')
        return jsonResponse({
          id: 'c1',
          name: 'opencti',
          slug: 'opencti',
          endpoint: 'http://opencti-mcp:8001/mcp',
          timeout_ms: 30000,
          status: 'unknown',
          metadata: {},
          created_at: '2026-01-01T00:00:00Z',
          updated_at: '2026-01-01T00:00:00Z',
        })
      throw new Error(`Unhandled fetch in test: ${url} ${init?.method}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add mcp/i }))

    // Nothing filled in yet.
    expect(screen.getByText('0 of 3 required fields')).toBeInTheDocument()
    const createButton = screen.getByRole('button', { name: /^create$/i })
    expect(createButton).toBeDisabled()

    await user.type(screen.getByPlaceholderText(/e\.g\. opencti/i), 'OpenCTI Prod')
    // Slug auto-derives from the name until the person edits it directly.
    expect(screen.getByDisplayValue('opencti-prod')).toBeInTheDocument()
    expect(screen.getByText('2 of 3 required fields')).toBeInTheDocument()

    await user.type(
      screen.getByPlaceholderText(/http:\/\/mcp-server:8001/i),
      'http://opencti-mcp:8001/mcp',
    )
    expect(screen.getByText('3 of 3 required fields')).toBeInTheDocument()
    expect(createButton).not.toBeDisabled()

    // Live summary mirrors the typed name and endpoint.
    expect(screen.getByText('OpenCTI Prod')).toBeInTheDocument()
    expect(screen.getByText('http://opencti-mcp:8001/mcp')).toBeInTheDocument()
    expect(screen.getByText('None')).toBeInTheDocument() // Auth type, no headers yet

    // Add one tool-argument override row.
    await user.click(screen.getByRole('button', { name: /add override/i }))
    await user.type(screen.getByPlaceholderText('Tool name'), 'search_indicators')
    await user.type(screen.getByPlaceholderText(/argument/i), 'customerId')
    await user.type(screen.getByPlaceholderText('Value'), 'acme-corp')

    // Overrides count in the live summary reflects the one valid row.
    const overridesRow = screen.getByText('Overrides').nextElementSibling
    expect(overridesRow).toHaveTextContent('1')

    await user.click(createButton)

    await waitFor(() => {
      const postCall = fetchMock.mock.calls.find(
        ([reqInput, init]) =>
          String(reqInput).endsWith('/api/v1/connectors') &&
          (init as RequestInit | undefined)?.method === 'POST',
      )
      expect(postCall).toBeTruthy()
      const body = JSON.parse((postCall![1] as RequestInit).body as string)
      expect(body.slug).toBe('opencti-prod')
      expect(body.metadata.tool_arg_overrides).toEqual({
        search_indicators: { customerId: 'acme-corp' },
      })
      // Server-initiated requests default to off and, untouched, are
      // omitted from the payload entirely (same treatment as `tls`).
      expect(body.metadata.server_requests).toBeUndefined()
    })
  })

  it('server-initiated request switches default off and are sent only once enabled', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/connectors') && (!init || init.method === undefined))
        return jsonResponse({ items: [], total: 0 })
      if (url.endsWith('/api/v1/headers/providers'))
        return jsonResponse({ providers: [], resolver_types: [] })
      if (url.endsWith('/api/v1/connectors') && init?.method === 'POST')
        return jsonResponse({
          id: 'c1',
          name: 'opencti',
          slug: 'opencti',
          endpoint: 'http://opencti-mcp:8001/mcp',
          timeout_ms: 30000,
          status: 'unknown',
          metadata: {},
          created_at: '2026-01-01T00:00:00Z',
          updated_at: '2026-01-01T00:00:00Z',
        })
      throw new Error(`Unhandled fetch in test: ${url} ${init?.method}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add mcp/i }))

    // Off by default.
    expect(screen.getByRole('switch', { name: 'Sampling' })).not.toBeChecked()
    expect(screen.getByRole('switch', { name: 'Elicitation' })).not.toBeChecked()
    expect(screen.getByRole('switch', { name: 'Roots' })).not.toBeChecked()

    await user.type(screen.getByPlaceholderText(/e\.g\. opencti/i), 'OpenCTI')
    await user.type(
      screen.getByPlaceholderText(/http:\/\/mcp-server:8001/i),
      'http://opencti-mcp:8001/mcp',
    )

    await user.click(screen.getByRole('switch', { name: 'Sampling' }))
    expect(screen.getByRole('switch', { name: 'Sampling' })).toBeChecked()

    await user.click(screen.getByRole('button', { name: /^create$/i }))

    await waitFor(() => {
      const postCall = fetchMock.mock.calls.find(
        ([reqInput, init]) =>
          String(reqInput).endsWith('/api/v1/connectors') &&
          (init as RequestInit | undefined)?.method === 'POST',
      )
      expect(postCall).toBeTruthy()
      const body = JSON.parse((postCall![1] as RequestInit).body as string)
      expect(body.metadata.server_requests).toEqual({
        sampling: true,
        elicitation: false,
        roots: false,
      })
    })
  })

  it('edit pre-fills server-initiated request switches from metadata and preserves them on save', async () => {
    const existing = {
      id: 'c1',
      name: 'OpenCTI Prod',
      slug: 'opencti-prod',
      endpoint: 'http://opencti-mcp:8001/mcp',
      timeout_ms: 30000,
      status: 'healthy',
      metadata: {
        server_requests: { sampling: true, elicitation: false, roots: true },
      },
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
    }
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/connectors') && (!init || init.method === undefined))
        return jsonResponse({ items: [existing], total: 1 })
      if (url.endsWith('/api/v1/headers/providers'))
        return jsonResponse({ providers: [], resolver_types: [] })
      if (url.endsWith(`/api/v1/connectors/${existing.id}/tools`))
        return jsonResponse({ items: [], total: 0 })
      if (url.endsWith(`/api/v1/connectors/${existing.id}`) && init?.method === 'PUT')
        return jsonResponse(existing)
      throw new Error(`Unhandled fetch in test: ${url} ${init?.method}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()

    await chooseRowAction(user, 'OpenCTI Prod', 'Edit')

    expect(await screen.findByRole('switch', { name: 'Sampling' })).toBeChecked()
    expect(screen.getByRole('switch', { name: 'Elicitation' })).not.toBeChecked()
    expect(screen.getByRole('switch', { name: 'Roots' })).toBeChecked()

    await user.click(screen.getByRole('button', { name: /save changes/i }))

    await waitFor(() => {
      const putCall = fetchMock.mock.calls.find(
        ([reqInput, init]) =>
          String(reqInput).endsWith(`/api/v1/connectors/${existing.id}`) &&
          (init as RequestInit | undefined)?.method === 'PUT',
      )
      expect(putCall).toBeTruthy()
      const body = JSON.parse((putCall![1] as RequestInit).body as string)
      expect(body.metadata.server_requests).toEqual({
        sampling: true,
        elicitation: false,
        roots: true,
      })
    })
  })
})
