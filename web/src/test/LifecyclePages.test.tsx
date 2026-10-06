import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import ConnectorsPage from '@/routes/ConnectorsPage'
import SkillsPage from '@/routes/SkillsPage'

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function renderPage(ui: React.ReactElement, path = '/') {
  sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>{ui}</MemoryRouter>
    </QueryClientProvider>,
  )
}

const skill = (name: string, enabled = true) => ({
  id: `id-${name}`,
  name,
  kind: 'skill',
  description: '',
  frontmatter: {},
  arguments: [],
  latest_version: 1,
  enabled,
  metadata: {},
  scope: 'tenant',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
})

function stubSkills(opts: { usage: Response | null; skills: unknown[] }) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.includes('/analytics/skills/usage')) return opts.usage ?? json({}, 404)
      if (url.includes('/analytics/skills')) return json({}, 404)
      if (url.match(/\/api\/v1\/skills(\?.*)?$/))
        return json({ items: opts.skills, total: opts.skills.length })
      throw new Error(`Unhandled fetch in test: ${url}`)
    }),
  )
}

describe('Skills page lifecycle states', () => {
  beforeEach(() => sessionStorage.clear())
  afterEach(() => vi.unstubAllGlobals())

  it('shows Active / Registered / Disabled / Discovered and prefills Register', async () => {
    stubSkills({
      skills: [skill('review-pr'), skill('fresh'), skill('old-skill', false)],
      usage: json({
        range: '7d',
        skills: [
          { name: 'review-pr', calls: 3, used_by: 1, last_seen: '2026-02-01T00:00:00Z', registered: true, kind: 'skill' },
          { name: 'old-skill', calls: 6, used_by: 1, last_seen: '2026-02-01T00:00:00Z', registered: true, kind: 'skill' },
          { name: 'code-audit', calls: 2, used_by: 1, last_seen: '2026-02-01T00:00:00Z', registered: false, kind: '' },
        ],
      }),
    })
    const user = userEvent.setup()
    renderPage(<SkillsPage />)

    const table = await screen.findByRole('table')
    const rowOf = async (name: string) => (await within(table).findByText(name)).closest('tr')!
    expect(await rowOf('review-pr')).toHaveTextContent('Active')
    expect(await rowOf('fresh')).toHaveTextContent('Registered')
    // Disabled wins even though the model still used it.
    const old = await rowOf('old-skill')
    expect(old).toHaveTextContent('Disabled')
    expect(old).not.toHaveTextContent('Active')
    const discovered = await rowOf('code-audit')
    expect(discovered).toHaveTextContent('Discovered')
    expect(discovered).toHaveTextContent('2')

    await user.click(within(discovered).getByRole('button', { name: 'Register code-audit' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('Add skill or command')).toBeInTheDocument()
    expect(within(dialog).getByDisplayValue('code-audit')).toBeInTheDocument()
  })

  it('filters by state', async () => {
    stubSkills({
      skills: [skill('review-pr'), skill('fresh')],
      usage: json({
        range: '7d',
        skills: [
          { name: 'review-pr', calls: 3, used_by: 1, last_seen: '2026-02-01T00:00:00Z', registered: true, kind: 'skill' },
          { name: 'code-audit', calls: 2, used_by: 1, last_seen: '2026-02-01T00:00:00Z', registered: false, kind: '' },
        ],
      }),
    })
    const user = userEvent.setup()
    renderPage(<SkillsPage />)
    await within(await screen.findByRole('table')).findByText('code-audit')

    await user.selectOptions(screen.getByLabelText('Filter by state'), 'discovered')
    const table = screen.getByRole('table')
    expect(within(table).getByText('code-audit')).toBeInTheDocument()
    expect(within(table).queryByText('review-pr')).not.toBeInTheDocument()
  })

  it('analytics off: Registered/Disabled only, Discovered hidden, with the off note', async () => {
    stubSkills({ skills: [skill('a'), skill('b', false)], usage: null })
    renderPage(<SkillsPage />)

    const table = await screen.findByRole('table')
    expect((await within(table).findByText('a')).closest('tr')).toHaveTextContent('Registered')
    expect(within(table).getByText('b').closest('tr')).toHaveTextContent('Disabled')
    expect(await screen.findByTestId('analytics-off-note')).toBeInTheDocument()
    expect(screen.getByLabelText('Filter by state')).not.toHaveTextContent('Discovered')
  })
})

const conn = (slug: string, extra: Record<string, unknown> = {}, metadata: Record<string, unknown> = {}) => ({
  id: `c-${slug}`,
  name: slug,
  slug,
  endpoint: `https://${slug}.example/mcp`,
  timeout_ms: 30000,
  status: 'healthy',
  metadata,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  ...extra,
})

const catalogEntry = (slug: string, name: string, added = false) => ({
  id: `cat-${slug}`,
  slug,
  name,
  description: `${name} docs`,
  icon: 'library',
  category: 'docs',
  url: `https://${slug}.example/mcp`,
  url_overridable: false,
  transport: 'streamable-http',
  auth: { kind: 'none', fields: [] },
  suggested_tools: [],
  enabled: true,
  scope: 'platform',
  supported: true,
  added,
  connector_id: added ? `c-${slug}` : undefined,
})

function stubMcps(opts: { usage: unknown[] | null }) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.includes('/analytics/mcps/usage'))
        return opts.usage ? json({ range: '7d', servers: opts.usage }) : json({}, 404)
      if (url.endsWith('/api/v1/mcp-catalog'))
        return json({
          items: [
            catalogEntry('context7', 'Context7'),
            catalogEntry('deepwiki', 'DeepWiki', true),
            catalogEntry('stripe', 'Stripe'),
          ],
          total: 3,
        })
      if (url.endsWith('/api/v1/connectors'))
        return json({
          items: [conn('deepwiki'), conn('drawio'), conn('idle'), conn('off', {}, { enabled: false })],
          total: 4,
        })
      if (/\/api\/v1\/connectors\/[^/]+\/tools$/.test(url)) return json({ items: [], total: 0 })
      throw new Error(`Unhandled fetch in test: ${url}`)
    }),
  )
}

