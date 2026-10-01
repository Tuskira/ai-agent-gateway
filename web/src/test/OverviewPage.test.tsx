import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { AuthProvider } from '@/auth/AuthContext'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import type { OverviewMetrics } from '@/lib/overview'
import OverviewPage from '@/routes/OverviewPage'

// jsdom has no Web Animations API; Base UI's ScrollArea (the page's
// scrolling lists) asks its viewport for running animations. Stubbed here
// rather than in setup.ts: with `getAnimations` present, Base UI's dialogs
// wait on it before unmounting, which other suites don't expect.
if (!Element.prototype.getAnimations) {
  Element.prototype.getAnimations = () => []
}

const PRINCIPAL = {
  subject: 'avinash@tuskira.ai',
  tenant_id: 'tuskira-dev-tenant',
  email: 'avinash@tuskira.ai',
  roles: ['admin'],
  auth_method: 'api_key',
  key_id: 'key_123',
}

const HEALTH = { status: 'ok', plane: 'api', version: '0.1.0' }

const KPI_TITLES = [
  'LLM Agent Calls',
  'MCP Tool Calls',
  'Total Tokens',
  'Total Cost',
  'Success rate',
]

const CARD_TITLES = [
  'LLM Usage',
  'Traffic Distribution',
  'Top agents / profiles',
  'Health · status codes',
  'Latency',
  'Requests by client',
  'MCP Tools',
  'Top MCPs',
  'Traffic Over Time',
  'Agent traffic flow',
]

const METRICS: OverviewMetrics = {
  range: '24h',
  kpis: {
    llmAgentCalls: { value: 602, delta: { pct: 7.38, direction: 'down' } },
    mcpToolCalls: { value: 260, delta: { pct: 7.8, direction: 'down' } },
    totalTokens: { value: 515490, delta: { pct: 47.33, direction: 'up' } },
    totalCost: { value: 11, delta: { pct: 1, direction: 'up' } },
    successRate: { value: 97.5, sub: '200 / 204 responses' },
  },
  llmUsage: [{ model: 'claude-sonnet-4-5', calls: 223, tokens: 126540, cost: 9.28 }],
  traffic: { llmCalls: 602, mcpCalls: 260 },
  topAgents: [{ name: 'CTEM Copilot Agent', count: 288 }],
  statusCodes: [
    { label: '200', pct: 96 },
    { label: '204', pct: 1.5 },
    { label: '4xx/5xx', pct: 2.5 },
  ],
  successPct: 97.5,
  latency: {
    medianMs: 487,
    p95Ms: 20255,
    slowest: [{ name: 'mesh__run_query', ms: 24133 }],
  },
  requestsByClient: [{ name: 'Claude Code', count: 642 }],
  mcpTools: [{ name: 'Bash', count: 37 }],
  topConnectors: [{ name: 'mesh', count: 210 }],
  trafficOverTime: [
    { label: '9PM', llmCalls: 560, mcpCalls: 236 },
    { label: '10PM', llmCalls: 528, mcpCalls: 221 },
  ],
}

