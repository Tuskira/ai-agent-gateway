import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { ACTING_TENANT_HEADER, setActingTenant, setCsrfToken } from '@/lib/api'
import TenantsPage from '@/routes/TenantsPage'
import { chooseRowAction } from './rowActions'
import {
  CSRF,
  SESSION_ADMIN,
  SESSION_PLATFORM_ADMIN,
  TENANTS,
  jsonResponse,
  renderWithAuth,
  stubApi,
} from './authHelpers'

function renderPage() {
  return renderWithAuth(
    <MemoryRouter initialEntries={['/tenants']}>
      <TenantsPage />
    </MemoryRouter>,
  )
}

describe('TenantsPage', () => {
  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    setCsrfToken(null)
    setActingTenant(null)
  })
  afterEach(() => vi.unstubAllGlobals())

  it('lists every tenant platform-wide and marks the acting one', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_PLATFORM_ADMIN),
      'GET /tenants?limit=200': () => jsonResponse(TENANTS),
    })
    renderPage()

    expect(await screen.findByText('Globex')).toBeInTheDocument()
    expect(screen.getByText('Acme')).toBeInTheDocument()
    expect(screen.getByText('Initech')).toBeInTheDocument()
    expect(screen.getByText('globex')).toBeInTheDocument()
    expect(screen.getByText('tenant-uuid-2')).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Tenants (3)' })).toBeInTheDocument()
    expect(screen.getByText(/platform-wide/i)).toBeInTheDocument()
    expect(screen.getByText('Acting')).toBeInTheDocument()
  })

  it('creates a tenant with the slug auto-filled from the name', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_PLATFORM_ADMIN),
      'GET /tenants?limit=200': () => jsonResponse(TENANTS),
      'POST /tenants': () =>
        jsonResponse(
          {
            id: 'tenant-uuid-4',
            slug: 'umbrella-corp',
            name: 'Umbrella Corp',
            created_at: '2026-10-22T10:00:00Z',
          },
          201,
        ),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('Acme')
    await user.click(screen.getByRole('button', { name: /create tenant/i }))
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('Name'), 'Umbrella Corp')
    expect(within(dialog).getByLabelText('Slug')).toHaveValue('umbrella-corp')
    await user.click(within(dialog).getByRole('button', { name: /create tenant/i }))

    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    const post = calls.find((c) => c.method === 'POST' && c.path === '/tenants')!
    expect(JSON.parse(post.body!)).toEqual({
      slug: 'umbrella-corp',
      name: 'Umbrella Corp',
    })
    expect(post.headers['X-CSRF-Token']).toBe(CSRF)
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    // The list is refreshed after a create.
    expect(calls.filter((c) => c.path === '/tenants?limit=200').length).toBeGreaterThan(1)
  })

  it('rejects a bad slug and explains a duplicate slug inline', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_PLATFORM_ADMIN),
      'GET /tenants?limit=200': () => jsonResponse(TENANTS),
      'POST /tenants': () =>
        jsonResponse({ error: { type: 'conflict', message: 'slug taken' } }, 409),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('Acme')
    await user.click(screen.getByRole('button', { name: /create tenant/i }))
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('Name'), 'Acme')
    await user.clear(within(dialog).getByLabelText('Slug'))
    await user.type(within(dialog).getByLabelText('Slug'), 'Bad Slug')
    await user.click(within(dialog).getByRole('button', { name: /create tenant/i }))
    expect(
      await screen.findByText('Lowercase letters, digits and dashes only'),
    ).toBeInTheDocument()

    await user.clear(within(dialog).getByLabelText('Slug'))
    await user.type(within(dialog).getByLabelText('Slug'), 'acme')
    await user.click(within(dialog).getByRole('button', { name: /create tenant/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'A tenant with this slug already exists.',
    )
  })

  it('"Act in this tenant" switches the acting tenant', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_PLATFORM_ADMIN),
      'GET /tenants?limit=200': () => jsonResponse(TENANTS),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('Globex')
    await chooseRowAction(user, 'Globex', 'Act in this tenant')

    // The list is platform-wide and still refetched after the switch; the
    // header now names the chosen tenant.
    await waitFor(() => {
      const lists = calls.filter((c) => c.path === '/tenants?limit=200')
      expect(lists.at(-1)?.headers[ACTING_TENANT_HEADER]).toBe('tenant-uuid-2')
    })
    expect(localStorage.getItem('gateway.acting_tenant.ops')).toBe('tenant-uuid-2')
    // Once the list is back, the acting row is marked and no longer offers
    // "Act in this tenant", while the others still do.
    await screen.findByRole('button', { name: 'Actions for Acme' })
    await user.click(screen.getByRole('button', { name: 'Actions for Globex' }))
    expect(await screen.findByRole('menuitem', { name: 'Rename' })).toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: 'Act in this tenant' })).toBeNull()
    await user.keyboard('{Escape}')
    const globexRow = screen.getByText('Globex').closest('tr')!
    expect(within(globexRow).getByText('Acting')).toBeInTheDocument()
  })

  it('renames a tenant with PATCH and refreshes the list', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_PLATFORM_ADMIN),
      'GET /tenants?limit=200': () => jsonResponse(TENANTS),
      'PATCH /tenants/tenant-uuid-2': () =>
        jsonResponse({ ...TENANTS.items[1], name: 'Globex Inc' }),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('Globex')
    await chooseRowAction(user, 'Globex', 'Rename')
    const dialog = await screen.findByRole('dialog')
    const input = within(dialog).getByLabelText('Name')
    expect(input).toHaveValue('Globex')
    await user.clear(input)
    await user.type(input, 'Globex Inc')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(calls.some((c) => c.method === 'PATCH')).toBe(true))
    const patch = calls.find((c) => c.method === 'PATCH')!
    expect(patch.path).toBe('/tenants/tenant-uuid-2')
    expect(JSON.parse(patch.body!)).toEqual({ name: 'Globex Inc' })
    expect(patch.headers['X-CSRF-Token']).toBe(CSRF)
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(calls.filter((c) => c.path === '/tenants?limit=200').length).toBeGreaterThan(1)
  })

  it('shows the server error when a rename fails', async () => {
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_PLATFORM_ADMIN),
      'GET /tenants?limit=200': () => jsonResponse(TENANTS),
      'PATCH /tenants/tenant-uuid-2': () =>
        jsonResponse({ error: { type: 'not_found', message: 'tenant not found' } }, 404),
    })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('Globex')
    await chooseRowAction(user, 'Globex', 'Rename')
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('Name'), ' 2')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'This tenant no longer exists.',
    )
  })

  it('shows the platform-admin-required state to a tenant admin without calling /tenants', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
    })
    renderPage()

    expect(await screen.findByText('Platform admin required')).toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: /create tenant/i }),
    ).not.toBeInTheDocument()
    expect(calls.some((c) => c.path.startsWith('/tenants'))).toBe(false)
  })
})
