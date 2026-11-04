import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import type { Connector } from '@/lib/connectors'
import { HeaderRowEditor } from '@/components/app/connectors/HeaderRowEditor'
import { ConnectorDetailDialog } from '@/components/app/connectors/ConnectorDetailDialog'
import {
  headerRowError,
  headersToRows,
  newHeaderRow,
  rowsToHeaders,
  type HeaderRowState,
} from '@/components/app/connectors/header-rows'
import type { HeaderProvider } from '@/lib/headers'

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

const EXTERNAL: HeaderProvider = {
  id: 'external_service',
  name: 'External credential service',
  config_schema: {
    properties: {
      credential_id: { type: 'string', description: 'Id of the credential.' },
      field: { type: 'string', description: 'Field to return.' },
      prefix: { type: 'string', description: 'Optional prefix.' },
    },
    required: ['credential_id', 'field'],
  },
  needs_source: true,
  source_schema: { properties: { url: { type: 'string' } }, required: ['url'] },
  sources: ['vault-a', 'vault-b'],
  source_refs: [
    { id: 'src-a', name: 'vault-a' },
    { id: 'src-b', name: 'vault-b' },
  ],
  option_fields: ['credential_id'],
}

const SECRET_STORE: HeaderProvider = {
  id: 'secret_store',
  name: 'Secret store',
  config_schema: {
    properties: { name: { type: 'string', description: 'Credential name.' } },
  },
}

function providersFetch(
  options: (sourceId: string, field: string) => Response = () =>
    jsonResponse({ options: [] }),
  providers: HeaderProvider[] = [SECRET_STORE, EXTERNAL],
) {
  const mock = vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === 'string' ? input : input.toString()
    if (url.endsWith('/api/v1/headers/providers'))
      return jsonResponse({ providers, resolver_types: [] })
    const m = url.match(/\/api\/v1\/credential-sources\/([^/]+)\/options\?field=(.+)$/)
    if (m) return options(m[1] ?? '', m[2] ?? '')
    throw new Error(`Unhandled fetch in test: ${url}`)
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

/** Hosts one editor row in state and mirrors it as the saved header. */
function Harness({
  initial,
  onRow,
}: {
  initial?: HeaderRowState
  onRow: (row: HeaderRowState) => void
}) {
  const [row, setRow] = useState<HeaderRowState>(
    initial ?? { ...newHeaderRow(), headerName: 'Authorization' },
  )
  return (
    <HeaderRowEditor
      row={row}
      onChange={(next) => {
        setRow(next)
        onRow(next)
      }}
      onRemove={() => {}}
    />
  )
}

function renderEditor(initial?: HeaderRowState) {
  sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
  const rows: HeaderRowState[] = []
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={queryClient}>
      <Harness initial={initial} onRow={(r) => rows.push(r)} />
    </QueryClientProvider>,
  )
  return { latest: () => rows.at(-1) as HeaderRowState }
}

async function chooseOption(
  user: ReturnType<typeof userEvent.setup>,
  combobox: string,
  option: string,
) {
  await user.click(screen.getByRole('combobox', { name: combobox }))
  await user.click(await screen.findByRole('option', { name: option }))
}

async function pickExternal(user: ReturnType<typeof userEvent.setup>) {
  await chooseOption(user, 'Header type', 'External provider')
}

