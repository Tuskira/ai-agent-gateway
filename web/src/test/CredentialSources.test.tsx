import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import { AuthProvider } from '@/auth/AuthContext'
import CredentialsPage from '@/routes/CredentialsPage'
import { chooseRowAction } from './rowActions'

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

const SOURCE = {
  id: 'src-1',
  name: 'vault-a',
  provider: 'external_service',
  config: { url: 'https://creds.example.com', auth_credential: 'vault-auth' },
  enabled: true,
  used_by: [{ id: 'c1', name: 'splunk' }],
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
}

const UNUSED_SOURCE = { ...SOURCE, id: 'src-2', name: 'vault-b', used_by: [] }

const PROVIDERS = {
  providers: [
    {
      id: 'secret_store',
      name: 'Secret store',
      config_schema: { properties: { name: { type: 'string' } } },
    },
    {
      id: 'external_service',
      name: 'External credential service',
      config_schema: { properties: { credential_id: { type: 'string' } } },
      needs_source: true,
      source_schema: {
        properties: {
          url: { type: 'string', description: 'Base URL of the credential service.' },
          auth_credential: {
            type: 'string',
            description: 'Name of the credential holding the token.',
          },
        },
        required: ['url', 'auth_credential'],
      },
      sources: ['vault-a', 'vault-b'],
      source_refs: [
        { id: 'src-1', name: 'vault-a' },
        { id: 'src-2', name: 'vault-b' },
      ],
      option_fields: ['credential_id'],
    },
  ],
  resolver_types: ['static', 'token_field', 'incoming_field', 'external'],
}

const CREDENTIALS = {
  items: [
    {
      id: 'cred1',
      name: 'vault-auth',
      type: 'generic',
      field_names: ['token'],
      key_id: 'k',
      created_at: '2026-01-01T00:00:00Z',
    },
    {
      id: 'cred2',
      name: 'other-auth',
      type: 'generic',
      field_names: ['token'],
      key_id: 'k',
      created_at: '2026-01-01T00:00:00Z',
    },
  ],
  total: 2,
}

type Handler = (url: string, init: RequestInit) => Response | undefined

/** A fetch stub that serves the pages' reads and lets a test add handlers
 * for the calls it cares about (tried first). */
function stubFetch(
  roles: string[],
  handlers: Handler[] = [],
  sources = [SOURCE, UNUSED_SOURCE],
) {
  const mock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    const method = init?.method ?? 'GET'
    for (const h of handlers) {
      const res = h(`${method} ${url.replace('/api/v1', '')}`, init ?? {})
      if (res) return res
    }
    if (url.endsWith('/api/v1/auth/me'))
      return jsonResponse({
        subject: 'key-1',
        tenant_id: 't1',
        email: '',
        roles,
        auth_method: 'api_key',
        key_id: 'key-1',
      })
    if (url.endsWith('/api/v1/auth/config')) return jsonResponse({ api_key_login: true })
    if (method !== 'GET') throw new Error(`Unhandled fetch in test: ${method} ${url}`)
    if (url.endsWith('/api/v1/credentials')) return jsonResponse(CREDENTIALS)
    if (url.endsWith('/api/v1/models')) return jsonResponse({ items: [], total: 0 })
    if (url.endsWith('/api/v1/credential-sources'))
      return jsonResponse({ items: sources, total: sources.length })
    if (url.endsWith('/api/v1/headers/providers')) return jsonResponse(PROVIDERS)
    throw new Error(`Unhandled fetch in test: ${method} ${url}`)
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

function calls(mock: ReturnType<typeof vi.fn>, method: string, suffix: string) {
  return mock.mock.calls.filter(
    ([input, init]) =>
      String(input).endsWith(suffix) &&
      ((init as RequestInit | undefined)?.method ?? 'GET') === method,
  )
}

function bodyOf(call: unknown[] | undefined) {
  return JSON.parse(((call ?? [])[1] as RequestInit).body as string)
}

function renderPage(entry = '/credentials') {
  sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        <MemoryRouter initialEntries={[entry]}>
          <CredentialsPage />
        </MemoryRouter>
      </AuthProvider>
    </QueryClientProvider>,
  )
}

async function openSources(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('tab', { name: 'Sources' }))
  expect(await screen.findByText('vault-a')).toBeInTheDocument()
}

