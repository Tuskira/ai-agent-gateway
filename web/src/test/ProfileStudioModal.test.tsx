import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { AuthProvider } from '@/auth/AuthContext'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import ProfilesPage from '@/routes/ProfilesPage'

const PRINCIPAL = {
  subject: 'avinash@tuskira.ai',
  tenant_id: 'tuskira-dev-tenant',
  email: 'avinash@tuskira.ai',
  roles: ['admin'],
  auth_method: 'api_key',
  key_id: 'key_123',
}

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

const TENANT_SKILL = {
  id: 'skill-1',
  name: 'incident-runbook',
  kind: 'skill',
  description: 'Standard incident triage steps.',
  frontmatter: {},
  arguments: [],
  latest_version: 3,
  enabled: true,
  metadata: {},
  scope: 'tenant',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
}

const PLATFORM_COMMAND = {
  id: 'skill-2',
  name: 'summarize',
  kind: 'command',
  description: 'Summarizes the current incident.',
  frontmatter: {},
  arguments: [],
  latest_version: 1,
  enabled: true,
  metadata: {},
  scope: 'platform',
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
      <AuthProvider>
        <MemoryRouter initialEntries={['/profiles']}>
          <ProfilesPage />
        </MemoryRouter>
      </AuthProvider>
    </QueryClientProvider>,
  )
}

