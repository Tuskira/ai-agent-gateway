import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, useLocation } from 'react-router-dom'
import { AuthProvider } from '@/auth/AuthContext'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import type { AccessLogItem } from '@/lib/logs'
import AccessLogsPage from '@/routes/AccessLogsPage'

const PRINCIPAL = {
  subject: 'avinash@tuskira.ai',
  tenant_id: 'tuskira-dev-tenant',
  email: 'avinash@tuskira.ai',
  roles: ['admin'],
  auth_method: 'api_key',
  key_id: 'key_123',
}

const CONNECTORS = {
  items: [{ id: 'c1', name: 'opencti', slug: 'opencti', status: 'healthy' }],
  total: 1,
}

const ITEMS: AccessLogItem[] = [
  {
    timestamp: '2026-01-01T10:00:00Z',
    tenant_id: 't1',
    principal: 'alice',
    key_id: 'k1',
    session_id: 's1',
    request_id: 'req-200',
    correlation_id: 'c-200',
    trace_id: 'tr-200',
    method: 'tools/call',
    json_rpc_id: null,
    connector_id: 'c1',
    tool_name: 'search_indicators',
    status_code: 200,
    error_code: null,
    duration_ms: 420,
    bytes_in: 512,
    bytes: 2048,
    client_ip: '10.0.0.1',
    user_agent: 'claude-code/1.0',
    profile: 'SOC Agent',
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
    correlation_id: 'c-204',
    trace_id: 'tr-204',
    method: 'tools/list',
    json_rpc_id: null,
    connector_id: 'c1',
    tool_name: 'list_incidents',
    status_code: 204,
    error_code: null,
    duration_ms: 180,
    bytes_in: 256,
    bytes: 0,
    client_ip: '10.0.0.2',
    user_agent: 'claude-code/1.0',
    profile: 'SOC Agent',
  },
  {
    timestamp: '2026-01-01T10:10:00Z',
    tenant_id: 't1',
    principal: 'carol',
    key_id: 'k1',
    session_id: 's3',
    request_id: 'req-slow',
    correlation_id: 'c-slow',
    trace_id: 'tr-slow',
    method: 'tools/call',
    json_rpc_id: null,
    connector_id: 'c1',
    tool_name: 'run_query',
    status_code: 500,
    error_code: null,
    duration_ms: 6234,
    bytes_in: 1024,
    bytes: 128,
    client_ip: '10.0.0.3',
    user_agent: 'claude-code/1.0',
    profile: 'SOC Agent',
  },
  {
    // A denied tools/call: the JSON-RPC error rides back on HTTP 200, so
    // the pill must show the error code in red instead of a misleading
    // green "200".
    timestamp: '2026-01-01T10:15:00Z',
    tenant_id: 't1',
    principal: 'dave',
    key_id: 'k1',
    session_id: 's4',
    request_id: 'req-denied',
    correlation_id: 'c-denied',
    trace_id: 'tr-denied',
    method: 'tools/call',
    json_rpc_id: null,
    connector_id: 'c1',
    tool_name: 'block_ip',
    status_code: 200,
    error_code: '-32003',
    duration_ms: 210,
    bytes_in: 256,
    bytes: 96,
    client_ip: '10.0.0.4',
    user_agent: 'claude-code/1.0',
    profile: 'SOC Agent',
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
        <MemoryRouter initialEntries={['/access-logs']}>
          <LocationProbe />
          <AccessLogsPage />
        </MemoryRouter>
      </AuthProvider>
    </QueryClientProvider>,
  )
}

