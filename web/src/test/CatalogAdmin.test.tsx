import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import { CatalogTab } from '@/components/app/connectors/CatalogTab'
import type { CatalogEntry } from '@/lib/mcpCatalog'

const PLATFORM: CatalogEntry = {
  id: 'p1',
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
  enabled: true,
  scope: 'platform',
  supported: true,
  added: false,
}
const MINE: CatalogEntry = {
  ...PLATFORM,
  id: 't1',
  slug: 'mine',
  name: 'Mine',
  description: 'Our server.',
  icon: '',
  category: 'internal',
  url: 'https://mine.example.com/mcp',
  auth: {
    kind: 'bearer',
    fields: [{ name: 'token', label: 'Token', secret: true, required: true }],
  },
  scope: 'tenant',
  enabled: false,
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function renderTab(canManage: boolean) {
  sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <CatalogTab canManage={canManage} />
    </QueryClientProvider>,
  )
}

function stubFetch(
  handlers: { write?: (url: string, init: RequestInit) => Response } = {},
) {
  const mock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    const method = init?.method ?? 'GET'
    if (method === 'GET' && url.endsWith('/api/v1/mcp-catalog'))
      return jsonResponse({ items: [PLATFORM, MINE], total: 2 })
    if (method !== 'GET' && handlers.write) return handlers.write(url, init ?? {})
    if (method === 'DELETE') return new Response(null, { status: 204 })
    throw new Error(`Unhandled fetch in test: ${method} ${url}`)
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

describe('Catalog admin controls', () => {
  beforeEach(() => sessionStorage.clear())
  afterEach(() => vi.unstubAllGlobals())

  it('shows no management controls to a non-admin', async () => {
    stubFetch()
    renderTab(false)
    await screen.findByTestId('catalog-card-mine')
    expect(screen.queryByRole('button', { name: 'New catalog entry' })).toBeNull()
    expect(screen.queryByRole('button', { name: /^Edit / })).toBeNull()
    expect(screen.queryByRole('button', { name: /^Delete / })).toBeNull()
  })

  it('marks platform entries and keeps them read-only for admins', async () => {
    stubFetch()
    renderTab(true)
    const platform = await screen.findByTestId('catalog-card-drawio')
    expect(within(platform).getByText('Platform')).toBeInTheDocument()
    expect(within(platform).queryByRole('button', { name: /^Edit / })).toBeNull()
    expect(within(platform).queryByRole('button', { name: /^Delete / })).toBeNull()

    const mine = screen.getByTestId('catalog-card-mine')
    expect(within(mine).queryByText('Platform')).toBeNull()
    expect(within(mine).getByText('Disabled')).toBeInTheDocument()
    expect(within(mine).getByRole('button', { name: 'Edit Mine' })).toBeInTheDocument()
    expect(within(mine).getByRole('button', { name: 'Delete Mine' })).toBeInTheDocument()
    expect(within(mine).getByRole('button', { name: 'Add Mine' })).toBeDisabled()
  })

  it('creates an entry with the slug derived from the name', async () => {
    const fetchMock = stubFetch({
      write: () => jsonResponse({ ...MINE, slug: 'my-new-server' }, 201),
    })
    renderTab(true)
    await userEvent.click(
      await screen.findByRole('button', { name: 'New catalog entry' }),
    )
    const dialog = await screen.findByRole('dialog')

    await userEvent.type(within(dialog).getByLabelText('Name'), 'My New Server')
    expect(within(dialog).getByLabelText('Slug')).toHaveValue('my-new-server')
    await userEvent.type(
      within(dialog).getByLabelText('Server URL'),
      'https://mcp.example.com/mcp',
    )
    await userEvent.selectOptions(
      within(dialog).getByLabelText('Authentication'),
      'bearer',
    )
    // A bearer kind starts with exactly one credential field to fill in.
    await userEvent.type(within(dialog).getByLabelText('Field name'), 'token')
    await userEvent.type(within(dialog).getByLabelText('Label'), 'Token')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Create entry' }))

    await waitFor(() => {
      const post = fetchMock.mock.calls.find(
        ([, init]) => (init as RequestInit | undefined)?.method === 'POST',
      )
      expect(post).toBeDefined()
      expect(String(post![0]).endsWith('/api/v1/mcp-catalog')).toBe(true)
      const body = JSON.parse((post![1] as RequestInit).body as string)
      expect(body).toMatchObject({
        slug: 'my-new-server',
        name: 'My New Server',
        url: 'https://mcp.example.com/mcp',
        enabled: true,
        auth: { kind: 'bearer', fields: [{ name: 'token', label: 'Token' }] },
      })
    })
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })

  it('edits a tenant entry with PUT to its slug and a locked slug field', async () => {
    const fetchMock = stubFetch({
      write: () => jsonResponse({ ...MINE, name: 'Mine v2' }),
    })
    renderTab(true)
    await userEvent.click(await screen.findByRole('button', { name: 'Edit Mine' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByLabelText('Slug')).toBeDisabled()
    const name = within(dialog).getByLabelText('Name')
    await userEvent.clear(name)
    await userEvent.type(name, 'Mine v2')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Save changes' }))

    await waitFor(() => {
      const put = fetchMock.mock.calls.find(
        ([, init]) => (init as RequestInit | undefined)?.method === 'PUT',
      )
      expect(put).toBeDefined()
      expect(String(put![0]).endsWith('/api/v1/mcp-catalog/mine?scope=tenant')).toBe(true)
      const body = JSON.parse((put![1] as RequestInit).body as string)
      expect(body.name).toBe('Mine v2')
      expect(body.slug).toBeUndefined()
      expect(body.auth.kind).toBe('bearer')
    })
  })

  it('shows the server error and keeps the form open', async () => {
    stubFetch({
      write: () =>
        jsonResponse(
          {
            error: {
              type: 'validation_error',
              message: 'url must be an absolute https URL',
            },
          },
          400,
        ),
    })
    renderTab(true)
    await userEvent.click(await screen.findByRole('button', { name: 'Edit Mine' }))
    const dialog = await screen.findByRole('dialog')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Save changes' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'url must be an absolute https URL',
    )
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })

  it('deletes a tenant entry after confirmation', async () => {
    const fetchMock = stubFetch()
    renderTab(true)
    await userEvent.click(await screen.findByRole('button', { name: 'Delete Mine' }))
    const confirm = await screen.findByRole('alertdialog')
    await userEvent.click(within(confirm).getByRole('button', { name: 'Delete' }))
    await waitFor(() => {
      const del = fetchMock.mock.calls.find(
        ([, init]) => (init as RequestInit | undefined)?.method === 'DELETE',
      )
      expect(del).toBeDefined()
      expect(String(del![0]).endsWith('/api/v1/mcp-catalog/mine?scope=tenant')).toBe(true)
    })
  })
})