describe('ProfileStudioModal', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('toggling a tool and saving PUTs the granted tool set', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString()
      if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
      if (url.endsWith('/api/v1/profiles'))
        return jsonResponse({ items: [PROFILE], total: 1 })
      if (url.endsWith('/api/v1/profiles/p1')) return jsonResponse(PROFILE)
      if (url.endsWith('/api/v1/connectors'))
        return jsonResponse({ items: [CONNECTOR], total: 1 })
      if (url.endsWith('/api/v1/connectors/c1/tools'))
        return jsonResponse({ items: [{ tool_name: 'search_indicators' }], total: 1 })
      if (url.endsWith('/api/v1/profiles/p1/tools') && init?.method === undefined)
        return jsonResponse({ items: [], total: 0 })
      if (url.endsWith('/api/v1/profiles/p1/tools') && init?.method === 'PUT')
        return jsonResponse({ items: [], total: 0 })
      if (url.endsWith('/api/v1/skills')) return jsonResponse({ items: [], total: 0 })
      if (url.endsWith('/api/v1/profiles/p1/skills'))
        return jsonResponse({ items: [], total: 0 })
      throw new Error(`Unhandled fetch in test: ${url} ${init?.method}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /profile studio/i }))

    // The first (only) tenant profile is auto-selected; its tools load.
    expect(await screen.findByText('search_indicators')).toBeInTheDocument()

    await user.click(screen.getByRole('checkbox'))
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

  function stubSkillsFetch(overrides: { instructionsPut?: unknown } = {}) {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString()
      const method = init?.method
      if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
      if (url.endsWith('/api/v1/profiles') && method === undefined)
        return jsonResponse({ items: [PROFILE], total: 1 })
      if (url.endsWith('/api/v1/profiles/p1') && method === undefined)
        return jsonResponse(PROFILE)
      if (url.endsWith('/api/v1/profiles/p1') && method === 'PUT')
        return jsonResponse(overrides.instructionsPut ?? PROFILE)
      if (url.endsWith('/api/v1/connectors'))
        return jsonResponse({ items: [], total: 0 })
      if (url.endsWith('/api/v1/skills') && method === undefined)
        return jsonResponse({ items: [TENANT_SKILL, PLATFORM_COMMAND], total: 2 })
      if (url.endsWith('/api/v1/profiles/p1/skills') && method === undefined)
        return jsonResponse({ items: [], total: 0 })
      if (url.endsWith('/api/v1/profiles/p1/skills') && method === 'PUT')
        return jsonResponse({ items: [], total: 0 })
      throw new Error(`Unhandled fetch in test: ${url} ${method}`)
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  it('renders the Skills & commands tab with kind and scope badges', async () => {
    stubSkillsFetch()
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /profile studio/i }))
    await user.click(await screen.findByRole('tab', { name: /skills & commands/i }))

    expect(await screen.findByText('incident-runbook')).toBeInTheDocument()
    expect(screen.getByText('summarize')).toBeInTheDocument()
    expect(screen.getAllByText('skill').length).toBeGreaterThan(0)
    expect(screen.getByText('command')).toBeInTheDocument()
    expect(screen.getByText('platform')).toBeInTheDocument()
  })

  it('checking a skill and saving PUTs the attached skill set', async () => {
    const fetchMock = stubSkillsFetch()
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /profile studio/i }))
    await user.click(await screen.findByRole('tab', { name: /skills & commands/i }))
    await screen.findByText('incident-runbook')

    await user.click(screen.getByRole('checkbox', { name: /grant incident-runbook/i }))
    await user.click(screen.getByRole('button', { name: /^save$/i }))

    await waitFor(() => {
      const putCall = fetchMock.mock.calls.find(
        ([reqInput, init]) =>
          String(reqInput).endsWith('/api/v1/profiles/p1/skills') &&
          (init as RequestInit | undefined)?.method === 'PUT',
      )
      expect(putCall).toBeTruthy()
      const body = JSON.parse((putCall![1] as RequestInit).body as string)
      expect(body).toEqual({ items: [{ skill_id: 'skill-1' }] })
    })
  })

  it('pinning a version includes it in the saved body', async () => {
    const fetchMock = stubSkillsFetch()
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /profile studio/i }))
    await user.click(await screen.findByRole('tab', { name: /skills & commands/i }))
    await screen.findByText('incident-runbook')

    await user.click(screen.getByRole('checkbox', { name: /grant incident-runbook/i }))
    await user.click(screen.getByRole('combobox', { name: /incident-runbook version/i }))
    await user.click(await screen.findByRole('option', { name: 'v2' }))
    await user.click(screen.getByRole('button', { name: /^save$/i }))

    await waitFor(() => {
      const putCall = fetchMock.mock.calls.find(
        ([reqInput, init]) =>
          String(reqInput).endsWith('/api/v1/profiles/p1/skills') &&
          (init as RequestInit | undefined)?.method === 'PUT',
      )
      expect(putCall).toBeTruthy()
      const body = JSON.parse((putCall![1] as RequestInit).body as string)
      expect(body).toEqual({ items: [{ skill_id: 'skill-1', version: 2 }] })
    })
  })

  it('shows an outdated badge for a stale pin and "Update to latest" clears the pin', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input.toString()
      const method = init?.method
      if (url.endsWith('/api/v1/auth/me')) return jsonResponse(PRINCIPAL)
      if (url.endsWith('/api/v1/profiles') && method === undefined)
        return jsonResponse({ items: [PROFILE], total: 1 })
      if (url.endsWith('/api/v1/profiles/p1') && method === undefined) return jsonResponse(PROFILE)
      if (url.endsWith('/api/v1/connectors')) return jsonResponse({ items: [], total: 0 })
      if (url.endsWith('/api/v1/skills') && method === undefined)
        // TENANT_SKILL.latest_version is 3 -- the registry's live view.
        return jsonResponse({ items: [TENANT_SKILL, PLATFORM_COMMAND], total: 2 })
      if (url.endsWith('/api/v1/profiles/p1/skills') && method === undefined)
        // Already attached, pinned at v1 -- behind the registry's v3.
        return jsonResponse({
          items: [
            {
              skill_id: 'skill-1',
              name: 'incident-runbook',
              kind: 'skill',
              version: 1,
              latest_version: 3,
              description: TENANT_SKILL.description,
            },
          ],
          total: 1,
        })
      if (url.endsWith('/api/v1/profiles/p1/skills') && method === 'PUT')
        return jsonResponse({ items: [], total: 0 })
      throw new Error(`Unhandled fetch in test: ${url} ${method}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /profile studio/i }))
    await user.click(await screen.findByRole('tab', { name: /skills & commands/i }))
    await screen.findByText('incident-runbook')

    expect(screen.getByText('Outdated')).toBeInTheDocument()
    expect(screen.getByRole('combobox', { name: /incident-runbook version/i })).toHaveTextContent(
      'v1',
    )

    await user.click(screen.getByRole('button', { name: /update to latest/i }))

    expect(screen.queryByText('Outdated')).not.toBeInTheDocument()
    expect(screen.getByRole('combobox', { name: /incident-runbook version/i })).toHaveTextContent(
      'Latest',
    )

    await user.click(screen.getByRole('button', { name: /^save$/i }))

    await waitFor(() => {
      const putCall = fetchMock.mock.calls.find(
        ([reqInput, init]) =>
          String(reqInput).endsWith('/api/v1/profiles/p1/skills') &&
          (init as RequestInit | undefined)?.method === 'PUT',
      )
      expect(putCall).toBeTruthy()
      const body = JSON.parse((putCall![1] as RequestInit).body as string)
      // No "version" key -- cleared back to "track latest".
      expect(body).toEqual({ items: [{ skill_id: 'skill-1' }] })
    })
  })

  it('saving instructions PUTs the full profile body', async () => {
    const fetchMock = stubSkillsFetch()
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /profile studio/i }))
    await user.click(await screen.findByRole('tab', { name: /^instructions$/i }))

    const textarea = await screen.findByRole('textbox', { name: /profile instructions/i })
    await user.type(textarea, 'Always check the allow list first.')
    await user.click(screen.getByRole('button', { name: /^save$/i }))

    await waitFor(() => {
      const putCall = fetchMock.mock.calls.find(
        ([reqInput, init]) =>
          String(reqInput).endsWith('/api/v1/profiles/p1') &&
          (init as RequestInit | undefined)?.method === 'PUT',
      )
      expect(putCall).toBeTruthy()
      const body = JSON.parse((putCall![1] as RequestInit).body as string)
      expect(body).toEqual({
        name: 'SOC Agent Profile',
        instructions: 'Always check the allow list first.',
      })
    })
  })

  it('enforces the 8 KiB instructions limit', async () => {
    stubSkillsFetch()
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /profile studio/i }))
    await user.click(await screen.findByRole('tab', { name: /^instructions$/i }))

    const textarea = (await screen.findByRole('textbox', {
      name: /profile instructions/i,
    })) as HTMLTextAreaElement

    const overLimit = 'a'.repeat(8 * 1024 + 1)
    fireEvent.change(textarea, { target: { value: overLimit } })

    // The edit is rejected outright: the textarea keeps its prior (empty)
    // value rather than accepting content past the limit.
    expect(textarea.value).toBe('')
    expect(screen.getByText('0 / 8192 bytes')).toBeInTheDocument()
  })
})