const FLOW = {
  range: '24h',
  metric: 'calls',
  total: 500,
  agents: [{ key: 'cursor', label: 'Cursor', value: 500 }],
  nodes: [
    { id: 'CLIENT:cursor', key: 'cursor', label: 'Cursor', type: 'CLIENT', value: 500 },
    { id: 'PATH:llm', key: 'llm', label: 'LLM calls', type: 'PATH', value: 400 },
    { id: 'PATH:mcp', key: 'mcp', label: 'MCP calls', type: 'PATH', value: 100 },
    {
      id: 'MODEL:claude-sonnet-5',
      key: 'claude-sonnet-5',
      label: 'claude-sonnet-5',
      type: 'MODEL',
      value: 400,
      sublabel: 'anthropic',
    },
    { id: 'CONNECTOR:mesh', key: 'mesh', label: 'mesh', type: 'CONNECTOR', value: 100 },
  ],
  links: [
    {
      source: 'cursor',
      sourceType: 'CLIENT',
      target: 'llm',
      targetType: 'PATH',
      value: 400,
    },
    {
      source: 'cursor',
      sourceType: 'CLIENT',
      target: 'mcp',
      targetType: 'PATH',
      value: 100,
    },
    {
      source: 'llm',
      sourceType: 'PATH',
      target: 'claude-sonnet-5',
      targetType: 'MODEL',
      value: 400,
    },
    {
      source: 'mcp',
      sourceType: 'PATH',
      target: 'mesh',
      targetType: 'CONNECTOR',
      value: 100,
    },
  ],
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

interface MockOptions {
  metrics?: OverviewMetrics | null
  connectors?: { items: unknown[]; total: number }
  profiles?: { items: unknown[]; total: number }
  connectorTools?: Record<string, number>
  profileTools?: Record<string, number>
  flow?: unknown | null
}

function installFetchMock({
  metrics = null,
  connectors = { items: [], total: 0 },
  profiles = { items: [], total: 0 },
  connectorTools = {},
  profileTools = {},
  flow = null,
}: MockOptions) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()

      if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
      if (url.endsWith('/api/v1/health')) return jsonResponse(HEALTH)
      if (url.includes('/api/v1/analytics/overview')) {
        return metrics
          ? jsonResponse(metrics)
          : jsonResponse({ error: { type: 'not_found', message: 'not found' } }, 404)
      }
      if (url.endsWith('/api/v1/connectors')) return jsonResponse(connectors)
      if (url.endsWith('/api/v1/profiles')) return jsonResponse(profiles)
      if (url.includes('/api/v1/analytics/traffic-flow')) {
        return flow
          ? jsonResponse(flow)
          : jsonResponse({ error: { type: 'not_found', message: 'not found' } }, 404)
      }

      const connMatch = /\/api\/v1\/connectors\/([^/]+)\/tools$/.exec(url)
      if (connMatch) {
        const count = connectorTools[connMatch[1]!] ?? 0
        return jsonResponse({ items: Array.from({ length: count }), total: count })
      }

      const profMatch = /\/api\/v1\/profiles\/([^/]+)\/tools$/.exec(url)
      if (profMatch) {
        const count = profileTools[profMatch[1]!] ?? 0
        return jsonResponse({ items: Array.from({ length: count }), total: count })
      }

      throw new Error(`Unhandled fetch in test: ${url}`)
    }),
  )
}

function renderOverviewPage() {
  sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        <MemoryRouter initialEntries={['/']}>
          <OverviewPage />
        </MemoryRouter>
      </AuthProvider>
    </QueryClientProvider>,
  )
}

