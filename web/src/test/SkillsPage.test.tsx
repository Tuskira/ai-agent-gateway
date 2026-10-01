import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import SkillsPage from '@/routes/SkillsPage'
import { chooseRowAction } from './rowActions'
import type { Skill, SkillDetail } from '@/lib/skills'

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

const platformSkill: Skill = {
  id: 'skill-platform',
  name: 'triage-runbook',
  kind: 'skill',
  description: 'Standard triage steps.',
  frontmatter: { name: 'triage-runbook', description: 'Standard triage steps.' },
  arguments: [],
  latest_version: 1,
  enabled: true,
  metadata: {},
  scope: 'platform',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-02T00:00:00Z',
}

const tenantCommand: Skill = {
  id: 'skill-tenant-1',
  name: 'summarize-incident',
  kind: 'command',
  description: 'Summarizes an incident.',
  frontmatter: { name: 'summarize-incident', description: 'Summarizes an incident.' },
  arguments: [{ name: 'incident_id', description: 'The incident to summarize', required: true }],
  latest_version: 2,
  enabled: true,
  metadata: {},
  scope: 'tenant',
  created_at: '2026-02-01T00:00:00Z',
  updated_at: '2026-02-05T00:00:00Z',
}

const disabledTenantSkill: Skill = {
  id: 'skill-tenant-2',
  name: 'legacy-helper',
  kind: 'skill',
  description: '',
  frontmatter: {},
  arguments: [],
  latest_version: 1,
  enabled: false,
  metadata: {},
  scope: 'tenant',
  created_at: '2026-01-15T00:00:00Z',
  updated_at: '2026-01-15T00:00:00Z',
}

function detailOf(skill: Skill): SkillDetail {
  return {
    ...skill,
    latest: {
      version: skill.latest_version,
      files: [
        {
          path: 'SKILL.md',
          content: `---\nname: ${skill.name}\ndescription: ${skill.description || 'd'}\n---\nBody`,
          sha256: 'abc',
          size: 10,
        },
      ],
    },
  }
}