describe('Credentials page tabs', () => {
  beforeEach(() => sessionStorage.clear())
  afterEach(() => vi.unstubAllGlobals())

  it('opens on Credentials and switches to Sources for an admin', async () => {
    stubFetch(['admin'])
    const user = userEvent.setup()
    renderPage()

    expect(await screen.findByRole('tab', { name: 'Credentials' })).toHaveAttribute(
      'aria-selected',
      'true',
    )
    expect(screen.getByRole('button', { name: /add credential/i })).toBeInTheDocument()
    expect((await screen.findAllByText('vault-auth')).length).toBeGreaterThan(0)

    await openSources(user)
    expect(screen.getByRole('tab', { name: 'Sources' })).toHaveAttribute(
      'aria-selected',
      'true',
    )
    expect(screen.getByRole('button', { name: /add source/i })).toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: /add credential/i }),
    ).not.toBeInTheDocument()
  })

  it('opens straight on Sources from ?tab=sources', async () => {
    stubFetch(['admin'])
    renderPage('/credentials?tab=sources')
    expect(await screen.findByText('vault-a')).toBeInTheDocument()
  })

  it('disables the Sources tab for a non-admin and never asks for sources', async () => {
    const mock = stubFetch(['viewer'])
    renderPage('/credentials?tab=sources')

    const tab = await screen.findByRole('tab', { name: 'Sources' })
    await waitFor(() => expect(tab).toHaveAttribute('aria-disabled', 'true'))
    // The deep link does not open it either: the Credentials tab shows.
    expect(screen.getByRole('tab', { name: 'Credentials' })).toHaveAttribute(
      'aria-selected',
      'true',
    )
    expect(calls(mock, 'GET', '/api/v1/credential-sources')).toHaveLength(0)
  })
})