describe('HeaderRowEditor: credential source', () => {
  beforeEach(() => sessionStorage.clear())
  afterEach(() => vi.unstubAllGlobals())

  it('shows a Source select for a provider that needs one and saves it top-level', async () => {
    providersFetch()
    const user = userEvent.setup()
    const { latest } = renderEditor()

    await pickExternal(user)
    await chooseOption(user, 'Provider', 'External credential service')
    await chooseOption(user, 'Source', 'vault-b')

    expect(latest().source).toBe('vault-b')
    expect(latest().provider).toBe('external_service')
    expect(
      rowsToHeaders([
        { ...latest(), config: { credential_id: 'splunk', field: 'token' } },
      ]),
    ).toEqual({
      Authorization: {
        type: 'external',
        provider: 'external_service',
        source: 'vault-b',
        config: { credential_id: 'splunk', field: 'token' },
      },
    })
  })

  it('leaves other providers as they were: no Source select, no source key', async () => {
    providersFetch()
    const user = userEvent.setup()
    const { latest } = renderEditor()

    await pickExternal(user)
    await chooseOption(user, 'Provider', 'Secret store')

    expect(screen.queryByRole('combobox', { name: 'Source' })).not.toBeInTheDocument()
    await user.type(screen.getByLabelText('Credential name.'), 'github-pat')
    expect(rowsToHeaders([latest()])).toEqual({
      Authorization: {
        type: 'external',
        provider: 'secret_store',
        config: { name: 'github-pat' },
      },
    })
  })

  it('clears the source and config when the provider changes', async () => {
    providersFetch()
    const user = userEvent.setup()
    const { latest } = renderEditor()

    await pickExternal(user)
    await chooseOption(user, 'Provider', 'External credential service')
    await chooseOption(user, 'Source', 'vault-a')
    await chooseOption(user, 'Provider', 'Secret store')

    expect(latest().source).toBe('')
    expect(latest().config).toEqual({})
  })

  it('points at the Sources tab when the tenant has no enabled source', async () => {
    providersFetch(undefined, [{ ...EXTERNAL, sources: [], source_refs: [] }])
    const user = userEvent.setup()
    renderEditor()

    await pickExternal(user)
    await chooseOption(user, 'Provider', 'External credential service')
    expect(
      await screen.findByText(/no enabled credential sources yet/i),
    ).toBeInTheDocument()
  })

  it('blocks a row that needs a source until it has one', () => {
    const row: HeaderRowState = {
      ...newHeaderRow(),
      headerName: 'Authorization',
      type: 'external',
      provider: 'external_service',
    }
    expect(headerRowError(row, [EXTERNAL])).toBe('Source is required for this provider')
    expect(headerRowError({ ...row, source: 'vault-a' }, [EXTERNAL])).toBeNull()
    // Without provider info (or for a provider with no sources) nothing changes.
    expect(headerRowError(row)).toBeNull()
    expect(
      headerRowError({ ...row, provider: 'secret_store' }, [SECRET_STORE, EXTERNAL]),
    ).toBeNull()
  })

  it('round-trips an existing header that names a source', () => {
    const headers = {
      Authorization: {
        type: 'external' as const,
        provider: 'external_service',
        source: 'vault-a',
        config: { credential_id: 'splunk-prod', field: 'token' },
      },
    }
    expect(rowsToHeaders(headersToRows(headers))).toEqual(headers)
  })
})

