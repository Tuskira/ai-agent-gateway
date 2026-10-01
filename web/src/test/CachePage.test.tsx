import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import CachePage from '@/routes/CachePage'

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
      <MemoryRouter initialEntries={['/cache']}>
        <CachePage />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('CachePage', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('renders a card per connector with tool count, last-cached and actions', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/cache/stats')) {
          return jsonResponse({
            connectors: [
              {
                connector_id: 'c1',
                name: 'opencti',
                tools: 12,
                stale: 2,
                cached_at: '2026-01-01T00:00:00Z',
                expires_at: '2026-01-02T00:00:00Z',
              },
            ],
            total_tools: 12,
            stale_tools: 2,
          })
        }
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()

    expect(await screen.findByText('opencti')).toBeInTheDocument()
    expect(screen.getByText('12')).toBeInTheDocument()
    expect(screen.getByText(/last cached/i)).toBeInTheDocument()
    expect(screen.getByText(/avg fetch/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^refresh$/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^clear$/i })).toBeInTheDocument()
  })

  it('shows a not-available state when the cache endpoint 404s', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/cache/stats')) {
          return jsonResponse({ error: { type: 'not_found', message: 'not found' } }, 404)
        }
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()

    expect(await screen.findByText(/cache isn.t available yet/i)).toBeInTheDocument()
  })
})
