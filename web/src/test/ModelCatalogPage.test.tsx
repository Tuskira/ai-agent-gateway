import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ModelCatalogPage from '@/routes/ModelCatalogPage'
import { chooseRowAction } from './rowActions'
import { AUTH_CONFIG, jsonResponse, renderWithAuth, SESSION_ADMIN, stubApi } from './authHelpers'

const CATALOG = {
  providers: [
    {
      id: 'prov1',
      slug: 'nebius',
      display_name: 'Nebius AI Studio',
      vendor: 'openai_compat',
      base_url: 'https://api.tokenfactory.eu-west2.nebius.com/v1',
      docs_url: 'https://docs.nebius.com',
      enabled: true,
      models: [
        {
          id: 'catm1',
          model_id: 'zai-org/GLM-5.3',
          display_name: 'GLM 5.3',
          suggested_name: 'glm-5.3-nebius',
          price: null,
          capabilities: { tools: true, vision: null, streaming: null, max_context: null },
          notes: 'Multi-turn tool use through Chat Completions translation is not supported yet.',
          enabled: true,
          tenant_model_id: null,
          tenant_model_name: null,
        },
      ],
    },
  ],
}

function renderPage() {
  return renderWithAuth(<ModelCatalogPage />)
}

describe('ModelCatalogPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('shows the admin-required notice on a 403 from GET /model-catalog', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /auth/config': () => jsonResponse(AUTH_CONFIG),
      'GET /model-catalog': () =>
        jsonResponse({ error: { type: 'forbidden', message: 'nope' } }, 403),
    })
    renderPage()

    await waitFor(() => expect(screen.getByText(/admin access required/i)).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: /add provider/i })).not.toBeInTheDocument()
  })

  it('lets a single-tenant session admin manage the catalog', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /auth/config': () => jsonResponse({ ...AUTH_CONFIG, single_tenant: true }),
      'GET /model-catalog': () => jsonResponse(CATALOG),
    })
    const user = userEvent.setup()
    renderPage()

    // Two independent queries (auth config + catalog) settle at different
    // times, each triggering its own re-render -- check both together in
    // one waitFor so a retry re-queries both rather than trusting a handle
    // from whichever query happened to settle first.
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /add provider/i })).toBeInTheDocument()
      expect(screen.getByText('Nebius AI Studio')).toBeInTheDocument()
    })
    expect(screen.queryByText(/only platform admins can change the catalog/i)).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Toggle row details' }))
    await waitFor(() => expect(screen.getByText('glm-5.3-nebius')).toBeInTheDocument())
    expect(screen.getByRole('button', { name: /add model/i })).toBeInTheDocument()
    expect(
      screen.getByText(/multi-turn tool use through chat completions translation/i),
    ).toBeInTheDocument()
  })

  it('is read-only for a session admin on a multi-tenant gateway', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /auth/config': () => jsonResponse({ ...AUTH_CONFIG, single_tenant: false }),
      'GET /model-catalog': () => jsonResponse(CATALOG),
    })
    renderPage()

    // Both queries (auth config, catalog) must have settled before either
    // assertion is meaningful -- check them together in one waitFor so a
    // retry re-queries both rather than trusting a handle from a separate,
    // earlier-settled query.
    await waitFor(() => {
      expect(screen.getByText(/only platform admins can change the catalog/i)).toBeInTheDocument()
      expect(screen.getByText('Nebius AI Studio')).toBeInTheDocument()
    })
    expect(screen.queryByRole('button', { name: /add provider/i })).not.toBeInTheDocument()
  })

  it('confirms with a usage count before force-deleting a provider still in use', async () => {
    let deleteCalls = 0
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /auth/config': () => jsonResponse({ ...AUTH_CONFIG, single_tenant: true }),
      'GET /model-catalog': () => jsonResponse(CATALOG),
      'DELETE /model-catalog/providers/prov1': () => {
        deleteCalls++
        if (deleteCalls === 1) {
          return jsonResponse({ error: { type: 'conflict', message: 'in use' }, tenant_models: 2 }, 409)
        }
        return new Response(null, { status: 204 })
      },
      'DELETE /model-catalog/providers/prov1?force=true': () => {
        deleteCalls++
        return new Response(null, { status: 204 })
      },
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('Nebius AI Studio')
    await chooseRowAction(user, 'Nebius AI Studio', 'Delete')
    await user.click(await screen.findByRole('button', { name: /^delete$/i }))

    expect(await screen.findByText(/used by 2 tenant models\. delete anyway\?/i)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /delete anyway/i }))

    await waitFor(() => expect(deleteCalls).toBe(2))
  })
})