const usageRow = (server: string, extra: Record<string, unknown> = {}) => ({
  server,
  tools: ['t'],
  calls: 2,
  used_by: 1,
  last_seen: '2026-02-01T00:00:00Z',
  registered_connector_slug: '',
  via_gateway: false,
  ...extra,
})

describe('MCPs page lifecycle states', () => {
  beforeEach(() => sessionStorage.clear())
  afterEach(() => vi.unstubAllGlobals())

  const usage = [
    usageRow('deepwiki', { registered_connector_slug: 'deepwiki' }),
    usageRow('gw', { registered_connector_slug: 'drawio', via_gateway: true }),
    usageRow('off', { registered_connector_slug: 'off', calls: 9 }),
    usageRow('context7'),
    usageRow('homegrown'),
  ]

  it('derives the State column separately from Health', async () => {
    stubMcps({ usage })
    renderPage(<ConnectorsPage />)

    const table = await screen.findByRole('table')
    const rowOf = async (name: string) => (await within(table).findByText(name)).closest('tr')!
    const deepwiki = await rowOf('deepwiki')
    expect(deepwiki).toHaveTextContent('Active')
    expect(deepwiki).toHaveTextContent('healthy') // health stays its own column
    expect(await rowOf('drawio')).toHaveTextContent('Active') // via gateway
    expect(await rowOf('idle')).toHaveTextContent('Registered')
    const off = await rowOf('off')
    expect(off).toHaveTextContent('Disabled')
    expect(off).not.toHaveTextContent('Active')
  })

  it('shows a via-gateway badge only for rows with gateway traffic; mixed shows both', async () => {
    stubMcps({
      usage: [
        usageRow('deepwiki', { registered_connector_slug: 'deepwiki' }),
        usageRow('gw', { registered_connector_slug: 'drawio', via_gateway: true }),
        usageRow('idle', { registered_connector_slug: 'idle' }),
        usageRow('idle', { registered_connector_slug: 'idle', via_gateway: true }),
      ],
    })
    renderPage(<ConnectorsPage />)

    const table = await screen.findByRole('table')
    const rowOf = async (name: string) => (await within(table).findByText(name)).closest('tr')!
    const direct = await rowOf('deepwiki')
    expect(within(direct).queryByTestId('via-gateway-badge')).toBeNull()
    const via = within(await rowOf('drawio')).getByTestId('via-gateway-badge')
    expect(via).toHaveTextContent(/^via gateway$/)
    expect(via).toHaveAttribute('title', "Calls reached this MCP through the gateway's MCP plane")
    expect(within(await rowOf('idle')).getByTestId('via-gateway-badge')).toHaveTextContent(
      'via gateway + direct',
    )
  })

  it('renders the Add from catalog label in full, outside the truncating State cell', async () => {
    stubMcps({ usage })
    renderPage(<ConnectorsPage />)

    const table = await screen.findByRole('table')
    const row = (await within(table).findByText('context7')).closest('tr')!
    const btn = within(row).getByRole('button', { name: /Add from catalog: context7/ })
    expect(btn).toHaveTextContent(/^Add from catalog$/)
    expect(btn.className).not.toMatch(/\btruncate\b|text-ellipsis/)
    expect(btn.className).toContain('whitespace-nowrap')
    const stateCell = within(row).getByText('Discovered').closest('td')!
    expect(stateCell).not.toContainElement(btn)
  })

  it('Discovered server matching a catalog entry offers "Add from catalog"', async () => {
    stubMcps({ usage })
    const user = userEvent.setup()
    renderPage(<ConnectorsPage />)

    const table = await screen.findByRole('table')
    const row = (await within(table).findByText('context7')).closest('tr')!
    expect(row).toHaveTextContent('Discovered')
    await user.click(within(row).getByRole('button', { name: /Add from catalog: context7/ }))
    expect(await screen.findByText('Add Context7')).toBeInTheDocument()
  })

  it('Discovered server without a catalog entry offers "Add MCP" with the name prefilled', async () => {
    stubMcps({ usage })
    const user = userEvent.setup()
    renderPage(<ConnectorsPage />)

    const table = await screen.findByRole('table')
    const row = (await within(table).findByText('homegrown')).closest('tr')!
    await user.click(within(row).getByRole('button', { name: /Add MCP: homegrown/ }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getAllByDisplayValue('homegrown').length).toBeGreaterThanOrEqual(2) // name + slug
  })

  it('Catalog tab shows Available for entries not added and the connector state for added ones', async () => {
    stubMcps({ usage })
    const user = userEvent.setup()
    renderPage(<ConnectorsPage />)
    await within(await screen.findByRole('table')).findByText('deepwiki')

    await user.click(screen.getByRole('tab', { name: 'Catalog' }))
    const stripe = await screen.findByTestId('catalog-card-stripe')
    expect(stripe).toHaveTextContent('Available')
    expect(screen.getByTestId('catalog-card-context7')).toHaveTextContent('Available')
    expect(screen.getByTestId('catalog-card-deepwiki')).toHaveTextContent('Active')
  })

  it('analytics off: Registered/Disabled only, Discovered hidden, with the off note', async () => {
    stubMcps({ usage: null })
    renderPage(<ConnectorsPage />)

    const table = await screen.findByRole('table')
    expect((await within(table).findByText('idle')).closest('tr')).toHaveTextContent('Registered')
    expect(within(table).getByText('off').closest('tr')).toHaveTextContent('Disabled')
    expect(within(table).queryByText('context7')).not.toBeInTheDocument()
    expect(await screen.findByTestId('analytics-off-note')).toBeInTheDocument()
  })
})

describe('refresh after adding a discovered item', () => {
  beforeEach(() => sessionStorage.clear())
  afterEach(() => vi.unstubAllGlobals())

  function stubStatefulMcps() {
    let added: string | null = null
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input)
        const method = init?.method ?? 'GET'
        if (url.includes('/analytics/mcps/usage'))
          return json({
            range: '7d',
            servers: [
              usageRow('context7', added === 'context7' ? { registered_connector_slug: 'context7' } : {}),
              usageRow('homegrown', added === 'homegrown' ? { registered_connector_slug: 'homegrown' } : {}),
            ],
          })
        if (url.endsWith('/api/v1/mcp-catalog/context7/add') && method === 'POST') {
          added = 'context7'
          return json({ connector: conn('context7'), discovery: { status: 'healthy', tools_discovered: 1 } }, 201)
        }
        if (url.endsWith('/api/v1/mcp-catalog'))
          return json({
            items: [catalogEntry('context7', 'Context7', added === 'context7')],
            total: 1,
          })
        if (url.endsWith('/api/v1/connectors') && method === 'POST') {
          added = 'homegrown'
          return json(conn('homegrown'), 201)
        }
        if (url.endsWith('/api/v1/connectors'))
          return json({ items: added ? [conn(added)] : [], total: added ? 1 : 0 })
        if (/\/api\/v1\/connectors\/[^/]+\/tools$/.test(url)) return json({ items: [], total: 0 })
        throw new Error(`Unhandled fetch in test: ${method} ${url}`)
      }),
    )
  }

  it('Add from catalog: Discovered row flips to a connector row without reload', async () => {
    stubStatefulMcps()
    const user = userEvent.setup()
    renderPage(<ConnectorsPage />)

    const table = await screen.findByRole('table')
    const row = (await within(table).findByText('context7')).closest('tr')!
    expect(row).toHaveTextContent('Discovered')
    await user.click(await within(row).findByRole('button', { name: /Add from catalog: context7/ }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Add to tenant' }))

    await vi.waitFor(() => {
      const r = within(screen.getByRole('table')).getByText('context7').closest('tr')!
      expect(r).not.toHaveTextContent('Discovered')
      expect(r).toHaveTextContent('Active')
    })
  })

  it('Add MCP: Discovered row flips to a connector row without reload', async () => {
    stubStatefulMcps()
    const user = userEvent.setup()
    renderPage(<ConnectorsPage />)

    const table = await screen.findByRole('table')
    const row = (await within(table).findByText('homegrown')).closest('tr')!
    expect(row).toHaveTextContent('Discovered')
    await user.click(within(row).getByRole('button', { name: /Add MCP: homegrown/ }))
    const dialog = await screen.findByRole('dialog')
    await user.type(
      within(dialog).getByPlaceholderText(/http:\/\/mcp-server:8001/i),
      'http://homegrown:8001/mcp',
    )
    await user.click(within(dialog).getByRole('button', { name: /^create$/i }))

    await vi.waitFor(() => {
      const r = within(screen.getByRole('table')).getByText('homegrown').closest('tr')!
      expect(r).not.toHaveTextContent('Discovered')
      expect(r).toHaveTextContent('Active')
    })
  })

  it('Skills Register: Discovered row flips to Active without reload', async () => {
    let created = false
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input)
        const method = init?.method ?? 'GET'
        if (url.includes('/analytics/skills/usage'))
          return json({
            range: '7d',
            skills: [
              {
                name: 'code-audit',
                calls: 2,
                used_by: 1,
                last_seen: '2026-02-01T00:00:00Z',
                registered: created,
                kind: created ? 'skill' : '',
              },
            ],
          })
        if (url.includes('/analytics/skills')) return json({}, 404)
        if (url.match(/\/api\/v1\/skills(\?.*)?$/) && method === 'POST') {
          created = true
          return json(skill('code-audit'), 201)
        }
        if (url.match(/\/api\/v1\/skills(\?.*)?$/)) {
          const items = created ? [skill('code-audit')] : []
          return json({ items, total: items.length })
        }
        throw new Error(`Unhandled fetch in test: ${method} ${url}`)
      }),
    )
    const user = userEvent.setup()
    renderPage(<SkillsPage />)

    const table = await screen.findByRole('table')
    const row = (await within(table).findByText('code-audit')).closest('tr')!
    expect(row).toHaveTextContent('Discovered')
    await user.click(within(row).getByRole('button', { name: 'Register code-audit' }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /^create$/i }))

    await vi.waitFor(() => {
      const r = within(screen.getByRole('table')).getByText('code-audit').closest('tr')!
      expect(r).not.toHaveTextContent('Discovered')
      expect(r).toHaveTextContent('Active')
    })
  })
})

