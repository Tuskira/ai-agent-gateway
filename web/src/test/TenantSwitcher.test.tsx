import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useQuery } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { useAuth } from '@/auth/AuthContext'
import { TopBar } from '@/components/layout/TopBar'
import { ACTING_TENANT_HEADER, apiFetch, setActingTenant, setCsrfToken } from '@/lib/api'
import {
  SESSION_ADMIN,
  SESSION_PLATFORM_ADMIN,
  TENANTS,
  jsonResponse,
  renderWithAuth,
  stubApi,
  type RecordedCall,
} from './authHelpers'

/** One tenant-scoped query next to the top bar, so the header it carries and
 * whether it was re-fetched after a switch can be asserted. Like every page
 * behind RequireAuth it waits for the session (and so the acting tenant) to
 * resolve before it fetches. */
function UsersProbe() {
  const { isLoading, principal } = useAuth()
  const users = useQuery({
    queryKey: ['users', 'list'],
    queryFn: () => apiFetch<{ items: { id: string }[] }>('/users'),
    enabled: !isLoading && principal !== null,
    retry: false,
  })
  return (
    <div data-testid="users">
      {users.isSuccess
        ? users.data.items.map((u) => u.id).join(',') || 'empty'
        : 'pending'}
    </div>
  )
}

/** A tenant-scoped query that never reads useAuth, like most route pages
 * (profiles, connectors, logs...). Nothing re-renders it on a switch, so it
 * only shows the new tenant's rows if the switch itself makes it refetch. */
function ConnectorsProbe() {
  const connectors = useQuery({
    queryKey: ['connectors', 'list'],
    queryFn: () => apiFetch<{ items: { id: string }[] }>('/connectors'),
    retry: false,
  })
  return (
    <div data-testid="connectors">
      {connectors.isSuccess ? connectors.data.items.map((c) => c.id).join(',') : 'pending'}
    </div>
  )
}

function renderTopBar(probe: ReactNode = <UsersProbe />) {
  return renderWithAuth(
    <MemoryRouter initialEntries={['/users']}>
      <TopBar collapsed={false} onToggleCollapsed={() => {}} onOpenMobileNav={() => {}} />
      {probe}
    </MemoryRouter>,
  )
}

function stubPlatform(): RecordedCall[] {
  return stubApi({
    'GET /auth/me': () => jsonResponse(SESSION_PLATFORM_ADMIN),
    'GET /tenants?limit=200': () => jsonResponse(TENANTS),
    'GET /users': (call) =>
      jsonResponse({
        // Each tenant answers with its own rows, keyed by the header.
        items: [{ id: `user-of-${call.headers[ACTING_TENANT_HEADER] ?? 'none'}` }],
      }),
  })
}