describe('OverviewPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('renders every KPI and card title, and the empty caption, when metrics is null', async () => {
    installFetchMock({ metrics: null })
    renderOverviewPage()

    for (const title of KPI_TITLES) {
      expect(await screen.findByText(title)).toBeInTheDocument()
    }
    for (const title of CARD_TITLES) {
      expect(screen.getByText(title)).toBeInTheDocument()
    }

    // Values render as the honest placeholder, never a fabricated number.
    // They replace the loading skeletons once the analytics request settles.
    expect((await screen.findAllByText('—')).length).toBeGreaterThan(0)
    expect(
      (await screen.findAllByText('No data yet · enable the ClickHouse sink')).length,
    ).toBeGreaterThan(0)
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })

  it('puts an info hint beside every KPI and card title', async () => {
    installFetchMock({ metrics: METRICS })
    renderOverviewPage()

    await screen.findByText('515.49K')
    for (const title of [...KPI_TITLES, ...CARD_TITLES]) {
      expect(screen.getByRole('button', { name: `About ${title}` })).toBeInTheDocument()
    }
  })

  it('shows loading skeletons, not placeholder values, while metrics are loading', async () => {
    installFetchMock({ metrics: METRICS })
    renderOverviewPage()

    // Titles are there from the first render; values are not.
    expect(await screen.findByText('LLM Usage')).toBeInTheDocument()
    expect(screen.getAllByRole('status').length).toBeGreaterThan(0)
    expect(screen.queryByText('No data yet · enable the ClickHouse sink')).toBeNull()

    expect(await screen.findByText('515.49K')).toBeInTheDocument()
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })

  it('renders numbers and deltas with the correct up/down class from a mocked metrics payload', async () => {
    installFetchMock({ metrics: METRICS, flow: FLOW })
    renderOverviewPage()

    // Total Tokens KPI value + its green "up" delta.
    expect(await screen.findByText('515.49K')).toBeInTheDocument()
    const upDelta = screen.getByText('▲ 47.33%')
    expect(upDelta).toHaveClass('text-status-resolved')

    // LLM Agent Calls KPI's red "down" delta.
    const downDelta = screen.getByText('▼ 7.38%')
    expect(downDelta).toHaveClass('text-sev-high')

    // Total Cost KPI value.
    expect(screen.getByText('$11.00')).toBeInTheDocument()
    // Success rate KPI value (also mirrored in the Health donut's center
    // label, since both read the same success percentage) + its real sub
    // text (not a delta).
    expect(screen.getAllByText('97.5%').length).toBeGreaterThan(0)
    expect(screen.getByText('200 / 204 responses')).toBeInTheDocument()

    // No empty captions once real metrics are present.
    expect(
      screen.queryByText('No data yet · enable the ClickHouse sink'),
    ).not.toBeInTheDocument()
  })

  it('shows the Total Cost tile\'s "estimate · rate card" subtitle and the LLM Usage "Cost (est.)" header when costEstimated is true or absent', async () => {
    installFetchMock({
      metrics: { ...METRICS, costEstimated: true, pricingSource: 'embedded' },
    })
    renderOverviewPage()

    expect(await screen.findByText('estimate · rate card')).toBeInTheDocument()
    expect(screen.getByText('Cost (est.)')).toBeInTheDocument()
  })

  it('still shows the cost-estimate subtitle when costEstimated/pricingSource are absent (older gateway payload)', async () => {
    // METRICS has no costEstimated/pricingSource fields at all -- the page
    // must tolerate their absence rather than crash, and treat "absent" the
    // same as "true" (every cost figure this page has ever shown has come
    // from the rate card).
    installFetchMock({ metrics: METRICS })
    renderOverviewPage()

    expect(await screen.findByText('estimate · rate card')).toBeInTheDocument()
    expect(screen.getByText('Cost (est.)')).toBeInTheDocument()
  })

  it('hides the cost-estimate subtitle when the payload says costEstimated is false', async () => {
    installFetchMock({ metrics: { ...METRICS, costEstimated: false } })
    renderOverviewPage()

    await screen.findByText('$11.00') // wait for metrics to render
    expect(screen.queryByText('estimate · rate card')).not.toBeInTheDocument()
  })

  it('lists real connectors and profiles from mocked endpoints when metrics is null', async () => {
    installFetchMock({
      metrics: null,
      connectors: {
        items: [
          { id: 'c1', name: 'Alpha Connector', slug: 'alpha', status: 'healthy' },
          { id: 'c2', name: 'Beta Connector', slug: 'beta', status: 'unhealthy' },
        ],
        total: 2,
      },
      profiles: {
        items: [{ id: 'p1', name: 'SOC Agent Profile', slug: 'soc-agent' }],
        total: 1,
      },
      connectorTools: { c1: 3, c2: 1 },
      profileTools: { p1: 5 },
    })
    renderOverviewPage()

    expect(await screen.findByText('Alpha Connector')).toBeInTheDocument()
    expect(screen.getByText('Beta Connector')).toBeInTheDocument()
    expect(screen.getByText('SOC Agent Profile')).toBeInTheDocument()

    await waitFor(() => {
      expect(screen.getByText('3')).toBeInTheDocument()
      expect(screen.getByText('5')).toBeInTheDocument()
    })
  })

  it('renders the Agent traffic flow card from a mocked payload, following the page range and with an agent filter', async () => {
    installFetchMock({ metrics: METRICS, flow: FLOW })
    renderOverviewPage()

    expect(await screen.findByText('Agent traffic flow')).toBeInTheDocument()
    expect(screen.getByText('who called what · last 24h')).toBeInTheDocument()
    expect(screen.getByText('LLM path')).toBeInTheDocument()
    expect(screen.getByText('MCP path')).toBeInTheDocument()
    expect(screen.getByRole('combobox', { name: 'Agent' })).toHaveValue('')
    await waitFor(() => {
      const fetchMock = globalThis.fetch as unknown as { mock: { calls: unknown[][] } }
      expect(
        fetchMock.mock.calls.some(([input]) =>
          String(input).includes('/api/v1/analytics/traffic-flow?range=24h&metric=calls'),
        ),
      ).toBe(true)
    })
  })

  it('shows the traffic flow card\'s "enable ClickHouse" empty state when the endpoint 404s', async () => {
    installFetchMock({ metrics: null, flow: null })
    renderOverviewPage()

    expect(await screen.findByText('Agent traffic flow')).toBeInTheDocument()
    expect(
      (await screen.findAllByText('No data yet · enable the ClickHouse sink')).length,
    ).toBeGreaterThan(0)
  })
})
