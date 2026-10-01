import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, useLocation } from 'react-router-dom'
import { AuthProvider } from '@/auth/AuthContext'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import type { LlmLogItem } from '@/lib/llm-logs'
import LlmLogsPage from '@/routes/LlmLogsPage'

const PRINCIPAL = {
  subject: 'avinash@tuskira.ai',
  tenant_id: 'tuskira-dev-tenant',
  email: 'avinash@tuskira.ai',
  roles: ['admin'],
  auth_method: 'api_key',
  key_id: 'key_123',
}

const ITEMS: LlmLogItem[] = [
  {
    timestamp: '2026-01-01T10:00:00Z',
    tenant_id: 't1',
    principal: 'alice',
    key_id: 'k1',
    session_id: 's1',
    request_id: 'req-200',
    provider: 'bedrock',
    upstream_host: 'bedrock-runtime.us-east-1.amazonaws.com',
    model: 'claude-sonnet-4-5',
    path: '/v1/messages',
    status_code: 200,
    duration_ms: 850,
    stream: false,
    input_tokens: 1200,
    output_tokens: 340,
    cache_read_tokens: 900,
    cache_creation_tokens: 0,
    stop_reason: 'end_turn',
    provider_request_id: 'prov-200',
    error: null,
    client_name: 'claude-code',
    user_agent: 'claude-cli/2.1.0 (external, cli)',
    source: 'interceptor',
    user: 'person@example.com',
  },
  {
    timestamp: '2026-01-01T10:05:00Z',
    tenant_id: 't1',
    principal: 'bob',
    key_id: 'k1',
    session_id: 's2',
    request_id: 'req-204',
    provider: 'anthropic',
    upstream_host: 'api.anthropic.com',
    model: 'claude-haiku-4-5',
    path: '/v1/messages',
    status_code: 204,
    duration_ms: 120,
    stream: false,
    input_tokens: 50,
    output_tokens: 0,
    cache_read_tokens: 0,
    cache_creation_tokens: 0,
    stop_reason: null,
    provider_request_id: 'prov-204',
    error: null,
  },
  {
    timestamp: '2026-01-01T10:10:00Z',
    tenant_id: 't1',
    principal: 'carol',
    key_id: 'k1',
    session_id: 's3',
    request_id: 'req-slow',
    provider: 'bedrock',
    upstream_host: 'bedrock-runtime.us-east-1.amazonaws.com',
    model: 'claude-opus-4-6',
    path: '/v1/messages',
    status_code: 500,
    duration_ms: 7412,
    stream: true,
    input_tokens: 3000,
    output_tokens: 0,
    cache_read_tokens: 0,
    cache_creation_tokens: 0,
    stop_reason: null,
    provider_request_id: 'prov-slow',
    error: null,
  },
  {
    // A failed call: `error` is set, so the pill must read "error" in red
    // instead of the (possibly still-2xx) HTTP status.
    timestamp: '2026-01-01T10:15:00Z',
    tenant_id: 't1',
    principal: 'dave',
    key_id: 'k1',
    session_id: 's4',
    request_id: 'req-error',
    provider: 'bedrock',
    upstream_host: 'bedrock-runtime.us-east-1.amazonaws.com',
    model: 'claude-sonnet-4-1',
    path: '/v1/messages',
    status_code: 200,
    duration_ms: 300,
    stream: false,
    input_tokens: 400,
    output_tokens: 0,
    cache_read_tokens: 0,
    cache_creation_tokens: 0,
    stop_reason: null,
    provider_request_id: 'prov-error',
    error: 'upstream_timeout',
  },
]

// "Interceptor" renders both as the Source filter's <select> option and as
// the User column/drawer badge on an ingested row; this filters to just the
// badge occurrences.
function interceptorBadges() {
  return screen.getAllByText('Interceptor').filter((el) => el.tagName !== 'OPTION')
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function LocationProbe() {
  const location = useLocation()
  return <div data-testid="location-search">{location.search}</div>
}

function renderPage() {
  sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        <MemoryRouter initialEntries={['/llm-logs']}>
          <LocationProbe />
          <LlmLogsPage />
        </MemoryRouter>
      </AuthProvider>
    </QueryClientProvider>,
  )
}