describe('TenantSwitcher', () => {
  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    setCsrfToken(null)
    setActingTenant(null)
  })
  afterEach(() => vi.unstubAllGlobals())

  it('is not rendered for a plain admin, who keeps the static Tenant label', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_ADMIN),
      'GET /users': () => jsonResponse({ items: [] }),
    })
    renderTopBar()

    await screen.findByText('empty')
    expect(screen.getByText('Tenant')).toBeInTheDocument()
    expect(screen.queryByTestId('tenant-switcher')).not.toBeInTheDocument()
    expect(calls.some((c) => c.path.startsWith('/tenants'))).toBe(false)
    expect(screen.getByTestId('tenant-id-pill')).toHaveTextContent('tenant-uuid-1')
  })

  it('names the acting tenant and lists every tenant with a short id and a count', async () => {
    stubPlatform()
    const user = userEvent.setup()
    renderTopBar()

    const trigger = await screen.findByTestId('tenant-switcher')
    await waitFor(() => expect(trigger).toHaveTextContent('Acme'))
    expect(screen.queryByText('Tenant')).not.toBeInTheDocument()

    await user.click(trigger)
    const list = await screen.findByRole('listbox')
    for (const t of TENANTS.items) {
      const option = within(list).getByRole('option', { name: new RegExp(t.name) })
      expect(option).toHaveTextContent(t.id.slice(0, 8))
    }
    expect(within(list).getByRole('option', { name: /acme/i })).toHaveAttribute(
      'data-checked',
      'true',
    )
    expect(screen.getByTestId('tenant-switcher-footer')).toHaveTextContent('3 tenants')
  })

  it('filters by name and by id', async () => {
    stubPlatform()
    const user = userEvent.setup()
    renderTopBar()

    await user.click(await screen.findByTestId('tenant-switcher'))
    const input = await screen.findByRole('combobox', { name: 'Search tenants' })

    await user.type(input, 'glob')
    await waitFor(() =>
      expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual([
        'Globextenant-u',
      ]),
    )

    await user.clear(input)
    await user.type(input, 'tenant-uuid-3')
    await waitFor(() =>
      expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual([
        'Initechtenant-u',
      ]),
    )

    await user.clear(input)
    await user.type(input, 'nothing-like-this')
    expect(await screen.findByText('No tenant matches.')).toBeInTheDocument()
  })

  it('selecting a tenant sends its id on the next call and drops the old tenant data', async () => {
    const calls = stubPlatform()
    const user = userEvent.setup()
    renderTopBar()

    expect(await screen.findByText('user-of-tenant-uuid-1')).toBeInTheDocument()
    await user.click(screen.getByTestId('tenant-switcher'))
    await user.click(await screen.findByRole('option', { name: /globex/i }))

    // The users query was removed from the cache and re-fetched under the
    // new tenant; its rows are the new tenant's.
    expect(await screen.findByText('user-of-tenant-uuid-2')).toBeInTheDocument()
    expect(screen.queryByText('user-of-tenant-uuid-1')).not.toBeInTheDocument()
    const userCalls = calls.filter((c) => c.path === '/users')
    expect(userCalls.map((c) => c.headers[ACTING_TENANT_HEADER])).toEqual([
      'tenant-uuid-1',
      'tenant-uuid-2',
    ])

    // The trigger, the pill and storage all follow the selection.
    expect(screen.getByTestId('tenant-switcher')).toHaveTextContent('Globex')
    expect(screen.getByTestId('tenant-id-pill')).toHaveTextContent('tenant-uuid-2')
    expect(localStorage.getItem('gateway.acting_tenant.ops')).toBe('tenant-uuid-2')
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
  })

  it('refetches a mounted query that does not read useAuth, so no page keeps the old tenant', async () => {
    const calls = stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_PLATFORM_ADMIN),
      'GET /tenants?limit=200': () => jsonResponse(TENANTS),
      'GET /connectors': (call) =>
        jsonResponse({ items: [{ id: `c-of-${call.headers[ACTING_TENANT_HEADER] ?? 'none'}` }] }),
    })
    const user = userEvent.setup()
    renderTopBar(<ConnectorsProbe />)

    // The probe fetches as soon as it mounts, like a page would.
    await screen.findByText(/^c-of-/)
    await screen.findByTestId('tenant-switcher')
    await user.click(screen.getByTestId('tenant-switcher'))
    await user.click(await screen.findByRole('option', { name: /globex/i }))

    // Nothing re-rendered the probe, yet the switch reset its query and the
    // refetch carried the new tenant's header.
    expect(await screen.findByText('c-of-tenant-uuid-2')).toBeInTheDocument()
    const connectorCalls = calls.filter((c) => c.path === '/connectors')
    expect(connectorCalls).toHaveLength(2)
    expect(connectorCalls[1]!.headers[ACTING_TENANT_HEADER]).toBe('tenant-uuid-2')
  })

  it('shows the short id when the acting tenant is not in the list', async () => {
    localStorage.setItem('gateway.acting_tenant.ops', 'tenant-uuid-3')
    stubApi({
      'GET /auth/me': () => jsonResponse(SESSION_PLATFORM_ADMIN),
      'GET /tenants?limit=200': () =>
        jsonResponse({ items: [TENANTS.items[2]], total: 1 }),
      'GET /users': () => jsonResponse({ items: [] }),
    })
    renderTopBar()

    const trigger = await screen.findByTestId('tenant-switcher')
    await waitFor(() => expect(trigger).toHaveTextContent('Initech'))
  })
})