describe('HeaderRowEditor: credential picker', () => {
  beforeEach(() => sessionStorage.clear())
  afterEach(() => vi.unstubAllGlobals())

  function editing(source = 'vault-a'): HeaderRowState {
    return {
      ...newHeaderRow(),
      headerName: 'Authorization',
      type: 'external',
      provider: 'external_service',
      source,
      config: {},
    }
  }

  const OFFERED = {
    options: [
      { value: 'splunk-prod', label: 'Splunk (production)' },
      { value: 'splunk-dev', label: 'Splunk (development)' },
      { value: 'ticketing', label: 'Ticketing' },
    ],
  }

  it('asks the chosen source for options and fills the field from a pick', async () => {
    const mock = providersFetch((id, field) => {
      expect(field).toBe('credential_id')
      return id === 'src-a' ? jsonResponse(OFFERED) : jsonResponse({ options: [] })
    })
    const user = userEvent.setup()
    const { latest } = renderEditor(editing())

    const input = await screen.findByRole('textbox', { name: 'credential_id' })
    await user.click(input)
    const list = await screen.findByRole('listbox', { name: 'credential_id options' })
    expect(within(list).getAllByRole('option')).toHaveLength(3)
    expect(
      mock.mock.calls.some(([u]) =>
        String(u).endsWith('/credential-sources/src-a/options?field=credential_id'),
      ),
    ).toBe(true)

    await user.click(within(list).getByRole('option', { name: /Splunk \(production\)/ }))
    expect(latest().config.credential_id).toBe('splunk-prod')
    expect(input).toHaveValue('splunk-prod')
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
  })

  it('filters the list by what is typed, on value or label', async () => {
    providersFetch(() => jsonResponse(OFFERED))
    const user = userEvent.setup()
    renderEditor(editing())

    const input = await screen.findByRole('textbox', { name: 'credential_id' })
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: /show credential_id options/i }),
      ).toBeEnabled(),
    )
    await user.type(input, 'development')
    const list = await screen.findByRole('listbox')
    const shown = within(list).getAllByRole('option')
    expect(shown).toHaveLength(1)
    expect(shown[0]).toHaveTextContent('splunk-dev')
  })

  it('keeps what was typed when the list has no match', async () => {
    providersFetch(() => jsonResponse(OFFERED))
    const user = userEvent.setup()
    const { latest } = renderEditor(editing())

    const input = await screen.findByRole('textbox', { name: 'credential_id' })
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: /show credential_id options/i }),
      ).toBeEnabled(),
    )
    await user.type(input, 'not-listed')
    expect(await screen.findByText(/no match/i)).toBeInTheDocument()
    expect(latest().config.credential_id).toBe('not-listed')
  })

  it('falls back to a plain text box when the service cannot be asked', async () => {
    providersFetch(() =>
      jsonResponse({ options: [], error: 'credential service returned HTTP 401' }),
    )
    const user = userEvent.setup()
    const { latest } = renderEditor(editing())

    expect(
      await screen.findByText(/credential service returned HTTP 401/),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /credential_id options/i })).toBeDisabled()
    await user.type(screen.getByRole('textbox', { name: 'credential_id' }), 'by-hand')
    expect(latest().config.credential_id).toBe('by-hand')
  })

  it('falls back to a plain text box when the options route is refused', async () => {
    providersFetch(() =>
      jsonResponse(
        { error: { type: 'forbidden', message: 'connector.update is required' } },
        403,
      ),
    )
    const user = userEvent.setup()
    const { latest } = renderEditor(editing())

    expect(await screen.findByText(/connector\.update is required/)).toBeInTheDocument()
    await user.type(screen.getByRole('textbox', { name: 'credential_id' }), 'by-hand')
    expect(latest().config.credential_id).toBe('by-hand')
  })

  it('uses plain inputs for fields the provider lists nothing for, and without a source', async () => {
    const mock = providersFetch()
    renderEditor(editing(''))

    // No source chosen yet: credential_id is a plain box and nothing is asked.
    expect(await screen.findByLabelText(/Id of the credential/)).toBeInTheDocument()
    expect(
      screen.queryByRole('textbox', { name: 'credential_id' }),
    ).not.toBeInTheDocument()
    expect(screen.getByLabelText(/Optional prefix/)).toBeInTheDocument()
    expect(mock.mock.calls.some(([u]) => String(u).includes('/options'))).toBe(false)
  })
})

describe('ConnectorDetailDialog: source', () => {
  beforeEach(() => sessionStorage.clear())
  afterEach(() => vi.unstubAllGlobals())

  it('shows the source name next to a header that names one', async () => {
    sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/tools')) return jsonResponse({ items: [], total: 0 })
        if (url.endsWith('/health')) return jsonResponse({ status: 'healthy' })
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    const connector: Connector = {
      id: 'c1',
      name: 'splunk',
      slug: 'splunk',
      endpoint: 'https://splunk.example.com/mcp',
      timeout_ms: 30000,
      status: 'healthy',
      metadata: {
        headers: {
          Authorization: {
            type: 'external',
            provider: 'external_service',
            source: 'vault-a',
            config: { credential_id: 'splunk-prod', field: 'token' },
          },
          'X-Plain': {
            type: 'external',
            provider: 'secret_store',
            config: { name: 'pat' },
          },
        },
      },
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
    }
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <ConnectorDetailDialog
            connector={connector}
            onOpenChange={() => {}}
            onEdit={() => {}}
          />
        </MemoryRouter>
      </QueryClientProvider>,
    )

    expect(await screen.findByText('vault-a')).toBeInTheDocument()
    expect(screen.getByText(/^Source:/)).toBeInTheDocument()
    // A header without a source shows none.
    expect(screen.getAllByText(/^Source:/)).toHaveLength(1)
  })
})