describe('LlmLogsPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('renders rows with the correct status pill tone and a slow-duration badge', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
        if (url.includes('/api/v1/analytics/llm-logs?'))
          return jsonResponse({ items: ITEMS, total: ITEMS.length })
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()

    expect(await screen.findByText('LLM Logs (4)')).toBeInTheDocument()

    expect(screen.getByText('200')).toHaveClass('bg-status-resolved-bg')
    expect(screen.getByText('204')).toHaveClass('bg-status-open-bg')
    expect(screen.getByText('500')).toHaveClass('bg-sev-high-bg')

    // A failed call shows the literal word "error" in a red pill instead
    // of its (possibly still-2xx) HTTP status.
    expect(screen.getAllByText('200').length).toBe(1)
    expect(screen.getByText('error')).toHaveClass('bg-sev-high-bg')

    const slow = screen.getByText('7412ms')
    expect(slow).toHaveClass('bg-sev-medium-bg')
    expect(screen.getByText('850ms')).toHaveClass('text-text-muted')

    // Stream flag badge only on the streaming row.
    expect(screen.getAllByText('stream').length).toBe(1)

    // Client column: a badge for the row with a classified client.
    expect(screen.getByText('claude-code')).toBeInTheDocument()

    // User column: the interceptor-sourced row (req-200) shows the badge
    // and self-reported email; the other three (gateway-proxied, no
    // source/user set) show the em dash instead. "Interceptor" also
    // appears as a <select> option in the Source filter, so exclude that.
    expect(interceptorBadges().length).toBe(1)
    expect(screen.getByText('person@example.com')).toBeInTheDocument()
  })

  it('filtering by source commits it to the URL and refetches', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
      if (url.includes('/api/v1/analytics/llm-logs?'))
        return jsonResponse({ items: ITEMS, total: ITEMS.length })
      throw new Error(`Unhandled fetch in test: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()
    await screen.findByText('LLM Logs (4)')
    fetchMock.mockClear()

    await user.selectOptions(screen.getByLabelText('Source'), 'interceptor')
    await user.click(screen.getByRole('button', { name: /apply filters/i }))

    expect(screen.getByTestId('location-search').textContent).toContain(
      'source=interceptor',
    )
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(
          ([reqInput]) =>
            String(reqInput).includes('/api/v1/analytics/llm-logs?') &&
            String(reqInput).includes('source=interceptor'),
        ),
      ).toBe(true)
    })
  })

  it('filtering by client commits client_name to the URL and refetches', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
      if (url.includes('/api/v1/analytics/llm-logs?'))
        return jsonResponse({ items: ITEMS, total: ITEMS.length })
      throw new Error(`Unhandled fetch in test: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()
    await screen.findByText('LLM Logs (4)')
    fetchMock.mockClear()

    await user.selectOptions(screen.getByLabelText('Client'), 'claude-code')
    await user.click(screen.getByRole('button', { name: /apply filters/i }))

    expect(screen.getByTestId('location-search').textContent).toContain(
      'client_name=claude-code',
    )
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(
          ([reqInput]) =>
            String(reqInput).includes('/api/v1/analytics/llm-logs?') &&
            String(reqInput).includes('client_name=claude-code'),
        ),
      ).toBe(true)
    })
  })

  it('shows a red "error" pill (not the HTTP status) for a failed call, in the table and the drawer header', async () => {
    const user = userEvent.setup()
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
        if (url.includes('/api/v1/analytics/llm-logs?'))
          return jsonResponse({ items: ITEMS, total: ITEMS.length })
        if (url.endsWith('/api/v1/analytics/llm-logs/req-error')) {
          return jsonResponse({
            ...ITEMS[3],
            headers: {},
            request_body: null,
            response_body: null,
            truncated: false,
          })
        }
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()
    await screen.findByText('claude-sonnet-4-1')

    // Table: the row's Status cell shows "error", not "200".
    expect(screen.getByText('error')).toBeInTheDocument()

    await user.click(screen.getByText('claude-sonnet-4-1'))

    // Drawer header: same red "error" pill. The drawer also repeats the
    // raw error text elsewhere, so filter to the pill-styled occurrences.
    await screen.findByText('Bodies not captured (capture.store_bodies is off).')
    const redPills = screen
      .getAllByText('error')
      .filter((el) => el.classList.contains('bg-sev-high-bg'))
    expect(redPills.length).toBe(2) // table row + drawer header
  })

  it('applying a filter commits it to the URL and refetches', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
      if (url.includes('/api/v1/analytics/llm-logs?'))
        return jsonResponse({ items: ITEMS, total: ITEMS.length })
      throw new Error(`Unhandled fetch in test: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()
    await screen.findByText('LLM Logs (4)')
    fetchMock.mockClear()

    await user.type(screen.getByLabelText('Model'), 'claude-opus-4-6')
    await user.click(screen.getByRole('button', { name: /apply filters/i }))

    expect(screen.getByTestId('location-search').textContent).toContain(
      'model=claude-opus-4-6',
    )
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(
          ([reqInput]) =>
            String(reqInput).includes('/api/v1/analytics/llm-logs?') &&
            String(reqInput).includes('model=claude-opus-4-6'),
        ),
      ).toBe(true)
    })
  })

  it('shows the ClickHouse-sink empty state when the endpoint 404s', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
        if (url.includes('/api/v1/analytics/llm-logs?'))
          return jsonResponse({ error: { type: 'not_found', message: 'not found' } }, 404)
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()

    expect(
      await screen.findByText('Analytics requires the ClickHouse sink.'),
    ).toBeInTheDocument()
  })

  it('opens the detail drawer and shows captured bodies', async () => {
    const user = userEvent.setup()
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
        if (url.includes('/api/v1/analytics/llm-logs?'))
          return jsonResponse({ items: ITEMS, total: ITEMS.length })
        if (url.endsWith('/api/v1/analytics/llm-logs/req-200')) {
          return jsonResponse({
            ...ITEMS[0],
            headers: { 'content-type': 'application/json', 'X-Gateway-Key': '[masked]' },
            request_body: '{"messages":[{"role":"user","content":"hi"}]}',
            response_body: '{"content":[{"type":"text","text":"hello"}]}',
            // Go []byte fields arrive base64; non-ASCII must survive the decode.
            messages: Buffer.from('[{"role":"user","content":"héllo ✓"}]').toString(
              'base64',
            ),
            truncated: true,
          })
        }
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()
    await screen.findByText('claude-sonnet-4-5')

    await user.click(screen.getByText('claude-sonnet-4-5'))

    expect(await screen.findByText('Truncated')).toBeInTheDocument()
    // Credential headers are masked at capture and not listed at all.
    expect(screen.getByText('application/json')).toBeInTheDocument()
    expect(screen.queryByText('X-Gateway-Key')).not.toBeInTheDocument()

    // Client + raw User-Agent come from the list item itself, no detail
    // fetch needed: the table row's badge (still visible) plus the
    // drawer's own "Client" field.
    expect(screen.getAllByText('claude-code').length).toBe(2)
    expect(
      screen.getByText('claude-cli/2.1.0 (external, cli)'),
    ).toBeInTheDocument()

    // Source/User: same story -- table row's User column badge plus the
    // drawer's own "Source"/"User" fields.
    expect(interceptorBadges().length).toBe(2)
    expect(screen.getAllByText('person@example.com').length).toBe(2)

    // Prompt parts and bodies sit behind buttons that open a viewer dialog.
    await user.click(screen.getByRole('button', { name: 'Messages' }))
    expect(await screen.findByRole('textbox', { name: 'Messages' })).toHaveTextContent(
      '"content": "héllo ✓"',
    )
    await user.keyboard('{Escape}')
    // Nothing was captured for these, so there is nothing to open.
    expect(screen.getByRole('button', { name: 'System' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Tools' })).toBeDisabled()

    await user.click(screen.getByRole('button', { name: 'Request body' }))
    expect(
      await screen.findByRole('textbox', { name: 'Request body' }),
    ).toHaveTextContent('"content": "hi"')
    expect(screen.getByText('45 B')).toBeInTheDocument()
    await user.keyboard('{Escape}')
    await user.click(screen.getByRole('button', { name: 'Response body' }))
    expect(
      await screen.findByRole('textbox', { name: 'Response body' }),
    ).toHaveTextContent('"text": "hello"')
  })

  it('clears the search on Escape before closing the body dialog', async () => {
    const user = userEvent.setup()
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
        if (url.includes('/api/v1/analytics/llm-logs?'))
          return jsonResponse({ items: ITEMS, total: ITEMS.length })
        if (url.endsWith('/api/v1/analytics/llm-logs/req-200')) {
          return jsonResponse({
            ...ITEMS[0],
            headers: {},
            request_body: '{"messages":[]}',
            response_body: null,
            truncated: false,
          })
        }
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()
    await user.click(await screen.findByText('claude-sonnet-4-5'))
    await user.click(await screen.findByRole('button', { name: 'Request body' }))
    await screen.findByRole('textbox', { name: 'Request body' })

    await user.type(screen.getByRole('textbox', { name: 'Find' }), 'messages')
    await user.keyboard('{Escape}')
    expect(screen.getByRole('textbox', { name: 'Find' })).toHaveValue('')
    expect(screen.getByRole('textbox', { name: 'Request body' })).toBeInTheDocument()

    await user.keyboard('{Escape}')
    expect(
      screen.queryByRole('textbox', { name: 'Request body' }),
    ).not.toBeInTheDocument()
  })

  it('shows the not-captured message when bodies are empty', async () => {
    const user = userEvent.setup()
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
        if (url.includes('/api/v1/analytics/llm-logs?'))
          return jsonResponse({ items: ITEMS, total: ITEMS.length })
        if (url.endsWith('/api/v1/analytics/llm-logs/req-204')) {
          return jsonResponse({
            ...ITEMS[1],
            headers: {},
            request_body: null,
            response_body: null,
            truncated: false,
          })
        }
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()
    await screen.findByText('claude-haiku-4-5')

    await user.click(screen.getByText('claude-haiku-4-5'))

    expect(
      await screen.findByText('Bodies not captured (capture.store_bodies is off).'),
    ).toBeInTheDocument()
  })

  it('shows why offloaded bodies are unavailable', async () => {
    const user = userEvent.setup()
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
        if (url.includes('/api/v1/analytics/llm-logs?'))
          return jsonResponse({ items: ITEMS, total: ITEMS.length })
        if (url.endsWith('/api/v1/analytics/llm-logs/req-204')) {
          return jsonResponse({
            ...ITEMS[1],
            headers: {},
            truncated: false,
            body_ref: 's3://bucket/llm-bodies/req-204',
            bodies_unavailable: 'body store not configured on this instance',
          })
        }
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()
    await screen.findByText('claude-haiku-4-5')

    await user.click(screen.getByText('claude-haiku-4-5'))

    expect(
      await screen.findByText(
        'Bodies unavailable: body store not configured on this instance.',
      ),
    ).toBeInTheDocument()
  })

  it('requests the next page and records it in the URL', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
      if (url.includes('/api/v1/analytics/llm-logs?'))
        return jsonResponse({ items: ITEMS, total: 60 })
      throw new Error(`Unhandled fetch in test: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)
    const user = userEvent.setup()
    renderPage()

    expect(await screen.findByText('60 items')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Next page' }))

    await waitFor(() => {
      expect(screen.getByTestId('location-search')).toHaveTextContent('offset=25')
    })
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(([reqInput]) => {
          const url = String(reqInput)
          return url.includes('/api/v1/analytics/llm-logs?') && url.includes('offset=25')
        }),
      ).toBe(true)
    })
  })
})
