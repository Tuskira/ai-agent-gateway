import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import TokenMonitoringPage from '@/routes/TokenMonitoringPage'
import TokenMonitoringDetailPage from '@/routes/TokenMonitoringDetailPage'
import { monitoring } from './fixtures/token-monitoring'

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** Answers every monitoring request from the contract fixture and records
 * the URLs asked for. */
function stubMonitoring(overrides: { overview?: (range: string) => Response } = {}) {
  const urls: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      urls.push(url.replace(/^.*\/api\/v1/, ''))
      const u = new URL(url, 'http://test')
      const range = u.searchParams.get('range') ?? ''
      if (u.pathname.endsWith('/analytics/token-monitoring')) {
        if (overrides.overview) return overrides.overview(range)
        return jsonResponse(
          range === '24h'
            ? monitoring.overview_24h.response
            : { ...monitoring.overview_7d.response, range },
        )
      }
      if (u.pathname.endsWith('/analytics/token-monitoring/model'))
        return jsonResponse({ ...monitoring.model.response, range })
      if (u.pathname.includes('/analytics/token-monitoring/keys/'))
        return jsonResponse({ ...monitoring.key.response, range })
      throw new Error(`Unhandled fetch in test: ${url}`)
    }),
  )
  return urls
}

function LocationProbe() {
  const loc = useLocation()
  return <div data-testid="location">{loc.pathname + loc.search}</div>
}

const currentLocation = () => screen.getByTestId('location').textContent