describe('AccessLogsPage', () => {
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
        if (url.endsWith('/api/v1/connectors')) return jsonResponse(CONNECTORS)
        if (url.includes('/api/v1/analytics/logs?'))
          return jsonResponse({ items: ITEMS, total: ITEMS.length })
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()

    expect(await screen.findByText('Access Logs (4)')).toBeInTheDocument()

    expect(screen.getByText('200')).toHaveClass('bg-status-resolved-bg')
    expect(screen.getByText('204')).toHaveClass('bg-status-open-bg')
    expect(screen.getByText('500')).toHaveClass('bg-sev-high-bg')

    // A denied call (JSON-RPC error over HTTP 200) shows the error code in
    // a red pill instead of a misleading green "200" — only one row shows
    // plain "200", the denied row shows "-32003" instead.
    expect(screen.getAllByText('200').length).toBe(1)
    expect(screen.getByText('-32003')).toHaveClass('bg-sev-high-bg')

    // The slow row (>5000ms) gets the orange clock badge; the fast rows
    // render plain muted text instead.
    const slow = screen.getByText('6234ms')
    expect(slow).toHaveClass('bg-sev-medium-bg')
    expect(screen.getByText('420ms')).toHaveClass('text-text-muted')

    // Connector id resolved to its name via /connectors.
    expect(screen.getAllByText('opencti').length).toBe(4)

    // User column: the interceptor-sourced row (req-200) shows the badge
    // and self-reported email; the other three (gateway-proxied, no
    // source/user set) show the em dash instead.
    expect(interceptorBadges().length).toBe(1)
    expect(screen.getByText('person@example.com')).toBeInTheDocument()
  })

  it('filtering by source commits it to the URL and refetches', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
      if (url.endsWith('/api/v1/connectors')) return jsonResponse(CONNECTORS)
      if (url.includes('/api/v1/analytics/logs?'))
        return jsonResponse({ items: ITEMS, total: ITEMS.length })
      throw new Error(`Unhandled fetch in test: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()
    await screen.findByText('Access Logs (4)')
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
            String(reqInput).includes('/api/v1/analytics/logs?') &&
            String(reqInput).includes('source=interceptor'),
        ),
      ).toBe(true)
    })
  })

  it('shows a red error pill (not the HTTP status) for a denied call, in the table and the drawer header', async () => {
    const user = userEvent.setup()
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
        if (url.endsWith('/api/v1/connectors')) return jsonResponse(CONNECTORS)
        if (url.includes('/api/v1/analytics/logs?'))
          return jsonResponse({ items: ITEMS, total: ITEMS.length })
        if (url.endsWith('/api/v1/analytics/logs/req-denied')) {
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
    await screen.findByText('block_ip')

    // Table: the row's Status cell shows the error code, not "200".
    expect(screen.getByText('-32003')).toBeInTheDocument()

    await user.click(screen.getByText('block_ip'))

    // Drawer header: same red error-code pill (the drawer also repeats the
    // code as plain text in its "Error code" definition-list field, so
    // filter to the pill-styled occurrences specifically).
    await screen.findByText('Bodies not captured (capture.store_bodies is off).')
    const redPills = screen
      .getAllByText('-32003')
      .filter((el) => el.classList.contains('bg-sev-high-bg'))
    expect(redPills.length).toBe(2) // table row + drawer header
  })

  it('applying a filter commits it to the URL and refetches', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
      if (url.endsWith('/api/v1/connectors')) return jsonResponse(CONNECTORS)
      if (url.includes('/api/v1/analytics/logs?'))
        return jsonResponse({ items: ITEMS, total: ITEMS.length })
      throw new Error(`Unhandled fetch in test: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()
    await screen.findByText('Access Logs (4)')
    fetchMock.mockClear()

    await user.selectOptions(screen.getByLabelText('Method'), 'tools/call')
    await user.click(screen.getByRole('button', { name: /apply filters/i }))

    // '/' in the JSON-RPC method name is URL-encoded as %2F.
    expect(screen.getByTestId('location-search').textContent).toContain(
      'method=tools%2Fcall',
    )
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(
          ([reqInput]) =>
            String(reqInput).includes('/api/v1/analytics/logs?') &&
            String(reqInput).includes('method=tools%2Fcall'),
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
        if (url.endsWith('/api/v1/connectors')) return jsonResponse(CONNECTORS)
        if (url.includes('/api/v1/analytics/logs?'))
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
        if (url.endsWith('/api/v1/connectors')) return jsonResponse(CONNECTORS)
        if (url.includes('/api/v1/analytics/logs?'))
          return jsonResponse({ items: ITEMS, total: ITEMS.length })
        if (url.endsWith('/api/v1/analytics/logs/req-200')) {
          return jsonResponse({
            ...ITEMS[0],
            headers: { 'content-type': 'application/json' },
            request_body: '{"query":"apt29"}',
            response_body: '{"results":[]}',
            truncated: false,
          })
        }
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()
    await screen.findByText('search_indicators')

    await user.click(screen.getByText('search_indicators'))

    // Source/User come from the list item itself, no detail fetch needed:
    // the table row's badge (still visible) plus the drawer's own fields.
    expect(interceptorBadges().length).toBe(2)
    expect(screen.getAllByText('person@example.com').length).toBe(2)

    // Bodies sit behind buttons that open a viewer dialog.
    await user.click(await screen.findByRole('button', { name: 'Request body' }))
    expect(
      await screen.findByRole('textbox', { name: 'Request body' }),
    ).toHaveTextContent('"query": "apt29"')
    await user.keyboard('{Escape}')
    await user.click(screen.getByRole('button', { name: 'Response body' }))
    expect(
      await screen.findByRole('textbox', { name: 'Response body' }),
    ).toHaveTextContent('"results": []')
  })

  it('shows the not-captured message when bodies are empty', async () => {
    const user = userEvent.setup()
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
        if (url.endsWith('/api/v1/connectors')) return jsonResponse(CONNECTORS)
        if (url.includes('/api/v1/analytics/logs?'))
          return jsonResponse({ items: ITEMS, total: ITEMS.length })
        if (url.endsWith('/api/v1/analytics/logs/req-204')) {
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
    await screen.findByText('list_incidents')

    await user.click(screen.getByText('list_incidents'))

    expect(
      await screen.findByText('Bodies not captured (capture.store_bodies is off).'),
    ).toBeInTheDocument()
  })

  it('requests the next page and records it in the URL', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
      if (url.endsWith('/api/v1/connectors')) return jsonResponse(CONNECTORS)
      if (url.includes('/api/v1/analytics/logs?'))
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
          return url.includes('/api/v1/analytics/logs?') && url.includes('offset=25')
        }),
      ).toBe(true)
    })
    // The log endpoints cannot sort, so no header offers it.
    expect(screen.getByRole('columnheader', { name: 'Timestamp' })).not.toHaveAttribute(
      'aria-sort',
    )
  })

  it('leaves the connector cell to its own tooltip', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
        if (url.endsWith('/api/v1/connectors'))
          return jsonResponse({
            items: [{ ...CONNECTORS.items[0], name: 'opencti-production-eu-west' }],
            total: 1,
          })
        if (url.includes('/api/v1/analytics/logs?'))
          return jsonResponse({ items: [ITEMS[0]], total: 1 })
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()

    // The name's own trigger shows the connector id; a second trigger around
    // it would show a competing tooltip on the same hover.
    const name = await screen.findByText('opencti-production-eu-west')
    expect(name).toHaveAttribute('data-slot', 'tooltip-trigger')
    expect(name.parentElement?.closest('[data-slot="tooltip-trigger"]')).toBeNull()
  })
})
