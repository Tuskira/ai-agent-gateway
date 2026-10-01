import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import ConnectorsPage from '@/routes/ConnectorsPage'
import { buildAddBody, type CatalogEntry } from '@/lib/mcpCatalog'

const LANGFUSE: CatalogEntry = {
  id: 'cat-lf',
  slug: 'langfuse',
  name: 'Langfuse',
  description: 'Query traces.',
  icon: 'activity',
  category: 'observability',
  url: 'https://us.cloud.langfuse.com/api/public/mcp',
  url_overridable: true,
  transport: 'streamable-http',
  auth: {
    kind: 'basic',
    fields: [
      {
        name: 'public_key',
        label: 'Public key',
        required: true,
        placeholder: 'pk-lf-...',
      },
      { name: 'secret_key', label: 'Secret key', secret: true, required: true },
    ],
  },
  suggested_tools: [],
  supported: true,
  enabled: true,
  scope: 'platform',
  added: false,
}
const DRAWIO: CatalogEntry = {
  id: 'cat-dr',
  slug: 'drawio',
  name: 'Draw.io',
  description: 'Diagrams.',
  icon: 'shapes',
  category: 'diagramming',
  url: 'https://mcp.draw.io/mcp',
  url_overridable: false,
  transport: 'streamable-http',
  auth: { kind: 'none', fields: [] },
  suggested_tools: [],
  supported: true,
  enabled: true,
  scope: 'platform',
  added: true,
  connector_id: 'c-dr',
}
const GDRIVE: CatalogEntry = {
  id: 'cat-gd',
  slug: 'google-drive',
  name: 'Google Drive',
  description: 'Files.',
  icon: 'hard-drive',
  category: 'storage',
  url: 'https://drivemcp.googleapis.com/mcp/v1',
  url_overridable: false,
  transport: 'streamable-http',
  auth: { kind: 'oauth', fields: [] },
  suggested_tools: [],
  supported: false,
  unsupported_reason: 'Requires OAuth, which the gateway does not support yet.',
  enabled: true,
  scope: 'platform',
  added: false,
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

function stubFetch(addResponse?: () => Response) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    if (url.endsWith('/api/v1/mcp-catalog'))
      return jsonResponse({ items: [LANGFUSE, DRAWIO, GDRIVE], total: 3 })
    if (url.endsWith('/api/v1/mcp-catalog/langfuse/add') && init?.method === 'POST')
      return (
        addResponse?.() ??
        jsonResponse(
          {
            connector: { id: 'c-lf', name: 'Langfuse', slug: 'langfuse' },
            discovery: { status: 'healthy', tools_discovered: 4 },
          },
          201,
        )
      )
    if (url.endsWith('/api/v1/connectors')) return jsonResponse({ items: [], total: 0 })
    throw new Error(`Unhandled fetch in test: ${url}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('buildAddBody', () => {
  it('drops blank fields and sends url/name only when changed', () => {
    const body = buildAddBody(
      LANGFUSE,
      { public_key: ' pk ', secret_key: '' },
      'Langfuse',
      LANGFUSE.url,
    )
    expect(body).toEqual({ fields: { public_key: 'pk' } })

    const changed = buildAddBody(
      LANGFUSE,
      { public_key: 'a', secret_key: 'b' },
      'Langfuse EU',
      'https://cloud.langfuse.com/api/public/mcp',
    )
    expect(changed).toEqual({
      fields: { public_key: 'a', secret_key: 'b' },
      name: 'Langfuse EU',
      url: 'https://cloud.langfuse.com/api/public/mcp',
    })
  })

  it('never sends a url for an entry that is not overridable', () => {
    const body = buildAddBody(DRAWIO, {}, 'Draw.io', 'https://evil.example.com')
    expect(body.url).toBeUndefined()
  })
})

describe('MCPs page Catalog tab', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('does not fetch the catalog until the tab is opened', async () => {
    const fetchMock = stubFetch()
    renderPage()
    await screen.findByText('No MCPs yet')
    expect(fetchMock.mock.calls.some(([u]) => String(u).includes('/mcp-catalog'))).toBe(
      false,
    )
  })

  it('shows added and unsupported states on the cards', async () => {
    stubFetch()
    renderPage()
    await userEvent.click(await screen.findByRole('tab', { name: 'Catalog' }))

    const lf = await screen.findByTestId('catalog-card-langfuse')
    expect(within(lf).getByText('Basic auth · observability')).toBeInTheDocument()
    expect(within(lf).getByRole('button', { name: 'Add Langfuse' })).toBeEnabled()

    const dr = screen.getByTestId('catalog-card-drawio')
    expect(within(dr).getByRole('button', { name: 'Added' })).toBeDisabled()

    const gd = screen.getByTestId('catalog-card-google-drive')
    expect(within(gd).getByText(/Requires OAuth — not supported yet/)).toBeInTheDocument()
    expect(within(gd).getByRole('button', { name: 'Add Google Drive' })).toBeDisabled()
  })

  it('renders the dialog from auth.fields and submits the body', async () => {
    const fetchMock = stubFetch()
    renderPage()
    await userEvent.click(await screen.findByRole('tab', { name: 'Catalog' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Add Langfuse' }))

    const dialog = await screen.findByRole('dialog')
    const pub = within(dialog).getByLabelText(/Public key/)
    const secret = within(dialog).getByLabelText(/Secret key/)
    expect(pub).toHaveAttribute('type', 'text')
    expect(secret).toHaveAttribute('type', 'password')
    // url_overridable entry shows the URL override, prefilled.
    const urlInput = within(dialog).getByLabelText('Server URL')
    expect(urlInput).toHaveValue(LANGFUSE.url)

    // Required fields block submit.
    await userEvent.click(within(dialog).getByRole('button', { name: 'Add to tenant' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'Public key is required',
    )
    expect(
      fetchMock.mock.calls.some(
        ([, init]) => (init as RequestInit | undefined)?.method === 'POST',
      ),
    ).toBe(false)

    await userEvent.type(pub, 'pk-lf-1')
    await userEvent.type(secret, 'sk-lf-2')
    await userEvent.clear(urlInput)
    await userEvent.type(urlInput, 'https://cloud.langfuse.com/api/public/mcp')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Add to tenant' }))

    await waitFor(() => {
      const post = fetchMock.mock.calls.find(
        ([u, init]) =>
          String(u).endsWith('/mcp-catalog/langfuse/add') &&
          (init as RequestInit | undefined)?.method === 'POST',
      )
      expect(post).toBeDefined()
      expect(JSON.parse((post![1] as RequestInit).body as string)).toEqual({
        fields: { public_key: 'pk-lf-1', secret_key: 'sk-lf-2' },
        url: 'https://cloud.langfuse.com/api/public/mcp',
      })
    })
    // On success the dialog closes and the page returns to the MCP list.
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(await screen.findByText('No MCPs yet')).toBeInTheDocument()
  })

  it('shows the server error in the dialog and keeps it open', async () => {
    stubFetch(() =>
      jsonResponse(
        { error: { type: 'already_added', message: 'Langfuse is already added' } },
        409,
      ),
    )
    renderPage()
    await userEvent.click(await screen.findByRole('tab', { name: 'Catalog' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Add Langfuse' }))
    const dialog = await screen.findByRole('dialog')
    await userEvent.type(within(dialog).getByLabelText(/Public key/), 'a')
    await userEvent.type(within(dialog).getByLabelText(/Secret key/), 'b')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Add to tenant' }))

    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'Langfuse is already added',
    )
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })
})
