import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, useLocation } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import ProfilesPage from '@/routes/ProfilesPage'
import { chooseRowAction } from './rowActions'

const PROFILE = {
  id: 'p1',
  name: 'SOC Agent Profile',
  slug: 'soc-agent',
  description: 'Handles SOC alerts',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function LocationProbe() {
  const location = useLocation()
  return <div data-testid="location-path">{location.pathname}</div>
}

function renderPage() {
  sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={['/profiles']}>
        <LocationProbe />
        <ProfilesPage />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('ProfilesPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('renders the profile list', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/profiles'))
          return jsonResponse({ items: [PROFILE], total: 1 })
        if (/\/api\/v1\/profiles\/[^/]+\/tools$/.test(url))
          return jsonResponse({ items: [], total: 0 })
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    renderPage()

    expect(await screen.findByText('SOC Agent Profile')).toBeInTheDocument()
    expect(screen.getByText('soc-agent')).toBeInTheDocument()
    expect(screen.getByText('Handles SOC alerts')).toBeInTheDocument()
  })

  it('submits the right body from the create dialog', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/profiles') && init?.method === undefined)
        return jsonResponse({ items: [], total: 0 })
      if (url.endsWith('/api/v1/profiles') && init?.method === 'POST')
        return jsonResponse({
          id: 'p2',
          name: 'New Profile',
          slug: 'new-profile',
          created_at: '2026-03-01T00:00:00Z',
          updated_at: '2026-03-01T00:00:00Z',
        })
      throw new Error(`Unhandled fetch in test: ${url} ${init?.method}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /create profile/i }))
    await user.type(screen.getByPlaceholderText(/SOC Agent Profile/i), 'New Profile')
    await user.click(screen.getByRole('button', { name: /^create profile$/i }))

    await waitFor(() => {
      const postCall = fetchMock.mock.calls.find(
        ([reqInput, init]) =>
          String(reqInput).endsWith('/api/v1/profiles') &&
          (init as RequestInit | undefined)?.method === 'POST',
      )
      expect(postCall).toBeTruthy()
      const body = JSON.parse((postCall![1] as RequestInit).body as string)
      expect(body).toEqual({ name: 'New Profile' })
    })
  })

  it('opens the tools page when Manage tools is chosen from the row menu', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/profiles'))
          return jsonResponse({ items: [PROFILE], total: 1 })
        if (/\/api\/v1\/profiles\/[^/]+\/tools$/.test(url))
          return jsonResponse({ items: [], total: 0 })
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    const user = userEvent.setup()
    renderPage()

    await chooseRowAction(user, 'SOC Agent Profile', 'Manage tools')

    expect(screen.getByTestId('location-path')).toHaveTextContent('/profiles/p1/tools')
  })

  it('sorts by tool count, which arrives after the rows', async () => {
    const totals: Record<string, number> = { p1: 5, p2: 50, p3: 1 }
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input.toString()
        if (url.endsWith('/api/v1/profiles'))
          return jsonResponse({
            items: [
              { ...PROFILE, id: 'p1', name: 'Alpha', slug: 'alpha' },
              { ...PROFILE, id: 'p2', name: 'Bravo', slug: 'bravo' },
              { ...PROFILE, id: 'p3', name: 'Charlie', slug: 'charlie' },
            ],
            total: 3,
          })
        const tools = /\/api\/v1\/profiles\/([^/]+)\/tools$/.exec(url)
        if (tools) return jsonResponse({ items: [], total: totals[tools[1]!] })
        throw new Error(`Unhandled fetch in test: ${url}`)
      }),
    )
    const user = userEvent.setup()
    renderPage()
    const names = () =>
      screen
        .getAllByRole('row')
        .slice(1)
        .map((row) => within(row).getAllByRole('cell')[0]?.textContent)

    await screen.findByText('50')
    await user.click(screen.getByRole('columnheader', { name: 'Tools' }))
    expect(names()).toEqual(['Charlie', 'Alpha', 'Bravo'])

    await user.click(screen.getByRole('columnheader', { name: 'Tools' }))
    expect(names()).toEqual(['Bravo', 'Alpha', 'Charlie'])
  })
})
