import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, useLocation } from 'react-router-dom'
import { AuthProvider } from '@/auth/AuthContext'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import type { SessionTimeline } from '@/lib/session-timeline'
import SessionTimelinePage from '@/routes/SessionTimelinePage'

const PRINCIPAL = {
  subject: 'admin@example.com',
  tenant_id: 'example-tenant',
  email: 'admin@example.com',
  roles: ['admin'],
  auth_method: 'api_key',
  key_id: 'key_123',
}

const MCP_EVENT = {
  ts: '2026-01-01T10:00:00Z',
  plane: 'mcp' as const,
  kind: 'tools/call',
  name: 'search_indicators',
  status: 'success' as const,
  duration_ms: 420,
  key_id: 'k1',
  id: 'req-mcp',
}

const LLM_EVENT = {
  ts: '2026-01-01T10:01:00Z',
  plane: 'llm' as const,
  kind: 'llm_call',
  name: 'claude-sonnet-4-5',
  status: 'success' as const,
  duration_ms: 850,
  key_id: 'k1',
  id: 'req-llm',
}

function timeline(events: SessionTimeline['events'], excluded = 0): SessionTimeline {
  return {
    session_id: 's-1',
    owner_key_id: 'k1',
    events,
    total_events: events.length,
    excluded_foreign_events: excluded,
  }
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

type Result = SessionTimeline | 'missing' | 'failing'

const LLM_DETAIL = {
  timestamp: LLM_EVENT.ts,
  tenant_id: 't',
  principal: 'k1',
  key_id: 'k1',
  session_id: 's-1',
  request_id: 'req-llm',
  provider: 'anthropic',
  upstream_host: 'api.anthropic.com',
  model: 'claude-sonnet-4-5',
  path: '/v1/messages',
  status_code: 200,
  duration_ms: 850,
  stream: false,
  input_tokens: 1234,
  output_tokens: 56,
  cache_read_tokens: 0,
  cache_creation_tokens: 0,
  stop_reason: 'end_turn',
  headers: {},
  truncated: false,
}

const MCP_DETAIL = {
  timestamp: MCP_EVENT.ts,
  tenant_id: 't',
  principal: 'k1',
  key_id: 'k1',
  session_id: 's-1',
  request_id: 'req-mcp',
  correlation_id: '',
  trace_id: '',
  method: 'tools/call',
  connector_id: 'c1',
  tool_name: 'search_indicators',
  status_code: 200,
  duration_ms: 420,
  bytes_in: 10,
  bytes: 20,
  client_ip: '10.0.0.1',
  user_agent: 'agent/1',
  profile: '',
  headers: {},
  truncated: false,
}

/** Stubs fetch with the given timeline response ('missing' answers 404, as
 * when the ClickHouse sink is off; 'failing' answers 500) and renders the
 * page on session s-1. */
function LocationProbe() {
  return <div data-testid="search">{useLocation().search}</div>
}

function renderTimeline(
  result: Result,
  details = true,
  opts: { entry?: string; byOrder?: Partial<Record<'asc' | 'desc', SessionTimeline>> } = {},
) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === 'string' ? input : input.toString()
    if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
    if (url.endsWith('/api/v1/analytics/llm-logs/req-llm'))
      return details
        ? jsonResponse(LLM_DETAIL)
        : jsonResponse({ error: { type: 'not_found', message: 'gone' } }, 404)
    if (url.endsWith('/api/v1/analytics/logs/req-mcp'))
      return details
        ? jsonResponse(MCP_DETAIL)
        : jsonResponse({ error: { type: 'not_found', message: 'gone' } }, 404)
    if (url.includes('/api/v1/connectors'))
      return jsonResponse({ items: [{ id: 'c1', name: 'threat-intel' }], total: 1 })
    if (url.includes('/api/v1/api-keys'))
      return jsonResponse({
        items: [{ id: 'k1', name: 'laptop', role: 'agent', prefix: 'gk_abcd1234' }],
        total: 1,
      })
    if (url.includes('/api/v1/analytics/sessions/s-1/timeline')) {
      const order = new URL(url, 'http://x').searchParams.get('order') === 'desc' ? 'desc' : 'asc'
      const byOrder = opts.byOrder?.[order]
      if (byOrder) return jsonResponse(byOrder)
      if (result === 'missing')
        return jsonResponse({ error: { type: 'not_found', message: 'not found' } }, 404)
      if (result === 'failing')
        return jsonResponse({ error: { type: 'internal', message: 'boom' } }, 500)
      return jsonResponse(result)
    }
    throw new Error(`Unhandled fetch in test: ${url}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        <MemoryRouter initialEntries={[opts.entry ?? '/session-timeline?session_id=s-1']}>
          <SessionTimelinePage />
          <LocationProbe />
        </MemoryRouter>
      </AuthProvider>
    </QueryClientProvider>,
  )
  return fetchMock
}

describe('SessionTimelinePage', () => {
  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('renders both planes in time order, the session id tile, and opens the event sheet', async () => {
    const fetchMock = renderTimeline(timeline([MCP_EVENT, LLM_EVENT]))
    const user = userEvent.setup()

    const list = await screen.findByRole('list')
    // Oldest first: the MCP call happened a minute before the LLM call.
    expect(
      within(list)
        .getAllByText(/^(LLM|MCP)$/)
        .map((el) => el.textContent),
    ).toEqual(['MCP', 'LLM'])
    expect(screen.getByText('2')).toBeInTheDocument()
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    expect(
      fetchMock.mock.calls.some(([req]) =>
        String(req).includes('/api/v1/analytics/sessions/s-1/timeline'),
      ),
    ).toBe(true)

    // Session ID stat tile: truncated, full id in the tooltip, copy button.
    expect(screen.getByTitle('s-1')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Copy session ID' })).toBeInTheDocument()

    // A row opens the full log drawer, as on the LLM Logs page.
    await user.click(within(list).getByText('claude-sonnet-4-5'))
    const llmDrawer = await screen.findByRole('dialog')
    expect(await within(llmDrawer).findByText('end_turn')).toBeInTheDocument()
    expect(within(llmDrawer).getByText('Input tokens')).toBeInTheDocument()
    await user.keyboard('{Escape}')

    // And an MCP row the Access Logs drawer, with the connector's name.
    await user.click(within(list).getByText('search_indicators'))
    const mcpDrawer = await screen.findByRole('dialog')
    expect(await within(mcpDrawer).findByText('threat-intel')).toBeInTheDocument()
  })

  it('shows a timestamp and readable duration in fixed-width tabular cells', async () => {
    renderTimeline(
      timeline([
        { ...MCP_EVENT, duration_ms: 233777 },
        { ...LLM_EVENT, duration_ms: 984 },
      ]),
    )
    const list = await screen.findByRole('list')
    const rows = within(list).getAllByRole('button')
    expect(rows).toHaveLength(2)

    const duration = within(rows[0]!).getByText('3m 53.8s')
    expect(duration.closest('span.w-28')).toHaveClass('justify-end', 'text-right', 'tabular-nums')
    expect(within(rows[1]!).getByText('984 ms')).toBeInTheDocument()

    // The time cell is the one holding the compact "D Mon HH:mm:ss" text.
    const time = within(rows[0]!).getByText(/^\d{1,2} [A-Z][a-z]{2}( \d{4})? \d{2}:\d{2}:\d{2}$/)
    expect(time).toHaveClass('w-36', 'text-right', 'tabular-nums')
    expect(screen.getByText(/^Times in .+ \(.+\)$/)).toBeInTheDocument()
  })

  it('falls back to the event summary when the full log cannot be read', async () => {
    renderTimeline(timeline([MCP_EVENT, LLM_EVENT]), false)
    const user = userEvent.setup()
    const list = await screen.findByRole('list')
    await user.click(within(list).getByText('claude-sonnet-4-5'))
    const sheet = await screen.findByRole('dialog')
    expect(await within(sheet).findByText('laptop (gk_abcd1234)')).toBeInTheDocument()
  })

  it('shows the ClickHouse-sink empty state when the timeline endpoint 404s', async () => {
    renderTimeline('missing')
    expect(
      await screen.findByText('Analytics requires the ClickHouse sink.'),
    ).toBeInTheDocument()
  })

  it('shows an error when the timeline endpoint fails', async () => {
    renderTimeline('failing')
    expect(await screen.findByText("Couldn't load this session.")).toBeInTheDocument()
  })

  it('counts a failure and filters by event type', async () => {
    renderTimeline(
      timeline([
        MCP_EVENT,
        { ...MCP_EVENT, id: 'req-mcp-2', status: 'error' },
        LLM_EVENT,
        { ...LLM_EVENT, id: 'req-llm-2', status: 'error' },
      ]),
    )
    const user = userEvent.setup()

    const list = await screen.findByRole('list')
    expect(within(list).getAllByRole('button')).toHaveLength(4)
    // Two errors out of four events.
    expect(screen.getByText('50.0% error rate')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'MCP' }))
    expect(
      within(screen.getByRole('list'))
        .getAllByText(/^(LLM|MCP)$/)
        .map((el) => el.textContent),
    ).toEqual(['MCP', 'MCP'])
  })

  it('reports events withheld from another key', async () => {
    renderTimeline(timeline([MCP_EVENT, LLM_EVENT], 3))
    expect(await screen.findByRole('status')).toHaveTextContent(
      "3 events from another key matched this session's tag and were withheld.",
    )
  })

  it('filters the visible events by start/end date, without changing the stat tiles', async () => {
    const dayOne = { ...MCP_EVENT, id: 'req-day1', ts: '2026-01-01T10:00:00Z' }
    const dayTwo = { ...LLM_EVENT, id: 'req-day2', ts: '2026-01-05T10:00:00Z' }
    renderTimeline(timeline([dayOne, dayTwo]))
    const user = userEvent.setup()

    const list = await screen.findByRole('list')
    expect(within(list).getAllByRole('button')).toHaveLength(2)
    // Stat tile totals cover the whole session, unaffected by filters.
    expect(screen.getByText('2')).toBeInTheDocument()

    await user.type(screen.getByLabelText('Start Date'), '2026-01-03')
    expect(within(screen.getByRole('list')).getAllByRole('button')).toHaveLength(1)
    expect(
      within(screen.getByRole('list')).getByText('claude-sonnet-4-5'),
    ).toBeInTheDocument()
    // Totals are still 2 -- the date filter narrows the list, not the tiles.
    expect(screen.getByText('2')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Clear dates' }))
    expect(within(screen.getByRole('list')).getAllByRole('button')).toHaveLength(2)
  })

  describe('order', () => {
    const asc = timeline([MCP_EVENT, LLM_EVENT])
    const desc = timeline([LLM_EVENT, MCP_EVENT])
    const planes = () =>
      within(screen.getByRole('list'))
        .getAllByText(/^(LLM|MCP)$/)
        .map((el) => el.textContent)
    const timelineCalls = (fetchMock: ReturnType<typeof renderTimeline>) =>
      fetchMock.mock.calls
        .map(([req]) => String(req))
        .filter((u) => u.includes('/analytics/sessions/s-1/timeline'))

    it('defaults to oldest first: sends order=asc and leaves the URL alone', async () => {
      const fetchMock = renderTimeline(asc)
      await screen.findByRole('list')
      expect(timelineCalls(fetchMock)).toEqual([
        expect.stringContaining('/analytics/sessions/s-1/timeline?order=asc'),
      ])
      expect(screen.getByRole('button', { name: 'Oldest first' })).toHaveAttribute('aria-pressed', 'true')
      expect(screen.getByRole('button', { name: 'Newest first' })).toHaveAttribute('aria-pressed', 'false')
      expect(screen.getByTestId('search')).toHaveTextContent('?session_id=s-1')
      expect(screen.getByTestId('search')).not.toHaveTextContent('order')
    })

    it('Newest first asks the server for order=desc, puts it in the URL and remembers it', async () => {
      const fetchMock = renderTimeline(asc, true, { byOrder: { asc, desc } })
      const user = userEvent.setup()
      await screen.findByRole('list')
      expect(planes()).toEqual(['MCP', 'LLM'])

      await user.click(screen.getByRole('button', { name: 'Newest first' }))
      await vi.waitFor(() => expect(planes()).toEqual(['LLM', 'MCP']))
      expect(timelineCalls(fetchMock).some((u) => u.includes('order=desc'))).toBe(true)
      expect(screen.getByTestId('search')).toHaveTextContent('order=desc')
      expect(localStorage.getItem('tusk.sessionTimeline.order')).toBe('desc')

      await user.click(screen.getByRole('button', { name: 'Oldest first' }))
      await vi.waitFor(() => expect(planes()).toEqual(['MCP', 'LLM']))
      expect(screen.getByTestId('search')).not.toHaveTextContent('order')
      expect(localStorage.getItem('tusk.sessionTimeline.order')).toBe('asc')
    })

    it('reads ?order=desc from the URL on load', async () => {
      const fetchMock = renderTimeline(desc, true, {
        entry: '/session-timeline?session_id=s-1&order=desc',
      })
      await screen.findByRole('list')
      expect(timelineCalls(fetchMock)).toEqual([expect.stringContaining('order=desc')])
      expect(screen.getByRole('button', { name: 'Newest first' })).toHaveAttribute('aria-pressed', 'true')
    })

    it("falls back to the browser's last choice and reflects it in the URL", async () => {
      localStorage.setItem('tusk.sessionTimeline.order', 'desc')
      const fetchMock = renderTimeline(desc)
      await screen.findByRole('list')
      expect(timelineCalls(fetchMock)).toEqual([expect.stringContaining('order=desc')])
      await vi.waitFor(() => expect(screen.getByTestId('search')).toHaveTextContent('order=desc'))
    })

    it('does not re-sort: renders the server order as given', async () => {
      // Deliberately newest-first for an asc request: the UI must not fix it.
      renderTimeline(desc)
      await screen.findByRole('list')
      expect(planes()).toEqual(['LLM', 'MCP'])
    })
  })
})