describe('MCPs and Skills time filter', () => {
  beforeEach(() => sessionStorage.clear())
  afterEach(() => vi.unstubAllGlobals())

  const urls = () => vi.mocked(fetch).mock.calls.map(([u]) => String(u))

  it('MCPs reads custom dates from the URL and asks the API for them', async () => {
    stubMcps({ usage: [usageRow('context7')] })
    renderPage(<ConnectorsPage />, '/connectors?from=2026-01-01&to=2026-01-03')
    expect(await screen.findByRole('button', { name: /custom dates/i })).toHaveTextContent(
      'Jan 1 – Jan 3, 2026',
    )
    expect(urls()).toContain('/api/v1/analytics/mcps/usage?from=2026-01-01&to=2026-01-03')
    expect(
      await screen.findByRole('columnheader', { name: /Calls \(Jan 1 – Jan 3, 2026\)/ }),
    ).toBeInTheDocument()
  })

  it('Skills reads custom dates from the URL for usage and the summary', async () => {
    stubSkills({ usage: json({ range: 'custom', skills: [] }), skills: [] })
    renderPage(<SkillsPage />, '/skills?from=2026-01-01&to=2026-01-03')
    await screen.findByRole('button', { name: /custom dates/i })
    expect(urls()).toContain('/api/v1/analytics/skills/usage?from=2026-01-01&to=2026-01-03')
    expect(urls()).toContain('/api/v1/analytics/skills?from=2026-01-01&to=2026-01-03')
  })

  it('MCPs presets keep the shared look and default to Last 7d', async () => {
    stubMcps({ usage: [] })
    renderPage(<ConnectorsPage />, '/connectors')
    const group = await screen.findByRole('group', { name: 'Time range' })
    expect(within(group).getByRole('button', { name: 'Last 7d' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(urls()).toContain('/api/v1/analytics/mcps/usage?range=7d')
  })
})