describe('Credential sources tab', () => {
  beforeEach(() => sessionStorage.clear())
  afterEach(() => vi.unstubAllGlobals())

  it('lists name, provider, url, state and the connectors using each source', async () => {
    stubFetch(['admin'], [], [SOURCE, { ...UNUSED_SOURCE, enabled: false }])
    const user = userEvent.setup()
    renderPage()
    await openSources(user)

    expect(screen.getAllByText('External credential service')).toHaveLength(2)
    expect(screen.getAllByText('https://creds.example.com')).toHaveLength(2)
    expect(screen.getByText('Enabled')).toBeInTheDocument()
    expect(screen.getByText('Disabled')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /splunk mcp/i })).toHaveAttribute(
      'href',
      '/connectors',
    )
    expect(screen.getByText('(unused)')).toBeInTheDocument()
  })

  it('creates a source from the provider schema, picking the auth credential', async () => {
    const mock = stubFetch(
      ['admin'],
      [
        (key) =>
          key === 'POST /credential-sources'
            ? jsonResponse({ ...SOURCE, id: 'src-3', name: 'vault-c', used_by: [] }, 201)
            : undefined,
      ],
    )
    const user = userEvent.setup()
    renderPage()
    await openSources(user)

    await user.click(screen.getByRole('button', { name: /add source/i }))
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('Name'), 'vault-c')
    await user.click(within(dialog).getByRole('combobox', { name: 'Provider' }))
    await user.click(
      await screen.findByRole('option', { name: 'External credential service' }),
    )

    // Only providers that use sources are offered, and the fields follow
    // the provider's source schema.
    await user.type(
      await within(dialog).findByLabelText(/Base URL of the credential service/),
      ' https://creds.example.com/ ',
    )
    await user.click(
      within(dialog).getByRole('combobox', { name: 'Authentication credential' }),
    )
    // The tenant's credentials, by name.
    expect(await screen.findByRole('option', { name: 'other-auth' })).toBeInTheDocument()
    await user.click(screen.getByRole('option', { name: 'vault-auth' }))
    await user.click(within(dialog).getByRole('button', { name: 'Add source' }))

    await waitFor(() =>
      expect(calls(mock, 'POST', '/api/v1/credential-sources')).toHaveLength(1),
    )
    expect(bodyOf(calls(mock, 'POST', '/api/v1/credential-sources')[0])).toEqual({
      name: 'vault-c',
      provider: 'external_service',
      config: { url: 'https://creds.example.com/', auth_credential: 'vault-auth' },
      enabled: true,
    })
  })

  it('does not post a source with a missing required field', async () => {
    const mock = stubFetch(['admin'])
    const user = userEvent.setup()
    renderPage()
    await openSources(user)

    await user.click(screen.getByRole('button', { name: /add source/i }))
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('Name'), 'vault-c')
    await user.click(within(dialog).getByRole('combobox', { name: 'Provider' }))
    await user.click(
      await screen.findByRole('option', { name: 'External credential service' }),
    )
    await user.click(within(dialog).getByRole('button', { name: 'Add source' }))

    expect(await within(dialog).findByText(/is required/)).toBeInTheDocument()
    expect(calls(mock, 'POST', '/api/v1/credential-sources')).toHaveLength(0)
  })

  it('edits a source: provider is fixed, config and name are sent', async () => {
    const mock = stubFetch(
      ['admin'],
      [
        (key) =>
          key === 'PUT /credential-sources/src-2'
            ? jsonResponse(UNUSED_SOURCE)
            : undefined,
      ],
    )
    const user = userEvent.setup()
    renderPage()
    await openSources(user)

    await chooseRowAction(user, 'vault-b', 'Edit')
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('combobox', { name: 'Provider' })).toBeDisabled()
    const url = await within(dialog).findByLabelText(/Base URL of the credential service/)
    expect(url).toHaveValue('https://creds.example.com')
    await user.clear(url)
    await user.type(url, 'https://creds2.example.com')
    await user.clear(within(dialog).getByLabelText('Name'))
    await user.type(within(dialog).getByLabelText('Name'), 'vault-b2')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(calls(mock, 'PUT', '/api/v1/credential-sources/src-2')).toHaveLength(1),
    )
    expect(bodyOf(calls(mock, 'PUT', '/api/v1/credential-sources/src-2')[0])).toEqual({
      name: 'vault-b2',
      config: { url: 'https://creds2.example.com', auth_credential: 'vault-auth' },
      enabled: true,
    })
  })

  it('disables an enabled source from the row menu', async () => {
    const mock = stubFetch(
      ['admin'],
      [
        (key) =>
          key === 'PUT /credential-sources/src-1'
            ? jsonResponse({ ...SOURCE, enabled: false })
            : undefined,
      ],
    )
    const user = userEvent.setup()
    renderPage()
    await openSources(user)

    await chooseRowAction(user, 'vault-a', 'Disable')
    await waitFor(() =>
      expect(calls(mock, 'PUT', '/api/v1/credential-sources/src-1')).toHaveLength(1),
    )
    expect(bodyOf(calls(mock, 'PUT', '/api/v1/credential-sources/src-1')[0])).toEqual({
      enabled: false,
    })
  })

  it('deletes an unused source', async () => {
    const mock = stubFetch(
      ['admin'],
      [
        (key) =>
          key === 'DELETE /credential-sources/src-2'
            ? new Response(null, { status: 204 })
            : undefined,
      ],
    )
    const user = userEvent.setup()
    renderPage()
    await openSources(user)

    await chooseRowAction(user, 'vault-b', 'Delete')
    expect(await screen.findByText('Delete "vault-b"?')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    await waitFor(() =>
      expect(calls(mock, 'DELETE', '/api/v1/credential-sources/src-2')).toHaveLength(1),
    )
  })

  it('shows the in-use conflict inside the delete dialog and keeps it open', async () => {
    stubFetch(
      ['admin'],
      [
        (key) =>
          key === 'DELETE /credential-sources/src-1'
            ? jsonResponse(
                {
                  error: {
                    type: 'conflict',
                    message:
                      'credential source "vault-a" is used by connector(s) "splunk"',
                  },
                },
                409,
              )
            : undefined,
      ],
    )
    const user = userEvent.setup()
    renderPage()
    await openSources(user)

    await chooseRowAction(user, 'vault-a', 'Delete')
    await user.click(await screen.findByRole('button', { name: 'Delete' }))

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('is used by connector(s) "splunk"')
    expect(screen.getByText('Delete "vault-a"?')).toBeInTheDocument()
  })

  it('tests a source and shows OK, then the error when it fails', async () => {
    let answer: { ok: boolean; error?: string } = { ok: true }
    const mock = stubFetch(
      ['admin'],
      [
        (key) =>
          key === 'POST /credential-sources/src-1/test'
            ? jsonResponse(answer)
            : undefined,
      ],
    )
    const user = userEvent.setup()
    renderPage()
    await openSources(user)

    await user.click(screen.getByRole('button', { name: 'Test vault-a' }))
    expect(await screen.findByText('OK')).toBeInTheDocument()

    answer = { ok: false, error: 'credential service returned HTTP 401' }
    await user.click(screen.getByRole('button', { name: 'Test vault-a' }))
    expect(
      await screen.findByText('credential service returned HTTP 401'),
    ).toBeInTheDocument()
    expect(screen.queryByText('OK')).not.toBeInTheDocument()
    expect(calls(mock, 'POST', '/api/v1/credential-sources/src-1/test')).toHaveLength(2)
  })
})
