import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import ProfileToolsPage from '@/routes/ProfileToolsPage'

const PROFILE = {
  id: 'p1',
  name: 'SOC Agent Profile',
  slug: 'soc-agent',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
}

const CONNECTOR = {
  id: 'c1',
  name: 'opencti',
  slug: 'opencti',
  endpoint: 'http://opencti-mcp:8001/mcp',
  timeout_ms: 30000,
  status: 'healthy',
  metadata: {},
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
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
      <MemoryRouter initialEntries={['/profiles/p1/tools']}>
        <Routes>
          <Route path="/profiles/:id/tools" element={<ProfileToolsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('ProfileToolsPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('renders connector tools on the left and lets you select one for the profile', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/profiles/p1')) return jsonResponse(PROFILE)
      if (url.endsWith('/api/v1/connectors'))
        return jsonResponse({ items: [CONNECTOR], total: 1 })
      if (url.endsWith('/api/v1/connectors/c1/tools'))
        return jsonResponse({ items: [{ tool_name: 'search_indicators' }], total: 1 })
      if (url.endsWith('/api/v1/profiles/p1/tools') && init?.method === undefined)
        return jsonResponse({ items: [], total: 0 })
      if (url.endsWith('/api/v1/profiles/p1/tools') && init?.method === 'PUT')
        return jsonResponse({ items: [], total: 0 })
      throw new Error(`Unhandled fetch in test: ${url} ${init?.method}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()

    expect(await screen.findByText('search_indicators')).toBeInTheDocument()
    expect(screen.getAllByText('SOC Agent Profile').length).toBeGreaterThan(0)

    await user.click(screen.getByRole('checkbox'))
    expect(await screen.findByText('Selected tools (1)')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /^save$/i }))

    await waitFor(() => {
      const putCall = fetchMock.mock.calls.find(
        ([reqInput, init]) =>
          String(reqInput).endsWith('/api/v1/profiles/p1/tools') &&
          (init as RequestInit | undefined)?.method === 'PUT',
      )
      expect(putCall).toBeTruthy()
      const body = JSON.parse((putCall![1] as RequestInit).body as string)
      expect(body).toEqual({
        tools: [{ connector_id: 'c1', tool_name: 'search_indicators' }],
      })
    })
  })
})
