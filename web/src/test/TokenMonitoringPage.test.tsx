import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
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
function stubMonitoring(overrides: { overview?: (window: string) => Response } = {}) {
  const urls: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      urls.push(url.replace(/^.*\/api\/v1/, ''))
      const u = new URL(url, 'http://test')
      const window = u.searchParams.get('window') ?? ''
      if (u.pathname.endsWith('/analytics/token-monitoring')) {
        if (overrides.overview) return overrides.overview(window)
        return jsonResponse(
          window === 'today'
            ? monitoring.overview_today.response
            : { ...monitoring.overview_7d.response, window },
        )
      }
      if (u.pathname.endsWith('/analytics/token-monitoring/model'))
        return jsonResponse({ ...monitoring.model.response, window })
      if (u.pathname.includes('/analytics/token-monitoring/keys/'))
        return jsonResponse({ ...monitoring.key.response, window })
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

  it('opens on Today and switching the window updates every widget', async () => {
    const user = userEvent.setup()
    const urls = stubMonitoring()
    renderAt('/token-monitoring')

    // Today (the default).
    expect(await screen.findByTestId('total-tokens')).toHaveTextContent('1.50K')
    expect(urls[0]).toBe('/analytics/token-monitoring?window=today')
    expect(screen.getByRole('button', { name: 'Today' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(screen.queryByText('gpt-5')).not.toBeInTheDocument()

    // Last 7 Days: a new request, and totals, change, cards and callers all follow.
    await user.click(screen.getByRole('button', { name: 'Last 7 Days' }))
    await waitFor(() =>
      expect(screen.getByTestId('total-tokens')).toHaveTextContent('339.01K'),
    )
    expect(urls).toContain('/analytics/token-monitoring?window=7d')
    expect(currentLocation()).toBe('/token-monitoring?window=7d')
    expect(screen.getByRole('button', { name: 'Last 7 Days' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(screen.getByTestId('total-delta')).toHaveTextContent('69.51%')
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

    // Last 30 Days asks again.
    await user.click(screen.getByRole('button', { name: 'Last 30 Days' }))
    await waitFor(() => expect(urls).toContain('/analytics/token-monitoring?window=30d'))
  })

  it('keeps the window when drilling into a model or a caller', async () => {
    const user = userEvent.setup()
    stubMonitoring()
    renderAt('/token-monitoring?window=7d')
    await screen.findByTestId('total-tokens')
    const cards = screen.getByRole('region', { name: 'Cost by model' })
    await user.click(within(cards).getByRole('button', { name: /claude-sonnet-4-5/ }))
    expect(currentLocation()).toBe(
      '/token-monitoring/model?model=claude-sonnet-4-5&window=7d',
    )
  })

  it('opens a caller from the callers table', async () => {
    const user = userEvent.setup()
    stubMonitoring()
    renderAt('/token-monitoring?window=7d')
    await screen.findByTestId('total-tokens')
    const callers = screen.getByRole('region', { name: 'Callers' })
    await user.click(within(callers).getByText('ci-bot'))
    expect(currentLocation()).toBe(`/token-monitoring/keys/${agentKeyId}?window=7d`)
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
    const base = monitoring.overview_today.response
    const many = {
      ...base,
      by_model: Array.from({ length: 7 }, (_, i) => ({
        ...base.by_model[0]!,
        model: `model-${i}`,
      })),
    }
    stubMonitoring({ overview: () => jsonResponse(many) })
    renderAt('/token-monitoring?window=7d')
    const region = await screen.findByRole('region', { name: 'Cost by model' })
    const cards = (within_: HTMLElement) =>
      within(within_).queryAllByRole('button', { name: /^Open / })
    await waitFor(() => expect(cards(region)).toHaveLength(6))
    expect(within(region).getByText('7 models')).toBeInTheDocument()

    // Every model is in the dialog (a modal: the page behind it is inert).
    await user.click(within(region).getByRole('button', { name: 'Show all 7 models' }))
    const dialog = await screen.findByRole('dialog', { name: 'Cost by model' })
    expect(cards(dialog)).toHaveLength(7)

    // A card in the dialog opens that model, keeping the window.
    await user.click(within(dialog).getByRole('button', { name: 'Open model-6' }))
    await waitFor(() =>
      expect(currentLocation()).toBe('/token-monitoring/model?model=model-6&window=7d'),
    )
  })

  it('orders the model cost cards by cost, unpriced models last', async () => {
    const base = monitoring.overview_today.response
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
      ...monitoring.overview_today.response,
      totals: {
        ...monitoring.overview_today.response.totals,
        tokens: 0,
        prompt_tokens: 0,
        completion_tokens: 0,
        cost_usd: null,
        calls: 0,
      },
      by_model: [],
      by_key: [],
      by_role: monitoring.overview_today.response.by_role!.map((r) => ({
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
    renderAt('/token-monitoring/model?model=claude-sonnet-4-5&window=7d')
    expect(
      await screen.findByRole('heading', { name: 'claude-sonnet-4-5' }),
    ).toBeInTheDocument()
    await screen.findByTestId('total-tokens')
    expect(urls[0]).toBe(
      '/analytics/token-monitoring/model?model=claude-sonnet-4-5&window=7d&limit=25&offset=0',
    )
    expect(screen.getByTestId('total-tokens')).toHaveTextContent('315.00K')
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

  it('shows one caller by model, and its window can change', async () => {
    const user = userEvent.setup()
    const urls = stubMonitoring()
    renderAt(`/token-monitoring/keys/${keyId}?window=7d`)
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

    await user.click(screen.getByRole('button', { name: 'Today' }))
    await waitFor(() =>
      expect(urls).toContain(
        `/analytics/token-monitoring/keys/${keyId}?window=today&limit=25&offset=0`,
      ),
    )
    expect(currentLocation()).toBe(`/token-monitoring/keys/${keyId}?window=today`)
  })
})