function renderPage() {
  sessionStorage.setItem(API_KEY_STORAGE_KEY, 'gk_test_session_key')
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={['/skills']}>
        <SkillsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

function stubFetch(handlers: {
  list?: () => Response
  analytics?: () => Response
  detail?: (id: string) => Response
  versions?: (id: string) => Response
  deleteSkill?: (id: string) => Response
}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    const method = init?.method ?? 'GET'

    if (url.includes('/api/v1/analytics/skills')) {
      return handlers.analytics
        ? handlers.analytics()
        : jsonResponse({ range: '7d', skills: [], total_skills: 0, most_used: null })
    }
    if (url.match(/\/api\/v1\/skills(\?.*)?$/) && method === 'GET') {
      return handlers.list ? handlers.list() : jsonResponse({ items: [], total: 0 })
    }
    const versionsMatch = url.match(/\/api\/v1\/skills\/([^/]+)\/versions$/)
    if (versionsMatch && method === 'GET') {
      return handlers.versions
        ? handlers.versions(versionsMatch[1]!)
        : jsonResponse({ items: [], total: 0 })
    }
    const detailMatch = url.match(/\/api\/v1\/skills\/([^/]+)$/)
    if (detailMatch && method === 'GET') {
      return handlers.detail
        ? handlers.detail(detailMatch[1]!)
        : jsonResponse({ error: { type: 'not_found', message: 'no' } }, 404)
    }
    if (detailMatch && method === 'DELETE') {
      return handlers.deleteSkill ? handlers.deleteSkill(detailMatch[1]!) : jsonResponse(null, 204)
    }
    throw new Error(`Unhandled fetch in test: ${url} ${method}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('SkillsPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('shows a loading state, then the empty state when there are no skills', async () => {
    stubFetch({ list: () => jsonResponse({ items: [], total: 0 }) })
    renderPage()

    expect(await screen.findByText('No skills yet')).toBeInTheDocument()
    expect(
      screen.getByText(/add a skill or command, then attach it to a profile/i),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /add skill/i })).toBeInTheDocument()
  })

  it('shows an error state when the list fails', async () => {
    stubFetch({
      list: () => jsonResponse({ error: { type: 'error', message: 'boom' } }, 500),
    })
    renderPage()

    expect(await screen.findByText("Couldn't load skills.")).toBeInTheDocument()
  })

  it('lists skills and commands with kind, scope, state, and version columns', async () => {
    stubFetch({
      list: () =>
        jsonResponse({
          items: [platformSkill, tenantCommand, disabledTenantSkill],
          total: 3,
        }),
    })
    renderPage()

    expect(await screen.findByText('Skills (3)')).toBeInTheDocument()
    expect(screen.getByText('Total skills').closest('div')!.nextElementSibling).toHaveTextContent(
      '3',
    )
    // One of the three rows is a command.
    expect(screen.getByText('Commands').closest('div')!.nextElementSibling).toHaveTextContent('1')

    const table = screen.getByRole('table')
    const { getByText: getByTextInTable } = within(table)

    const platformRow = getByTextInTable('triage-runbook').closest('tr')!
    expect(platformRow).toHaveTextContent('platform')
    expect(platformRow).toHaveTextContent('v1')
    // Platform rows have no actions menu.
    expect(
      within(platformRow).queryByRole('button', {
        name: 'Actions for triage-runbook',
      }),
    ).not.toBeInTheDocument()

    const commandRow = getByTextInTable('summarize-incident').closest('tr')!
    expect(commandRow).toHaveTextContent('command')
    expect(commandRow).toHaveTextContent('v2')
    expect(commandRow).toHaveTextContent('Registered')

    const disabledRow = getByTextInTable('legacy-helper').closest('tr')!
    expect(disabledRow).toHaveTextContent('Disabled')
    expect(
      within(disabledRow).getByRole('button', { name: 'Actions for legacy-helper' }),
    ).toBeInTheDocument()
  })

  it('shows the analytics-off state when there are no skills and ClickHouse is off', async () => {
    stubFetch({
      list: () => jsonResponse({ items: [], total: 0 }),
      analytics: () => jsonResponse({ error: { type: 'not_found', message: 'off' } }, 404),
    })
    renderPage()

    expect(await screen.findByText(/analytics are turned off/i)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /view docs/i })).toHaveAttribute('href', '/docs')
    // Still actionable -- the header (with Add skill) stays above the notice.
    expect(screen.getByRole('button', { name: /add skill/i })).toBeInTheDocument()
  })

  it('shows Calls/Used by/Last seen usage columns and the Most used tile', async () => {
    stubFetch({
      list: () => jsonResponse({ items: [platformSkill, tenantCommand], total: 2 }),
      analytics: () =>
        jsonResponse({
          range: '7d',
          skills: [
            { name: 'triage-runbook', kind: 'skill', calls: 12, used_by: 3, last_seen: '2026-02-10T00:00:00Z' },
            { name: 'summarize-incident', kind: 'command', calls: 4, used_by: 1, last_seen: '2026-02-09T00:00:00Z' },
          ],
          total_skills: 2,
          most_used: { name: 'triage-runbook', kind: 'skill', calls: 12 },
        }),
    })
    renderPage()

    // Wait for the table's real rows (not the loading skeleton) before
    // asserting on the tiles -- both queries have settled by then, since
    // DataTable only leaves its skeleton once `isLoading` (which factors
    // in the analytics query too) goes false.
    const table = await screen.findByRole('table')
    const { getByText: getByTextInTable } = within(table)
    await within(table).findByText('triage-runbook')

    expect(screen.getByText('Most used (7d)').closest('div')!.nextElementSibling).toHaveTextContent(
      'triage-runbook',
    )
    expect(screen.getByText('12 calls')).toBeInTheDocument()

    const runbookRow = getByTextInTable('triage-runbook').closest('tr')!
    expect(runbookRow).toHaveTextContent('12')
    expect(runbookRow).toHaveTextContent('3 keys')

    const commandRow = getByTextInTable('summarize-incident').closest('tr')!
    expect(commandRow).toHaveTextContent('4')
    expect(commandRow).toHaveTextContent('1 key')
  })

  it('shows zeroed usage columns for a skill with no observed traffic', async () => {
    stubFetch({
      list: () => jsonResponse({ items: [disabledTenantSkill], total: 1 }),
      analytics: () =>
        jsonResponse({ range: '7d', skills: [], total_skills: 0, most_used: null }),
    })
    renderPage()

    const row = (await screen.findByText('legacy-helper')).closest('tr')!
    expect(row).toHaveTextContent('0')
    expect(row).toHaveTextContent('—')
  })

  it('opens the detail sheet on row click and shows latest files', async () => {
    stubFetch({
      list: () => jsonResponse({ items: [tenantCommand], total: 1 }),
      detail: () => jsonResponse(detailOf(tenantCommand)),
      versions: () =>
        jsonResponse({
          items: [{ version: 2, created_by: 'admin', created_at: '2026-02-05T00:00:00Z', file_count: 1 }],
          total: 1,
        }),
    })
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByText('summarize-incident'))

    expect(await screen.findByText('SKILL.md')).toBeInTheDocument()
    expect(screen.getByText('incident_id')).toBeInTheDocument()
    expect(screen.getAllByText('Summarizes an incident.').length).toBeGreaterThan(0)
  })

  it('deletes a tenant-scope skill after confirmation', async () => {
    const fetchMock = stubFetch({
      list: () => jsonResponse({ items: [disabledTenantSkill], total: 1 }),
    })
    const user = userEvent.setup()
    renderPage()

    await chooseRowAction(user, 'legacy-helper', 'Delete')
    await user.click(await screen.findByRole('button', { name: /^delete$/i }))

    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(
          ([reqInput, init]) =>
            String(reqInput).endsWith(`/api/v1/skills/${disabledTenantSkill.id}`) &&
            (init as RequestInit | undefined)?.method === 'DELETE',
        ),
      ).toBe(true)
    })
    // Choosing Delete must not also open the detail sheet.
    expect(screen.queryByText('SKILL.md')).not.toBeInTheDocument()
  })
})
