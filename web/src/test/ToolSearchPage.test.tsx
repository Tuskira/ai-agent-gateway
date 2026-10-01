import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import ToolSearchPage from '@/routes/ToolSearchPage'

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
      <MemoryRouter initialEntries={['/tool-search']}>
        <ToolSearchPage />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('ToolSearchPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('shows an idle prompt, then renders results for a submitted query', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.startsWith('/api/v1/cache/search')) {
          expect(url).toContain('q=indicator')
          return jsonResponse({
            items: [
              {
                connector_id: 'c1',
                connector_name: 'opencti',
                tool_name: 'search_indicators',
                description: 'Search threat indicators',
              },
            ],
            total: 1,
          })
        }
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    const user = userEvent.setup()
    renderPage()

    expect(
      screen.getByText(/enter a search query to find tools/i),
    ).toBeInTheDocument()

    await user.type(
      screen.getByPlaceholderText(/search by tool name or description/i),
      'indicator',
    )
    await user.click(screen.getByRole('button', { name: /^search$/i }))

    expect(await screen.findByText('search_indicators')).toBeInTheDocument()
    expect(screen.getByText('opencti')).toBeInTheDocument()
    expect(screen.getByText('Search threat indicators')).toBeInTheDocument()
  })

  it('sends include_stale=true once the checkbox is checked', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.startsWith('/api/v1/cache/search')) {
          expect(url).toContain('include_stale=true')
          return jsonResponse({ items: [], total: 0 })
        }
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    const user = userEvent.setup()
    renderPage()

    await user.type(
      screen.getByPlaceholderText(/search by tool name or description/i),
      'indicator',
    )
    await user.click(screen.getByRole('checkbox', { name: /include stale results/i }))
    await user.click(screen.getByRole('button', { name: /^search$/i }))

    expect(await screen.findByText(/no matching tools/i)).toBeInTheDocument()
  })
})
