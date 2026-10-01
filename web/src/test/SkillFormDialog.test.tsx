import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { API_KEY_STORAGE_KEY } from '@/lib/api'
import SkillsPage from '@/routes/SkillsPage'

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function stubApi() {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    const method = init?.method ?? 'GET'

    if (url.match(/\/api\/v1\/skills(\?.*)?$/) && method === 'GET') {
      return jsonResponse({ items: [], total: 0 })
    }
    if (url.includes('/api/v1/analytics/skills')) {
      return jsonResponse({ range: '7d', skills: [], total_skills: 0, most_used: null })
    }
    if (url.endsWith('/api/v1/skills') && method === 'POST') {
      return jsonResponse(
        {
          id: 'new-skill-id',
          name: 'my-skill',
          kind: 'skill',
          description: '',
          frontmatter: {},
          arguments: [],
          latest_version: 1,
          enabled: true,
          metadata: {},
          scope: 'tenant',
          created_at: '2026-03-01T00:00:00Z',
          updated_at: '2026-03-01T00:00:00Z',
        },
        201,
      )
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
  return call ? (JSON.parse(call[1]!.body as string) as Record<string, unknown>) : undefined
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

describe('SkillFormDialog (Add skill/command)', () => {
  beforeEach(() => {
    sessionStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('prefills SKILL.md from the typed name and creates a skill', async () => {
    const fetchMock = stubApi()
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add skill/i }))
    await user.type(screen.getByPlaceholderText('e.g. incident-runbook'), 'my-skill')

    const skillMdField = screen.getByLabelText('SKILL.md content') as HTMLTextAreaElement
    expect(skillMdField.value).toContain('name: my-skill')

    await user.click(screen.getByRole('button', { name: /^create$/i }))

    await waitFor(() => {
      const body = sentBody(fetchMock, 'POST', '/api/v1/skills')
      expect(body).toMatchObject({
        name: 'my-skill',
        kind: 'skill',
      })
    })
    const body = sentBody(fetchMock, 'POST', '/api/v1/skills')!
    expect(body.files).toEqual([
      { path: 'SKILL.md', content: expect.stringContaining('name: my-skill') },
    ])
    // description is derived server-side from the SKILL.md frontmatter --
    // the dialog has no field for it and must never send one.
    expect(body).not.toHaveProperty('description')
  })

  it('has no separate Description field -- the frontmatter is the only source', async () => {
    stubApi()
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add skill/i }))

    expect(screen.queryByLabelText('Description')).not.toBeInTheDocument()
    expect(screen.queryByPlaceholderText('What is this used for?')).not.toBeInTheDocument()
  })

  it('rejects an invalid name before submitting', async () => {
    const fetchMock = stubApi()
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add skill/i }))
    await user.type(screen.getByPlaceholderText('e.g. incident-runbook'), 'My Skill!')
    await user.click(screen.getByRole('button', { name: /^create$/i }))

    expect(
      await screen.findByText(/lowercase letters, numbers, dot, underscore, and dash only/i),
    ).toBeInTheDocument()
    expect(sentBody(fetchMock, 'POST', '/api/v1/skills')).toBeUndefined()
  })

  it('stops auto-filling SKILL.md once the person edits it directly', async () => {
    stubApi()
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add skill/i }))
    const skillMd = screen.getByLabelText('SKILL.md content')
    await user.clear(skillMd)
    await user.type(skillMd, '---\nname: custom\ndescription: d\n---\nbody')

    await user.type(screen.getByPlaceholderText('e.g. incident-runbook'), 'my-skill')

    // The name field changed but the manually-edited content did not.
    expect(skillMd).toHaveValue('---\nname: custom\ndescription: d\n---\nbody')
  })

  it('rejects frontmatter whose "name" does not match the skill name', async () => {
    const fetchMock = stubApi()
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add skill/i }))
    await user.type(screen.getByPlaceholderText('e.g. incident-runbook'), 'my-skill')

    const skillMd = screen.getByLabelText('SKILL.md content')
    await user.clear(skillMd)
    await user.type(skillMd, '---{Enter}name: other{Enter}description: d{Enter}---{Enter}body')

    await user.click(screen.getByRole('button', { name: /^create$/i }))

    await waitFor(() => {
      expect(sentBody(fetchMock, 'POST', '/api/v1/skills')).toBeUndefined()
    })
  })

  it('rejects the "hooks" frontmatter key', async () => {
    const fetchMock = stubApi()
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add skill/i }))
    await user.type(screen.getByPlaceholderText('e.g. incident-runbook'), 'my-skill')

    const skillMd = screen.getByLabelText('SKILL.md content')
    await user.clear(skillMd)
    await user.type(
      skillMd,
      '---{Enter}name: my-skill{Enter}description: d{Enter}hooks: evil{Enter}---{Enter}body',
    )

    await user.click(screen.getByRole('button', { name: /^create$/i }))

    await waitFor(() => {
      expect(sentBody(fetchMock, 'POST', '/api/v1/skills')).toBeUndefined()
    })
  })

  it('for a command, requires every {{placeholder}} to be a declared argument', async () => {
    const fetchMock = stubApi()
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add skill/i }))
    await user.type(screen.getByPlaceholderText('e.g. incident-runbook'), 'my-command')
    await user.click(screen.getByRole('radio', { name: 'Command' }))

    const skillMd = screen.getByLabelText('SKILL.md content')
    await user.clear(skillMd)
    await user.type(
      skillMd,
      '---{Enter}name: my-command{Enter}description: d{Enter}---{Enter}Sev: {{{{severity}}',
    )

    // No arguments declared yet -- submitting should fail.
    await user.click(screen.getByRole('button', { name: /^create$/i }))
    await waitFor(() => {
      expect(sentBody(fetchMock, 'POST', '/api/v1/skills')).toBeUndefined()
    })

    // Declare it and retry.
    await user.click(screen.getByRole('button', { name: /add argument/i }))
    await user.type(screen.getByLabelText('Argument 1 name'), 'severity')

    await user.click(screen.getByRole('button', { name: /^create$/i }))

    await waitFor(() => {
      const body = sentBody(fetchMock, 'POST', '/api/v1/skills')
      expect(body).toMatchObject({
        kind: 'command',
        arguments: [{ name: 'severity', required: false }],
      })
    })
  })

  it('rejects an invalid argument name for a command', async () => {
    const fetchMock = stubApi()
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: /add skill/i }))
    await user.type(screen.getByPlaceholderText('e.g. incident-runbook'), 'my-command')
    await user.click(screen.getByRole('radio', { name: 'Command' }))

    const skillMd = screen.getByLabelText('SKILL.md content')
    await user.clear(skillMd)
    await user.type(skillMd, '---{Enter}name: my-command{Enter}description: d{Enter}---{Enter}body')

    await user.click(screen.getByRole('button', { name: /add argument/i }))
    await user.type(screen.getByLabelText('Argument 1 name'), 'Bad Name')

    await user.click(screen.getByRole('button', { name: /^create$/i }))

    expect(
      await screen.findByText(/lowercase letters, numbers, and underscore only/i),
    ).toBeInTheDocument()
    expect(sentBody(fetchMock, 'POST', '/api/v1/skills')).toBeUndefined()
  })
})
