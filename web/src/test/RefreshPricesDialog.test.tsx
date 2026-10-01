import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import { RefreshPricesDialog } from '@/components/app/models/RefreshPricesDialog'
import type { CatalogProvider } from '@/lib/model-catalog'
import { jsonResponse, stubApi } from './authHelpers'

const NEBIUS_PROVIDER: CatalogProvider = {
  id: 'prov-nebius',
  slug: 'nebius',
  display_name: 'Nebius AI Studio',
  vendor: 'openai_compat',
  base_url: 'https://api.tokenfactory.eu-west2.nebius.com/v1',
  docs_url: '',
  enabled: true,
  models: [],
}

const TOGETHER_PROVIDER: CatalogProvider = {
  id: 'prov-together',
  slug: 'together',
  display_name: 'Together AI',
  vendor: 'openai_compat',
  base_url: 'https://api.together.ai/v1',
  docs_url: '',
  enabled: true,
  models: [],
}

const PREVIEW_RESULT = {
  source_url: 'https://tokenfactory.nebius.com/api/public/models_info',
  fetched_at: '2026-10-01T00:00:00Z',
  items: [
    {
      catalog_model_id: 'catm1',
      model_id: 'zai-org/GLM-5.3',
      current_price: { input: 1, output: 2 },
      new_price: { input: 1.4, output: 4.4 },
      changed: true,
    },
    {
      catalog_model_id: 'catm2',
      model_id: 'moonshotai/Kimi-K3',
      current_price: { input: 3, output: 15 },
      new_price: { input: 3, output: 15 },
      changed: false,
    },
    {
      catalog_model_id: 'catm3',
      model_id: 'vendor/not-in-feed',
      current_price: null,
      new_price: null,
      changed: false,
    },
  ],
  unmatched_provider_models: 2,
}

function renderDialog(
  provider: CatalogProvider,
  opts: {
    credentials?: unknown[]
    previewResponse?: () => ReturnType<typeof jsonResponse>
    applyResponse?: () => ReturnType<typeof jsonResponse>
  } = {},
) {
  sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
  const calls = stubApi({
    'GET /credentials': () =>
      jsonResponse({ items: opts.credentials ?? [], total: (opts.credentials ?? []).length }),
    [`POST /model-catalog/providers/${provider.id}/prices/preview`]:
      opts.previewResponse ?? (() => jsonResponse(PREVIEW_RESULT)),
    [`POST /model-catalog/providers/${provider.id}/prices/apply`]:
      opts.applyResponse ?? (() => jsonResponse({ catalog_updated: 1, tenant_updated: 2 })),
  })
  const onOpenChange = vi.fn()
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={queryClient}>
      <RefreshPricesDialog provider={provider} onOpenChange={onOpenChange} />
    </QueryClientProvider>,
  )
  return { calls, onOpenChange }
}

describe('RefreshPricesDialog', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('fetches nebius prices with no key and shows the diff table', async () => {
    const { calls } = renderDialog(NEBIUS_PROVIDER)
    const user = userEvent.setup()

    expect(screen.getByText(/pricing feed is public/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Enter API key' })).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /fetch prices/i }))

    expect(await screen.findByText('zai-org/GLM-5.3')).toBeInTheDocument()
    expect(screen.getByText('vendor/not-in-feed')).toBeInTheDocument()
    expect(screen.getByText(/not found/i)).toBeInTheDocument()
    expect(screen.getByText(/2 models priced by nebius ai studio are not in the catalog/i)).toBeInTheDocument()

    const previewCall = calls.find((c) => c.path === '/model-catalog/providers/prov-nebius/prices/preview')
    expect(previewCall).toBeTruthy()
    expect(JSON.parse(previewCall!.body!)).toEqual({})

    // Only the one changed row (GLM-5.3) is apply-able.
    expect(screen.getByRole('button', { name: /apply 1 change/i })).toBeInTheDocument()
  })

  it('requires a key before fetching for together, and sends it as api_key', async () => {
    const { calls, onOpenChange } = renderDialog(TOGETHER_PROVIDER)
    const user = userEvent.setup()

    expect(screen.getByRole('button', { name: 'Enter API key' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /fetch prices/i }))
    // No key entered yet -- preview must not have been called.
    expect(calls.find((c) => c.path.includes('/prices/preview'))).toBeFalsy()

    await user.click(screen.getByRole('button', { name: 'Enter API key' }))
    await user.type(screen.getByLabelText('Together AI API key'), 'sk-together-test')
    await user.click(screen.getByRole('button', { name: /fetch prices/i }))

    await screen.findByText('zai-org/GLM-5.3')
    const previewCall = calls.find((c) => c.path === '/model-catalog/providers/prov-together/prices/preview')
    expect(JSON.parse(previewCall!.body!)).toEqual({ api_key: 'sk-together-test' })

    await user.click(screen.getByRole('button', { name: /apply 1 change/i }))
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false))

    const applyCall = calls.find((c) => c.path === '/model-catalog/providers/prov-together/prices/apply')
    expect(JSON.parse(applyCall!.body!)).toEqual({
      items: [{ catalog_model_id: 'catm1', price: { input: 1.4, output: 4.4 } }],
      update_tenant_models: true,
    })
  })

  it('uses an existing credential by name when one is on file', async () => {
    const { calls } = renderDialog(TOGETHER_PROVIDER, {
      credentials: [
        { name: 'together-api-key', id: 'c1', type: 'api-key', field_names: ['token'], key_id: 'k1', created_at: '2026-01-01T00:00:00Z' },
      ],
    })
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: /fetch prices/i }))
    await screen.findByText('zai-org/GLM-5.3')

    const previewCall = calls.find((c) => c.path === '/model-catalog/providers/prov-together/prices/preview')
    expect(JSON.parse(previewCall!.body!)).toEqual({ credential: 'together-api-key' })
  })

  it('omits the apply request body when "update tenant models" is unchecked', async () => {
    const { calls } = renderDialog(NEBIUS_PROVIDER)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: /fetch prices/i }))
    await screen.findByText('zai-org/GLM-5.3')

    await user.click(
      screen.getByRole('checkbox', { name: /also update tenant models that still use/i }),
    )
    await user.click(screen.getByRole('button', { name: /apply 1 change/i }))

    await waitFor(() =>
      expect(calls.find((c) => c.path === '/model-catalog/providers/prov-nebius/prices/apply')).toBeTruthy(),
    )
    const applyCall = calls.find((c) => c.path === '/model-catalog/providers/prov-nebius/prices/apply')
    expect(JSON.parse(applyCall!.body!)).toEqual({
      items: [{ catalog_model_id: 'catm1', price: { input: 1.4, output: 4.4 } }],
      update_tenant_models: false,
    })
  })

  it('disables Apply when nothing changed', async () => {
    renderDialog(NEBIUS_PROVIDER, {
      previewResponse: () =>
        jsonResponse({
          source_url: 'https://tokenfactory.nebius.com/api/public/models_info',
          fetched_at: '2026-10-01T00:00:00Z',
          items: [
            {
              catalog_model_id: 'catm1',
              model_id: 'zai-org/GLM-5.3',
              current_price: { input: 1.4, output: 4.4 },
              new_price: { input: 1.4, output: 4.4 },
              changed: false,
            },
          ],
          unmatched_provider_models: 0,
        }),
    })
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: /fetch prices/i }))
    await screen.findByText('zai-org/GLM-5.3')

    expect(screen.getByRole('button', { name: /apply 0 changes/i })).toBeDisabled()
  })
})