function renderAt(path: string) {
  sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <LocationProbe />
        <Routes>
          <Route path="/token-monitoring" element={<TokenMonitoringPage />} />
          <Route
            path="/token-monitoring/model"
            element={<TokenMonitoringDetailPage kind="model" />}
          />
          <Route
            path="/token-monitoring/keys/:id"
            element={<TokenMonitoringDetailPage kind="key" />}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

const keyId = monitoring.key.response.key!.id
const agentKeyId = monitoring.overview_7d.response.by_key[0]!.key_id

describe('TokenMonitoringPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
    // ScrollArea (ranked lists) animates in jsdom.
    Element.prototype.getAnimations = () => []
  })
  afterEach(() => vi.unstubAllGlobals())

  it('opens on Last 24h and switching the range updates every widget', async () => {
    const user = userEvent.setup()
    const urls = stubMonitoring()
    renderAt('/token-monitoring')

    // Last 24h (the default).
    expect(await screen.findByTestId('total-tokens')).toHaveTextContent('1.50K')
    expect(urls[0]).toBe('/analytics/token-monitoring?range=24h')
    expect(screen.getByRole('button', { name: 'Last 24h' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(screen.queryByText('gpt-5')).not.toBeInTheDocument()

    // Last 7d: a new request, and totals, change, cards and callers all follow.
    await user.click(screen.getByRole('button', { name: 'Last 7d' }))
    await waitFor(() =>
      expect(screen.getByTestId('total-tokens')).toHaveTextContent('2.95M'),
    )
    expect(urls).toContain('/analytics/token-monitoring?range=7d')
    expect(currentLocation()).toBe('/token-monitoring?range=7d')
    expect(screen.getByRole('button', { name: 'Last 7d' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    // The headline counts cache too: 339,012 + 2,430,000 + 180,000, +47.45% on 2.00M.
    expect(screen.getByTestId('total-delta')).toHaveTextContent('47.45%')
    expect(screen.getByTestId('total-cost-value')).toHaveTextContent('$4.12')
    expect(screen.getByTestId('total-cost')).toHaveTextContent(
      'Estimate · 6 unpriced calls',
    )

    const cards = screen.getByRole('region', { name: 'Cost by model' })
    expect(within(cards).getByText('gpt-5')).toBeInTheDocument()
    expect(within(cards).getByText('$0.0000097')).toBeInTheDocument() // a tiny cost, not "$0.00"
    expect(within(cards).getAllByText('<0.1%').length).toBeGreaterThan(0)

    const callers = screen.getByRole('region', { name: 'Callers' })
    expect(within(callers).getByText('ci-bot')).toBeInTheDocument()
    expect(within(callers).getByText('collector')).toBeInTheDocument()
    expect(within(callers).getByText('Unknown key')).toBeInTheDocument()
    const roles = screen.getByRole('region', { name: 'By caller role' })
    expect(within(roles).getByText('Interceptor')).toBeInTheDocument()

    // Last 30d asks again.
    await user.click(screen.getByRole('button', { name: 'Last 30d' }))
    await waitFor(() => expect(urls).toContain('/analytics/token-monitoring?range=30d'))
  })

  it('keeps the range when drilling into a model or a caller', async () => {
    const user = userEvent.setup()
    stubMonitoring()
    renderAt('/token-monitoring?range=7d')
    await screen.findByTestId('total-tokens')
    const cards = screen.getByRole('region', { name: 'Cost by model' })
    await user.click(within(cards).getByRole('button', { name: /claude-sonnet-4-5/ }))
    expect(currentLocation()).toBe(
      '/token-monitoring/model?model=claude-sonnet-4-5&range=7d',
    )
  })

  it('keeps showing the current numbers while a new range loads (no flash)', async () => {
    const user = userEvent.setup()
    let release: () => void = () => {}
    const slow = new Promise<void>((r) => (release = r))
    stubMonitoring({
      overview: (range) =>
        range === '7d'
          ? (slow.then(() =>
              jsonResponse({ ...monitoring.overview_7d.response, range }),
            ) as unknown as Response)
          : jsonResponse(monitoring.overview_24h.response),
    })
    renderAt('/token-monitoring')
    expect(await screen.findByTestId('total-tokens')).toHaveTextContent('1.50K')
    await user.click(screen.getByRole('button', { name: 'Last 7d' }))
    // While 7d is in flight the tiles keep the 24h figures instead of
    // dropping back to loading placeholders.
    expect(screen.getByTestId('total-tokens')).toHaveTextContent('1.50K')
    release()
    await waitFor(() =>
      expect(screen.getByTestId('total-tokens')).toHaveTextContent('2.95M'),
    )
  })

  it('explains every widget with a help icon', async () => {
    stubMonitoring()
    renderAt('/token-monitoring')
    await screen.findByTestId('total-tokens')
    for (const title of [
      'Total tokens',
      'Input / Output',
      'Cache read / write',
      'Cost',
      'Calls',
      'By caller role',
      'By model',
      'By caller',
      'Usage over time (UTC)',
      'Cost by model',
      'Callers',
    ]) {
      expect(screen.getByRole('button', { name: `About ${title}` })).toBeInTheDocument()
    }
  })

  it('reads custom dates from the URL and keeps them on a drill-down', async () => {
    const user = userEvent.setup()
    const urls = stubMonitoring()
    renderAt('/token-monitoring?from=2026-01-01&to=2026-01-03')
    await screen.findByTestId('total-tokens')
    expect(urls[0]).toBe('/analytics/token-monitoring?from=2026-01-01&to=2026-01-03')
    expect(screen.getByRole('button', { name: /custom dates/i })).toHaveTextContent(
      'Jan 1 – Jan 3, 2026',
    )
    const cards = screen.getByRole('region', { name: 'Cost by model' })
    await user.click(within(cards).getByRole('button', { name: /claude-sonnet-4-5/ }))
    expect(currentLocation()).toBe(
      '/token-monitoring/model?model=claude-sonnet-4-5&from=2026-01-01&to=2026-01-03',
    )
  })

  it('opens a caller from the callers table', async () => {
    const user = userEvent.setup()
    stubMonitoring()
    renderAt('/token-monitoring?range=7d')
    await screen.findByTestId('total-tokens')
    const callers = screen.getByRole('region', { name: 'Callers' })
    await user.click(within(callers).getByText('ci-bot'))
    expect(currentLocation()).toBe(`/token-monitoring/keys/${agentKeyId}?range=7d`)
  })

  it('explains when analytics are off', async () => {
    stubMonitoring({
      overview: () =>
        jsonResponse(
          {
            error: {
              type: 'not_found',
              message: 'analytics requires the ClickHouse sink',
            },
          },
          404,
        ),
    })
    renderAt('/token-monitoring')
    expect(await screen.findByText('Analytics are turned off')).toBeInTheDocument()
  })

  it('shows the top six model cost cards and the rest in a scrollable dialog', async () => {
    const user = userEvent.setup()
    const base = monitoring.overview_24h.response
    const many = {
      ...base,
      by_model: Array.from({ length: 7 }, (_, i) => ({
        ...base.by_model[0]!,
        model: `model-${i}`,
      })),
    }
    stubMonitoring({ overview: () => jsonResponse(many) })
    renderAt('/token-monitoring?range=7d')
    const region = await screen.findByRole('region', { name: 'Cost by model' })
    const cards = (within_: HTMLElement) =>
      within(within_).queryAllByRole('button', { name: /^Open / })
    await waitFor(() => expect(cards(region)).toHaveLength(6))
    expect(within(region).getByText('7 models')).toBeInTheDocument()

    // Every model is in the dialog (a modal: the page behind it is inert).
    await user.click(within(region).getByRole('button', { name: 'Show all 7 models' }))
    const dialog = await screen.findByRole('dialog', { name: 'Cost by model' })
    expect(cards(dialog)).toHaveLength(7)

    // A card in the dialog opens that model, keeping the range.
    await user.click(within(dialog).getByRole('button', { name: 'Open model-6' }))
    await waitFor(() =>
      expect(currentLocation()).toBe('/token-monitoring/model?model=model-6&range=7d'),
    )
  })

  it('shows a pointer on cost cards and keeps the callers table to its own scroll', async () => {
    stubMonitoring()
    renderAt('/token-monitoring?range=7d')
    const region = await screen.findByRole('region', { name: 'Cost by model' })
    const card = (await within(region).findAllByRole('button', { name: /^Open / }))[0]!
    expect(card).toHaveClass('cursor-pointer')
    const callers = screen.getByRole('region', { name: 'Callers' })
    const scroller = within(callers).getByRole('region', { name: /table/i })
    expect(scroller.style.maxHeight).not.toBe('')
  })

  it('fills two complete rows of cost cards for however many columns fit', async () => {
    // The browser reports the grid's resolved columns and notifies on resize;
    // jsdom does neither, so both are simulated here.
    let template = '300px 300px 300px 300px'
    const resized: (() => void)[] = []
    vi.stubGlobal(
      'ResizeObserver',
      class {
        cb: () => void
        constructor(cb: () => void) {
          this.cb = cb
        }
        observe(el: Element) {
          // Only the cost grid: other observers (the chart) expect real entries.
          if (el.closest('section[aria-label="Cost by model"]')) resized.push(this.cb)
        }
        disconnect() {}
      },
    )
    const realStyle = window.getComputedStyle.bind(window)
    vi.spyOn(window, 'getComputedStyle').mockImplementation((el, pseudo) => {
      const style = realStyle(el, pseudo)
      if (!(el as HTMLElement).closest?.('section[aria-label="Cost by model"]'))
        return style
      return new Proxy(style, {
        get: (t, p) => {
          if (p === 'gridTemplateColumns') return template
          const v = Reflect.get(t, p, t)
          return typeof v === 'function' ? v.bind(t) : v
        },
      })
    })
    const base = monitoring.overview_24h.response
    stubMonitoring({
      overview: () =>
        jsonResponse({
          ...base,
          by_model: Array.from({ length: 11 }, (_, i) => ({
            ...base.by_model[0]!,
            model: `m-${i}`,
          })),
        }),
    })
    renderAt('/token-monitoring')
    const region = await screen.findByRole('region', { name: 'Cost by model' })
    const cards = () => within(region).queryAllByRole('button', { name: /^Open / })
    await waitFor(() => expect(cards()).toHaveLength(8)) // 4 columns x 2 rows
    template = '250px 250px 250px 250px 250px'
    act(() => resized.forEach((cb) => cb()))
    await waitFor(() => expect(cards()).toHaveLength(10)) // 5 columns
    template = '300px 300px 300px'
    act(() => resized.forEach((cb) => cb()))
    await waitFor(() => expect(cards()).toHaveLength(6)) // 3 columns
    expect(
      within(region).getByRole('button', { name: 'Show all 11 models' }),
    ).toBeInTheDocument()
  })

  it('switches Cost by model between cards and a sortable list of every model', async () => {
    const user = userEvent.setup()
    const base = monitoring.overview_24h.response
    stubMonitoring({
      overview: () =>
        jsonResponse({
          ...base,
          by_model: Array.from({ length: 7 }, (_, i) => ({
            ...base.by_model[0]!,
            model: `model-${i}`,
            cost_usd: i,
          })),
        }),
    })
    renderAt('/token-monitoring?range=7d')
    const region = await screen.findByRole('region', { name: 'Cost by model' })
    await waitFor(() =>
      expect(within(region).getAllByRole('button', { name: /^Open / })).toHaveLength(6),
    )

    await user.click(within(region).getByRole('button', { name: 'List' }))
    const table = within(region).getByRole('table')
    const rows = () => within(table).getAllByRole('row').slice(1) // minus the header row
    expect(rows()).toHaveLength(7) // every model, not the top two rows of cards
    expect(rows()[0]).toHaveTextContent('model-6') // most expensive first
    expect(
      within(region).queryByRole('button', { name: /^Show all/ }),
    ).not.toBeInTheDocument()

    await user.click(rows()[0]!)
    await waitFor(() =>
      expect(currentLocation()).toBe('/token-monitoring/model?model=model-6&range=7d'),
    )
  })

  it('searches the all-models dialog by name, in cards and in the list', async () => {
    const user = userEvent.setup()
    const base = monitoring.overview_24h.response
    stubMonitoring({
      overview: () =>
        jsonResponse({
          ...base,
          by_model: [
            ...Array.from({ length: 6 }, (_, i) => ({
              ...base.by_model[0]!,
              model: `claude-${i}`,
            })),
            { ...base.by_model[0]!, model: 'gpt-4o-mini' },
          ],
        }),
    })
    renderAt('/token-monitoring')
    const region = await screen.findByRole('region', { name: 'Cost by model' })
    await user.click(
      await within(region).findByRole('button', { name: 'Show all 7 models' }),
    )
    const dialog = await screen.findByRole('dialog', { name: 'Cost by model' })

    await user.type(
      within(dialog).getByRole('searchbox', { name: 'Search models' }),
      'GPT',
    )
    expect(
      within(dialog)
        .getAllByRole('button', { name: /^Open / })
        .map((b) => b.getAttribute('aria-label')),
    ).toEqual(['Open gpt-4o-mini'])
    expect(within(dialog).getByText('1 of 7 models')).toBeInTheDocument()

    await user.click(within(dialog).getByRole('button', { name: 'List' }))
    const rows = within(within(dialog).getByRole('table')).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(1)
    expect(rows[0]).toHaveTextContent('gpt-4o-mini')
  })

  it('orders the model cost cards by cost, unpriced models last', async () => {
    const base = monitoring.overview_24h.response
    const model = (name: string, tokens: number, cost_usd: number | null) => ({
      ...base.by_model[0]!,
      model: name,
      tokens,
      cost_usd,
    })
    stubMonitoring({
      overview: () =>
        jsonResponse({
          ...base,
          // The API sends by_model sorted by tokens.
          by_model: [
            model('cheap-big', 900, 0.1),
            model('unpriced', 800, null),
            model('pricey-small', 10, 5.91),
            model('mid', 5, 0.5),
          ],
        }),
    })
    renderAt('/token-monitoring')
    const region = await screen.findByRole('region', { name: 'Cost by model' })
    await waitFor(() =>
      expect(
        within(region)
          .getAllByRole('button', { name: /^Open / })
          .map((b) => b.getAttribute('aria-label')),
      ).toEqual(['Open pricey-small', 'Open mid', 'Open cheap-big', 'Open unpriced']),
    )
  })

  it('shows empty states when there was no usage', async () => {
    const empty = {
      ...monitoring.overview_24h.response,
      totals: {
        ...monitoring.overview_24h.response.totals,
        tokens: 0,
        prompt_tokens: 0,
        completion_tokens: 0,
        cost_usd: null,
        calls: 0,
      },
      by_model: [],
      by_key: [],
      by_role: monitoring.overview_24h.response.by_role!.map((r) => ({
        ...r,
        tokens: 0,
        calls: 0,
      })),
    }
    stubMonitoring({ overview: () => jsonResponse(empty) })
    renderAt('/token-monitoring')
    expect(await screen.findByTestId('total-tokens')).toHaveTextContent('0')
    // Every widget says so, rather than showing zeros as if they were data.
    expect(
      within(screen.getByRole('region', { name: 'Cost by model' })).getByText(
        'No LLM usage in this window yet',
      ),
    ).toBeInTheDocument()
    expect(
      within(screen.getByRole('region', { name: 'By caller role' })).getByText(
        'No LLM usage in this window yet',
      ),
    ).toBeInTheDocument()
    expect(
      screen.getAllByText('No LLM usage in this window yet').length,
    ).toBeGreaterThanOrEqual(4)
  })
})

describe('TokenMonitoringDetailPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
    Element.prototype.getAnimations = () => []
  })
  afterEach(() => vi.unstubAllGlobals())

  it('shows one model by caller and by session', async () => {
    const urls = stubMonitoring()
    renderAt('/token-monitoring/model?model=claude-sonnet-4-5&range=7d')
    expect(
      await screen.findByRole('heading', { name: 'claude-sonnet-4-5' }),
    ).toBeInTheDocument()
    await screen.findByTestId('total-tokens')
    expect(urls[0]).toBe(
      '/analytics/token-monitoring/model?model=claude-sonnet-4-5&range=7d&limit=25&offset=0',
    )
    expect(screen.getByTestId('total-tokens')).toHaveTextContent('2.90M')
    const sessions = screen.getByRole('region', { name: 'Sessions' })
    expect(within(sessions).getByRole('link', { name: 'sess-1' })).toHaveAttribute(
      'href',
      '/session-timeline?session_id=sess-1',
    )
    expect(within(sessions).getByText('No session')).toBeInTheDocument()
    expect(
      within(screen.getByRole('region', { name: 'By caller' })).getByText('ops'),
    ).toBeInTheDocument()
  })

  it('shows one caller by model, and its range can change', async () => {
    const user = userEvent.setup()
    const urls = stubMonitoring()
    renderAt(`/token-monitoring/keys/${keyId}?range=7d`)
    expect(await screen.findByRole('heading', { name: 'collector' })).toBeInTheDocument()
    await screen.findByTestId('total-tokens')
    expect(screen.getByText('interceptor')).toBeInTheDocument()
    expect(
      within(screen.getByRole('region', { name: 'By model' })).getByText('gpt-5'),
    ).toBeInTheDocument()
    expect(
      within(screen.getByRole('region', { name: 'Sessions' })).getByRole('link', {
        name: 'chat-77',
      }),
    ).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Last 24h' }))
    await waitFor(() =>
      expect(urls).toContain(
        `/analytics/token-monitoring/keys/${keyId}?range=24h&limit=25&offset=0`,
      ),
    )
    expect(currentLocation()).toBe(`/token-monitoring/keys/${keyId}?range=24h`)
  })
})
