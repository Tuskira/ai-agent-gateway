import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import ModelsPage from '@/routes/ModelsPage'
import { chooseRowAction } from './rowActions'
import { contract } from './fixtures/contract'

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
      <MemoryRouter initialEntries={['/models']}>
        <ModelsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

function stubFetch(handlers: {
  models?: () => Response
  analytics?: () => Response
  catalog?: () => Response
  credentials?: () => Response
  deleteModel?: (id: string) => Response
  deleteCredential?: (name: string) => Response
}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    if (url.endsWith('/api/v1/models') && (!init || init.method === undefined)) {
      return handlers.models ? handlers.models() : jsonResponse({ items: [], total: 0 })
    }
    if (url.includes('/api/v1/analytics/models')) {
      return handlers.analytics
        ? handlers.analytics()
        : jsonResponse({ range: '7d', models: [], total_models: 0, highest_traffic: null })
    }
    if (url.endsWith('/api/v1/model-catalog')) {
      return handlers.catalog ? handlers.catalog() : jsonResponse({ providers: [] })
    }
    if (url.endsWith('/api/v1/credentials')) {
      return handlers.credentials ? handlers.credentials() : jsonResponse({ items: [], total: 0 })
    }
    const deleteCredMatch = url.match(/\/api\/v1\/credentials\/([^/]+)$/)
    if (deleteCredMatch && init?.method === 'DELETE') {
      return handlers.deleteCredential
        ? handlers.deleteCredential(decodeURIComponent(deleteCredMatch[1]!))
        : new Response(null, { status: 204 })
    }
    const deleteMatch = url.match(/\/api\/v1\/models\/([^/]+)$/)
    if (deleteMatch && init?.method === 'DELETE') {
      return handlers.deleteModel
        ? handlers.deleteModel(deleteMatch[1]!)
        : new Response(null, { status: 204 })
    }
    throw new Error(`Unhandled fetch in test: ${url} ${init?.method}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('ModelsPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('shows the admin-required state on a 403 from the model registry', async () => {
    stubFetch({ models: () => jsonResponse({ error: { type: 'forbidden', message: 'nope' } }, 403) })
    renderPage()

    expect(await screen.findByText(/admin access required/i)).toBeInTheDocument()
    expect(
      screen.getByText(/you're signed in with an agent key/i),
    ).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /add model/i })).not.toBeInTheDocument()
  })

  it('shows the analytics-off state when there is nothing registered and ClickHouse is off', async () => {
    stubFetch({
      models: () => jsonResponse({ items: [], total: 0 }),
      analytics: () => jsonResponse({ error: { type: 'not_found', message: 'off' } }, 404),
    })
    renderPage()

    expect(await screen.findByText(/analytics are turned off/i)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /view docs/i })).toHaveAttribute('href', '/docs')
  })

  it('shows the plain empty state when the registry and traffic are both empty but analytics is on', async () => {
    stubFetch({
      models: () => jsonResponse({ items: [], total: 0 }),
      analytics: () => jsonResponse({ range: '7d', models: [], total_models: 0, highest_traffic: null }),
    })
    renderPage()

    expect(await screen.findByText('No models seen yet')).toBeInTheDocument()
    expect(
      screen.getByText(/models appear here as agents route calls through the gateway/i),
    ).toBeInTheDocument()
    // The empty state is still actionable -- unlike admin-required.
    expect(screen.getByRole('button', { name: /add model/i })).toBeInTheDocument()
  })

  it('merges registry + traffic rows: seen/unseen/unregistered, platform badge, and tiles', async () => {
    // Both bodies are the real handlers' output (see fixtures/contract.ts):
    // registry = haiku (platform), local-coder, team-sonnet; traffic =
    // local-coder (registered) and claude-sonnet-4-5 (an unregistered name).
    stubFetch({
      models: () => jsonResponse(contract.list.response),
      analytics: () => jsonResponse(contract.analytics_models.response),
    })
    renderPage()

    expect(await screen.findByText('Models (4)')).toBeInTheDocument()
    expect(screen.getByText('Total Models').closest('div')!.nextElementSibling).toHaveTextContent(
      '4',
    )
    expect(screen.getByText('3.40K tokens')).toBeInTheDocument()

    // Seen + registered -> Active. (Also appears in the "Highest traffic"
    // tile above, so scope to the table row.)
    const table = screen.getByRole('table')
    const { getByText: getByTextInTable } = within(table)
    const coderRow = getByTextInTable('local-coder').closest('tr')!
    expect(coderRow).toHaveTextContent('Active')
    expect(coderRow).toHaveTextContent('openai_compat')
    // Registered, never seen -> Registered status, zeroed columns.
    const unusedRow = screen.getByText('team-sonnet').closest('tr')!
    expect(unusedRow).toHaveTextContent('Registered')
    // Platform scope -> badge, no actions menu.
    const platformRow = screen.getByText('haiku').closest('tr')!
    expect(platformRow).toHaveTextContent('platform')
    expect(
      screen.queryByRole('button', { name: 'Actions for haiku' }),
    ).not.toBeInTheDocument()
    // Traffic for an unregistered name still shows up, as "Discovered" --
    // distinct from "Active" -- with no actions menu (no registry record
    // to edit or delete).
    const discoveredRow = getByTextInTable('claude-sonnet-4-5').closest('tr')!
    expect(discoveredRow).toHaveTextContent('Discovered')
    expect(within(discoveredRow).getByText('Discovered').closest('span')).toHaveClass('border-dashed')
    expect(discoveredRow.querySelector('[title]')).toHaveAttribute(
      'title',
      'Seen in traffic but not registered. Calls pass straight through to the vendor.',
    )
    expect(
      screen.queryByRole('button', { name: 'Actions for claude-sonnet-4-5' }),
    ).not.toBeInTheDocument()

    expect(
      screen.getByRole('button', { name: 'Actions for local-coder' }),
    ).toBeInTheDocument()
  })

  const NEBIUS_PROVIDER = {
    id: 'prov1',
    slug: 'nebius',
    display_name: 'Nebius AI Studio',
    vendor: 'openai_compat',
    base_url: 'https://api.tokenfactory.eu-west2.nebius.com/v1',
    docs_url: '',
    enabled: true,
    models: [
      {
        id: 'catm1',
        model_id: 'zai-org/GLM-5.3',
        display_name: 'GLM 5.3',
        suggested_name: 'glm-5.3-nebius',
        price: null,
        capabilities: { tools: true, vision: null, streaming: null, max_context: null },
        enabled: true,
        tenant_model_id: null,
        tenant_model_name: null,
      },
    ],
  }

  it('shows an unconnected catalog model as "Available" with a Set up action that opens the Connect dialog', async () => {
    stubFetch({
      models: () => jsonResponse({ items: [], total: 0 }),
      analytics: () =>
        jsonResponse({ range: '7d', models: [], total_models: 0, highest_traffic: null }),
      catalog: () => jsonResponse({ providers: [NEBIUS_PROVIDER] }),
    })
    const user = userEvent.setup()
    renderPage()

    const row = (await screen.findByText('glm-5.3-nebius')).closest('tr')!
    expect(row).toHaveTextContent('Available')
    expect(within(row).getByText('Available').closest('span')).toHaveClass('border-dashed')
    expect(row).toHaveTextContent('Nebius AI Studio')

    await chooseRowAction(user, 'glm-5.3-nebius', 'Set up')
    expect(await screen.findByText('Connect Nebius AI Studio')).toBeInTheDocument()
  })

  it('gives a Discovered row a Set up action when its name matches a catalog model', async () => {
    stubFetch({
      models: () => jsonResponse({ items: [], total: 0 }),
      analytics: () =>
        jsonResponse({
          range: '7d',
          models: [
            {
              name: 'glm-5.3-nebius',
              provider: 'openai_compat',
              calls: 3,
              tokens: 300,
              cost_usd: null,
              used_by: 1,
              last_seen: '2026-01-02T00:00:00Z',
              status: 'active',
            },
          ],
          total_models: 1,
          highest_traffic: null,
        }),
      catalog: () => jsonResponse({ providers: [NEBIUS_PROVIDER] }),
    })
    const user = userEvent.setup()
    renderPage()

    const row = (await screen.findByText('glm-5.3-nebius')).closest('tr')!
    expect(row).toHaveTextContent('Discovered')

    await chooseRowAction(user, 'glm-5.3-nebius', 'Set up')
    expect(await screen.findByText('Connect Nebius AI Studio')).toBeInTheDocument()
  })

  it('deletes a tenant-scope model after confirmation', async () => {
    const fetchMock = stubFetch({
      models: () => jsonResponse(contract.list.response),
    })
    const user = userEvent.setup()
    renderPage()

    await chooseRowAction(user, 'team-sonnet', 'Delete')
    await user.click(await screen.findByRole('button', { name: /^delete$/i }))

    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(
          ([reqInput, init]) =>
            String(reqInput).endsWith(`/api/v1/models/${contract.create.response.id}`) &&
            (init as RequestInit | undefined)?.method === 'DELETE',
        ),
      ).toBe(true)
    })
  })

  function modelWithCredential(id: string, name: string, credential: string) {
    return {
      id,
      name,
      description: '',
      enabled: true,
      targets: [{ vendor: 'openai_compat', model: 'zai-org/GLM-5.3', credential }],
      price: null,
      limits: null,
      scope: 'tenant',
      metadata: {},
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
    }
  }

  it('warns a still-shared credential is used elsewhere, with no delete-it-too checkbox', async () => {
    const modelA = modelWithCredential('mA', 'model-a', 'shared-api-key')
    const modelB = modelWithCredential('mB', 'model-b', 'shared-api-key')
    const fetchMock = stubFetch({
      models: () => jsonResponse({ items: [modelA, modelB], total: 2 }),
    })
    const user = userEvent.setup()
    renderPage()

    await chooseRowAction(user, 'model-a', 'Delete')
    expect(await screen.findByText(/is still used by 1 other model/i)).toBeInTheDocument()
    expect(screen.getByText('shared-api-key')).toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /^delete$/i }))
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(
          ([reqInput]) => String(reqInput).endsWith('/api/v1/models/mA'),
        ),
      ).toBe(true)
    })
    expect(
      fetchMock.mock.calls.some(([reqInput, init]) => {
        const url = String(reqInput)
        return url.includes('/api/v1/credentials/') && init?.method === 'DELETE'
      }),
    ).toBe(false)
  })

  it('offers to delete a credential left orphaned by the deletion, off by default', async () => {
    const modelA = modelWithCredential('mA', 'model-a', 'lonely-api-key')
    const fetchMock = stubFetch({
      models: () => jsonResponse({ items: [modelA], total: 1 }),
    })
    const user = userEvent.setup()
    renderPage()

    await chooseRowAction(user, 'model-a', 'Delete')
    const checkbox = await screen.findByRole('checkbox', {
      name: /delete credential lonely-api-key too/i,
    })
    expect(checkbox).not.toBeChecked()
    expect(screen.getByText(/is no longer used by any model/i)).toBeInTheDocument()
    expect(screen.getByText('lonely-api-key')).toBeInTheDocument()

    await user.click(checkbox)
    await user.click(screen.getByRole('button', { name: /^delete$/i }))

    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(
          ([reqInput, init]) =>
            String(reqInput).endsWith('/api/v1/credentials/lonely-api-key') &&
            init?.method === 'DELETE',
        ),
      ).toBe(true)
    })
  })
})
