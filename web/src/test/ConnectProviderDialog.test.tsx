import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import { ConnectProviderDialog } from '@/components/app/models/ConnectProviderDialog'
import type { CatalogProvider } from '@/lib/model-catalog'
import { jsonResponse, stubApi } from './authHelpers'

const PROVIDER: CatalogProvider = {
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
      capabilities: { tools: false, vision: null, streaming: null, max_context: null },
      notes: 'Multi-turn tool use through Chat Completions translation is not supported yet.',
      enabled: true,
      tenant_model_id: null,
      tenant_model_name: null,
    },
    {
      id: 'catm2',
      model_id: 'moonshotai/Kimi-K3',
      display_name: 'Kimi K3',
      suggested_name: 'kimi-k3-nebius',
      price: { input: 1, output: 2 },
      capabilities: { tools: true, vision: null, streaming: null, max_context: null },
      notes: '',
      enabled: true,
      tenant_model_id: 'reg1',
      tenant_model_name: 'kimi-k3-nebius',
    },
  ],
}

function renderDialog(opts: { preselectModelId?: string; credentials?: unknown[] } = {}) {
  sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
  const calls = stubApi({
    'GET /credentials': () =>
      jsonResponse({ items: opts.credentials ?? [], total: (opts.credentials ?? []).length }),
    'POST /model-catalog/providers/prov1/test': () =>
      jsonResponse({
        ok: true,
        status: 200,
        models_found: ['zai-org/GLM-5.3'],
        // Keyed by the vendor's own model id, not the catalog row's uuid --
        // see `TestConnectionResult.catalog_matches` in `lib/model-catalog.ts`.
        catalog_matches: { 'zai-org/GLM-5.3': true, 'moonshotai/Kimi-K3': false },
      }),
    'POST /model-catalog/providers/prov1/connect': () =>
      jsonResponse({
        credential: { name: 'nebius-api-key', created: true },
        models: [{ catalog_model_id: 'catm1', model_id: 'reg-new', name: 'glm-5.3-nebius', status: 'created' }],
      }),
  })
  const onOpenChange = vi.fn()
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={queryClient}>
      <ConnectProviderDialog
        provider={PROVIDER}
        preselectModelId={opts.preselectModelId}
        onOpenChange={onOpenChange}
      />
    </QueryClientProvider>,
  )
  return { calls, onOpenChange }
}

describe('ConnectProviderDialog', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('preselects the clicked model and disables an already-set-up one', async () => {
    renderDialog({ preselectModelId: 'catm1' })

    const glm = await screen.findByRole('checkbox', { name: 'GLM 5.3' })
    expect(glm).toBeChecked()
    expect(glm).not.toHaveAttribute('aria-disabled', 'true')

    const kimi = screen.getByRole('checkbox', { name: 'Kimi K3' })
    expect(kimi).toBeChecked()
    expect(kimi).toHaveAttribute('aria-disabled', 'true')
    expect(screen.getByText(/already set up as/i)).toBeInTheDocument()
  })

  it('shows capability and price warnings only for a selected model', async () => {
    renderDialog({ preselectModelId: 'catm1' })

    await screen.findByRole('checkbox', { name: 'GLM 5.3' })
    expect(
      screen.getByText(/no tool calling: will not work with claude code/i),
    ).toBeInTheDocument()
    expect(screen.getByText(/no price set/i)).toBeInTheDocument()
  })

  it('shows the catalog model notes next to its capability warnings', async () => {
    renderDialog({ preselectModelId: 'catm1' })

    await screen.findByRole('checkbox', { name: 'GLM 5.3' })
    expect(
      screen.getByText(/multi-turn tool use through chat completions translation/i),
    ).toBeInTheDocument()
  })

  it('submits a new credential in the request body', async () => {
    const { calls, onOpenChange } = renderDialog({ preselectModelId: 'catm1' })
    const user = userEvent.setup()

    await screen.findByRole('checkbox', { name: 'GLM 5.3' })
    await user.click(screen.getByRole('button', { name: 'Enter API key' }))
    await user.type(screen.getByLabelText('Nebius AI Studio API key'), 'sk-test-123')
    await user.click(screen.getByRole('button', { name: /^connect$/i }))

    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false))
    const connectCall = calls.find((c) => c.path === '/model-catalog/providers/prov1/connect')
    expect(connectCall).toBeTruthy()
    expect(JSON.parse(connectCall!.body!)).toEqual({
      credential: { new: { name: 'nebius-api-key', api_key: 'sk-test-123' } },
      models: ['catm1'],
    })
  })

  it('submits an existing credential in the request body', async () => {
    const { calls, onOpenChange } = renderDialog({
      preselectModelId: 'catm1',
      credentials: [{ name: 'nebius-api-key', id: 'c1', type: 'api-key', field_names: ['token'], key_id: 'k1', created_at: '2026-01-01T00:00:00Z' }],
    })
    const user = userEvent.setup()

    await screen.findByRole('checkbox', { name: 'GLM 5.3' })
    await user.click(screen.getByRole('button', { name: /^connect$/i }))

    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false))
    const connectCall = calls.find((c) => c.path === '/model-catalog/providers/prov1/connect')
    expect(JSON.parse(connectCall!.body!)).toEqual({
      credential: { name: 'nebius-api-key' },
      models: ['catm1'],
    })
  })

  it('runs a test connection and shows which selected models were found', async () => {
    renderDialog({ preselectModelId: 'catm1' })
    const user = userEvent.setup()

    await screen.findByRole('checkbox', { name: 'GLM 5.3' })
    await user.click(screen.getByRole('button', { name: 'Enter API key' }))
    await user.type(screen.getByLabelText('Nebius AI Studio API key'), 'sk-test-123')
    await user.click(screen.getByRole('button', { name: /test connection/i }))

    expect(await screen.findByText(/connected \(http 200\)/i)).toBeInTheDocument()
    expect(screen.getByText('Found on provider')).toBeInTheDocument()
  })
})
