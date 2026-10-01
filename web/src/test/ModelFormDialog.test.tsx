import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import ModelsPage from '@/routes/ModelsPage'
import { chooseRowAction } from './rowActions'
import { contract } from './fixtures/contract'

/**
 * The Add/Edit dialog against the model registry's real shapes. `fetch` is
 * answered from `fixtures/models-api.json` -- bodies produced by the real
 * handlers -- and what the dialog sends is compared with the fixture's
 * requests, which `internal/api/handlers/models_contract_test.go` replays
 * through those handlers (201 / 200). So a body the dialog builds that the
 * API would refuse fails here or there.
 */

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

const credentials = {
  items: [
    {
      id: 'cred-1',
      name: 'anthropic-prod',
      type: 'api_key',
      field_names: ['api_key'],
      key_id: 'k1',
      created_at: '2026-01-01T00:00:00Z',
    },
  ],
  total: 1,
}

/** Per-test answers for the writes a typed API key triggers; each is
 * called with the 1-based count of that request so far. */
interface StubWrites {
  credentialPost?: (n: number) => Response
  modelPost?: (n: number) => Response
}

function stubApi(models: unknown, writes: StubWrites = {}) {
  let credentialPosts = 0
  let modelPosts = 0
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    const method = init?.method ?? 'GET'
    if (url.endsWith('/api/v1/credentials') && method === 'POST') {
      credentialPosts++
      return writes.credentialPost
        ? writes.credentialPost(credentialPosts)
        : jsonResponse({ ...credentials.items[0], id: 'cred-new' }, 201)
    }
    if (url.endsWith('/api/v1/models') && method === 'POST' && writes.modelPost) {
      modelPosts++
      return writes.modelPost(modelPosts)
    }
    if (url.endsWith('/api/v1/models') && method === 'GET') return jsonResponse(models)
    if (url.includes('/api/v1/analytics/models')) {
      return jsonResponse({
        range: '7d',
        models: [],
        total_models: 0,
        highest_traffic: null,
      })
    }
    if (url.includes('/api/v1/credentials/') && method === 'PUT') {
      return jsonResponse({ ...credentials.items[0], id: 'cred-1' })
    }
    if (url.endsWith('/api/v1/credentials')) return jsonResponse(credentials)
    if (url.endsWith('/api/v1/model-catalog')) return jsonResponse({ providers: [] })
    if (url.endsWith('/api/v1/models') && method === 'POST') {
      return jsonResponse(contract.create.response, contract.create.status)
    }
    if (url.endsWith(`/api/v1/models/${contract.seed.response.id}`) && method === 'PUT') {
      return jsonResponse(contract.update.response, contract.update.status)
    }
    throw new Error(`Unhandled fetch in test: ${url} ${method}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function sentBody(fetchMock: ReturnType<typeof stubApi>, method: string, suffix: string) {
  const call = fetchMock.mock.calls.find(
    ([input, init]) => String(input).endsWith(suffix) && init?.method === method,
  )
  return call ? (JSON.parse(call[1]!.body as string) as unknown) : undefined
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

describe('ModelFormDialog (Add/Edit Model, against the model registry)', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('creates a model: target with base URL and allow caller key, pricing, limits', async () => {
    const fetchMock = stubApi({ items: [], total: 0 })
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add model/i }))

    await user.type(screen.getByPlaceholderText('e.g. claude-sonnet'), 'team-sonnet')
    await user.type(
      screen.getByPlaceholderText('What is this model used for?'),
      'Team default',
    )
    await user.type(
      screen.getByPlaceholderText('e.g. claude-sonnet-4-5-20250929'),
      'claude-sonnet-4-5',
    )
    // A base URL is optional on an Anthropic target (the API takes one).
    await user.type(
      screen.getByRole('textbox', { name: 'Target 1 base URL' }),
      'https://anthropic-proxy.internal',
    )
    await user.click(screen.getByRole('switch', { name: 'Target 1 allow caller key' }))
    await user.type(screen.getByPlaceholderText('3.00'), '3')
    await user.type(screen.getByPlaceholderText('15.00'), '15')
    await user.type(screen.getByLabelText('Daily budget (USD)'), '5')
    await user.type(screen.getByLabelText('Requests / minute'), '60')

    await user.click(screen.getByRole('button', { name: /^create$/i }))

    await waitFor(() => {
      expect(sentBody(fetchMock, 'POST', '/api/v1/models')).toEqual(
        contract.create.request,
      )
    })
  })

  it('pre-fills the edit dialog from the API row and PUTs the whole model back', async () => {
    const fetchMock = stubApi(contract.list.response)
    const user = userEvent.setup()
    renderPage()

    await chooseRowAction(user, 'local-coder', 'Edit')

    expect(screen.getByDisplayValue('local-coder')).toBeInTheDocument()
    expect(screen.getByDisplayValue('qwen2.5:1.5b')).toBeInTheDocument()
    expect(screen.getByDisplayValue('http://127.0.0.1:11434/v1')).toBeInTheDocument()
    expect(screen.getByDisplayValue('claude-haiku-4-5')).toBeInTheDocument()
    // label: shown on the OpenAI-compatible target only.
    expect(screen.getByRole('textbox', { name: 'Target 1 label' })).toHaveValue('ollama')
    expect(
      screen.queryByRole('textbox', { name: 'Target 2 label' }),
    ).not.toBeInTheDocument()
    // allow_caller_key: on for the first target; the second sends its own
    // credential, so the switch is off and cannot be turned on.
    expect(
      screen.getByRole('switch', { name: 'Target 1 allow caller key' }),
    ).toBeChecked()
    const second = screen.getByRole('switch', { name: 'Target 2 allow caller key' })
    expect(second).not.toBeChecked()
    // Base UI's Switch is a span, so it reports disabled through ARIA.
    expect(second).toHaveAttribute('aria-disabled', 'true')
    // limits {rpm: 5, max_tokens: 1024}; no price on this row (null).
    expect(screen.getByLabelText('Requests / minute')).toHaveValue('5')
    expect(screen.getByLabelText('Max tokens')).toHaveValue('1024')
    expect(screen.getByLabelText('Daily budget (USD)')).toHaveValue('')
    expect(screen.getByPlaceholderText('3.00')).toHaveValue('')

    const description = screen.getByPlaceholderText('What is this model used for?')
    await user.clear(description)
    await user.type(description, 'Local model, now budgeted')
    await user.type(screen.getByLabelText('Monthly budget (USD)'), '20')

    await user.click(screen.getByRole('button', { name: /^save changes$/i }))

    await waitFor(() => {
      expect(
        sentBody(fetchMock, 'PUT', `/api/v1/models/${contract.seed.response.id}`),
      ).toEqual(contract.update.request)
    })
  })

  it('offers a label on an OpenAI-compatible target only, and refuses a reserved one', async () => {
    const fetchMock = stubApi(contract.list.response)
    const user = userEvent.setup()
    renderPage()

    // local-coder: target 1 is OpenAI-compatible (label "ollama"), target 2
    // is Anthropic, which takes no label.
    await chooseRowAction(user, 'local-coder', 'Edit')
    expect(screen.getAllByText('Label (optional, e.g. groq)')).toHaveLength(1)

    const label = screen.getByRole('textbox', { name: 'Target 1 label' })
    await user.clear(label)
    await user.type(label, 'anthropic')
    expect(screen.getByText('Label "anthropic" is reserved')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /^save changes$/i }))
    const put = `/api/v1/models/${contract.seed.response.id}`
    expect(sentBody(fetchMock, 'PUT', put)).toBeUndefined()

    await user.clear(label)
    await user.type(label, 'Groq')
    expect(screen.getByText(/^Label: lowercase letters/)).toBeInTheDocument()

    await user.clear(label)
    await user.type(label, 'groq')
    await user.click(screen.getByRole('button', { name: /^save changes$/i }))
    await waitFor(() => {
      const body = sentBody(fetchMock, 'PUT', put) as { targets: unknown[] }
      expect(body.targets).toEqual([
        { ...contract.seed.request.targets[0], label: 'groq' },
        contract.seed.request.targets[1],
      ])
    })
  })

  it('rejects a name with uppercase/invalid characters before submitting', async () => {
    const fetchMock = stubApi({ items: [], total: 0 })
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add model/i }))
    await user.type(screen.getByPlaceholderText('e.g. claude-sonnet'), 'Claude Sonnet!')
    await user.click(screen.getByRole('button', { name: /^create$/i }))

    expect(
      await screen.findByText(
        /lowercase letters, numbers, dot, underscore, and dash only/i,
      ),
    ).toBeInTheDocument()
    // Never reached the API with an invalid name.
    expect(sentBody(fetchMock, 'POST', '/api/v1/models')).toBeUndefined()
  })

  it('requires input and output rates once any price is set, and whole-number limits', async () => {
    const fetchMock = stubApi({ items: [], total: 0 })
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add model/i }))
    await user.type(screen.getByPlaceholderText('e.g. claude-sonnet'), 'm')
    await user.type(
      screen.getByPlaceholderText('e.g. claude-sonnet-4-5-20250929'),
      'claude',
    )
    // The API stores a price as given: a lone cache rate would save input
    // and output as 0.
    await user.type(screen.getByPlaceholderText('0.30'), '0.3')
    await user.type(screen.getByLabelText('Requests / minute'), '1.5')
    await user.type(screen.getByLabelText('Max tokens'), '100001')
    await user.click(screen.getByRole('button', { name: /^create$/i }))

    expect(
      await screen.findAllByText('Required when a pricing override is set'),
    ).toHaveLength(2)
    expect(screen.getByText('Must be a whole number, 0 or more')).toBeInTheDocument()
    expect(sentBody(fetchMock, 'POST', '/api/v1/models')).toBeUndefined()

    // rpm is capped like the API's (100000); max_tokens is not.
    const rpm = screen.getByLabelText('Requests / minute')
    await user.clear(rpm)
    await user.type(rpm, '100001')
    await user.click(screen.getByRole('button', { name: /^create$/i }))
    expect(await screen.findByText('At most 100000')).toBeInTheDocument()
    expect(sentBody(fetchMock, 'POST', '/api/v1/models')).toBeUndefined()
  })
})

describe('ModelFormDialog: Nebius / Together AI providers and "Accept API key"', () => {
  const NEBIUS_URL = 'https://api.tokenfactory.eu-west2.nebius.com/v1/'
  const TEST_KEY = 'test-key-not-real'

  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  async function choose(
    user: ReturnType<typeof userEvent.setup>,
    combobox: string,
    option: string,
  ) {
    await user.click(screen.getByRole('combobox', { name: combobox }))
    await user.click(await screen.findByRole('option', { name: option }))
  }

  /** Add model -> kimi-k3 on Nebius with a typed key, then Create. */
  async function createKimiOnNebius(user: ReturnType<typeof userEvent.setup>) {
    await user.click(await screen.findByRole('button', { name: /add model/i }))
    await user.type(screen.getByPlaceholderText('e.g. claude-sonnet'), 'kimi-k3')
    await choose(user, 'Target 1 vendor', 'Nebius')
    await user.type(
      screen.getByPlaceholderText('e.g. claude-sonnet-4-5-20250929'),
      'moonshotai/Kimi-K3',
    )
    await choose(user, 'Target 1 credential', 'Accept API key')
    await user.type(screen.getByLabelText('Target 1 API key'), TEST_KEY)
    await user.click(screen.getByRole('button', { name: /^create$/i }))
  }

  function postIndex(fetchMock: ReturnType<typeof stubApi>, suffix: string) {
    return fetchMock.mock.calls.findIndex(
      ([input, init]) => String(input).endsWith(suffix) && init?.method === 'POST',
    )
  }

  it('stores a typed key as a credential first, then saves the model referencing it', async () => {
    const fetchMock = stubApi({ items: [], total: 0 })
    const user = userEvent.setup()
    renderPage()

    await createKimiOnNebius(user)

    await waitFor(() => {
      expect(sentBody(fetchMock, 'POST', '/api/v1/models')).toBeDefined()
    })
    expect(sentBody(fetchMock, 'POST', '/api/v1/credentials')).toEqual({
      name: 'nebius-api-key',
      type: 'api_key',
      payload: { api_key: TEST_KEY },
    })
    expect(postIndex(fetchMock, '/api/v1/credentials')).toBeLessThan(
      postIndex(fetchMock, '/api/v1/models'),
    )
    const model = sentBody(fetchMock, 'POST', '/api/v1/models') as { targets: unknown[] }
    expect(model.targets).toEqual([
      {
        vendor: 'openai_compat',
        model: 'moonshotai/Kimi-K3',
        base_url: NEBIUS_URL,
        label: 'nebius',
        credential: 'nebius-api-key',
      },
    ])
    expect(JSON.stringify(model)).not.toContain(TEST_KEY)
  })

  it('Together AI pre-fills its base URL, which stays editable', async () => {
    stubApi({ items: [], total: 0 })
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add model/i }))
    await choose(user, 'Target 1 vendor', 'Together AI')
    const baseUrl = screen.getByRole('textbox', { name: 'Target 1 base URL' })
    expect(baseUrl).toHaveValue('https://api.together.ai/v1')
    await user.clear(baseUrl)
    await user.type(baseUrl, 'https://api.together.xyz/v1')
    expect(baseUrl).toHaveValue('https://api.together.xyz/v1')
  })

  it('each vendor fills its own link, which stays editable; Others starts empty', async () => {
    stubApi({ items: [], total: 0 })
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add model/i }))
    const baseUrl = () => screen.getByRole('textbox', { name: 'Target 1 base URL' })
    for (const [vendor, url] of [
      ['OpenAI', 'https://api.openai.com/v1'],
      ['Google Gemini', 'https://generativelanguage.googleapis.com/v1beta/openai/'],
      ['Anthropic', 'https://api.anthropic.com'],
      ['Nebius', NEBIUS_URL],
      ['Others', ''],
    ] as const) {
      await choose(user, 'Target 1 vendor', vendor)
      expect(baseUrl()).toHaveValue(url)
    }
    await user.type(baseUrl(), 'https://api.groq.com/openai/v1')
    expect(baseUrl()).toHaveValue('https://api.groq.com/openai/v1')
  })

  it('Others keeps its label field while a label naming a provider is typed', async () => {
    stubApi({ items: [], total: 0 })
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add model/i }))
    await choose(user, 'Target 1 vendor', 'Others')
    await user.type(screen.getByRole('textbox', { name: 'Target 1 label' }), 'openai-eu')
    expect(screen.getByRole('textbox', { name: 'Target 1 label' })).toHaveValue(
      'openai-eu',
    )
  })

  it('sends no model when storing the key fails', async () => {
    const fetchMock = stubApi(
      { items: [], total: 0 },
      {
        credentialPost: () =>
          jsonResponse({ error: { type: 'conflict', message: 'already exists' } }, 409),
      },
    )
    const user = userEvent.setup()
    renderPage()

    await createKimiOnNebius(user)

    await waitFor(() => {
      expect(sentBody(fetchMock, 'POST', '/api/v1/credentials')).toBeDefined()
    })
    expect(sentBody(fetchMock, 'POST', '/api/v1/models')).toBeUndefined()
  })

  it('a retry after the model save fails reuses the stored key', async () => {
    const fetchMock = stubApi(
      { items: [], total: 0 },
      {
        modelPost: (n) =>
          n === 1
            ? jsonResponse(
                { error: { type: 'invalid_request', message: 'try again' } },
                400,
              )
            : jsonResponse(contract.create.response, contract.create.status),
      },
    )
    const user = userEvent.setup()
    renderPage()

    await createKimiOnNebius(user)
    await waitFor(() => {
      expect(postIndex(fetchMock, '/api/v1/models')).toBeGreaterThanOrEqual(0)
    })
    await user.click(screen.getByRole('button', { name: /^create$/i }))

    await waitFor(() => {
      const modelPosts = fetchMock.mock.calls.filter(
        ([input, init]) =>
          String(input).endsWith('/api/v1/models') && init?.method === 'POST',
      )
      expect(modelPosts).toHaveLength(2)
      for (const [, init] of modelPosts) {
        const body = JSON.parse(init!.body as string) as {
          targets: { credential?: string }[]
        }
        expect(body.targets[0]!.credential).toBe('nebius-api-key')
      }
    })
    const credentialPosts = fetchMock.mock.calls.filter(
      ([input, init]) =>
        String(input).endsWith('/api/v1/credentials') && init?.method === 'POST',
    )
    expect(credentialPosts).toHaveLength(1)
  })

  it('offers no "Accept API key" on Bedrock, which signs with an AWS key pair', async () => {
    stubApi({ items: [], total: 0 })
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add model/i }))
    await choose(user, 'Target 1 vendor', 'Amazon Bedrock')
    await user.click(screen.getByRole('combobox', { name: 'Target 1 credential' }))
    expect(
      await screen.findByRole('option', { name: 'anthropic-prod' }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('option', { name: 'Accept API key' }),
    ).not.toBeInTheDocument()
  })

  it('editing a saved Nebius target shows Nebius and keeps its label and base URL', async () => {
    const saved = {
      ...contract.seed.response,
      targets: [
        {
          vendor: 'openai_compat',
          model: 'moonshotai/Kimi-K3',
          base_url: NEBIUS_URL,
          label: 'nebius',
          credential: 'anthropic-prod',
        },
      ],
    }
    const fetchMock = stubApi({ items: [saved], total: 1 })
    const user = userEvent.setup()
    renderPage()

    await chooseRowAction(user, saved.name, 'Edit')
    expect(screen.getByRole('combobox', { name: 'Target 1 vendor' })).toHaveTextContent(
      'Nebius',
    )
    await user.click(screen.getByRole('button', { name: /^save changes$/i }))

    await waitFor(() => {
      const body = sentBody(fetchMock, 'PUT', `/api/v1/models/${saved.id}`) as {
        targets: unknown[]
      }
      expect(body.targets).toEqual(saved.targets)
    })
  })
})
