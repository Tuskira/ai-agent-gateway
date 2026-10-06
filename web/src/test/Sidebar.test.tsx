import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { Sidebar } from '@/components/layout/Sidebar'
import { setCsrfToken } from '@/lib/api'
import { navGroups } from '@/lib/nav'
import {
  SESSION_ADMIN,
  SESSION_VIEWER,
  errorResponse,
  jsonResponse,
  renderWithAuth,
  stubApi,
} from './authHelpers'

function renderSidebar(ui: React.ReactNode) {
  return renderWithAuth(<MemoryRouter initialEntries={['/models']}>{ui}</MemoryRouter>)
}

describe('Sidebar', () => {
  beforeEach(() => setCsrfToken(null))
  afterEach(() => vi.unstubAllGlobals())

  it('renders every nav group and every nav item as a link for an admin', async () => {
    stubApi({ 'GET /auth/me': () => jsonResponse(SESSION_ADMIN) })
    renderSidebar(<Sidebar />)

    // The Users link appears once the session is restored.
    await screen.findByRole('link', { name: 'Users' })
    for (const group of navGroups) {
      expect(screen.getAllByText(group.label).length).toBeGreaterThan(0)
      for (const item of group.items) {
        expect(screen.getByRole('link', { name: item.label })).toBeInTheDocument()
      }
    }
  })

  it('links Token Monitoring under Dashboards, below Overview, for every signed-in role', async () => {
    stubApi({ 'GET /auth/me': () => jsonResponse(SESSION_VIEWER) })
    renderSidebar(<Sidebar />)

    const link = await screen.findByRole('link', { name: 'Token Monitoring' })
    expect(link).toHaveAttribute('href', '/token-monitoring')
    expect(link.querySelector('svg.lucide-coins')).not.toBeNull()
    expect(screen.getByText('Dashboards')).toBeInTheDocument()
    const dashboards = navGroups.find((g) => g.label === 'Dashboards')
    expect(dashboards?.items.map((i) => i.path)).toEqual(['/', '/token-monitoring'])
  })

  it('hides Users from a viewer', async () => {
    stubApi({ 'GET /auth/me': () => jsonResponse(SESSION_VIEWER) })
    renderSidebar(<Sidebar />)

    await screen.findByRole('link', { name: 'Models' })
    // Let the principal resolve before asserting an absence.
    await new Promise((r) => setTimeout(r, 50))
    expect(screen.queryByRole('link', { name: 'Users' })).not.toBeInTheDocument()
  })

  it('hides Users when signed out', async () => {
    stubApi({
      'GET /auth/me': () => errorResponse(401, 'authentication_error', 'no session'),
    })
    renderSidebar(<Sidebar />)

    await screen.findByRole('link', { name: 'Models' })
    expect(screen.queryByRole('link', { name: 'Users' })).not.toBeInTheDocument()
  })

  it('labels collapsed items and shows the label in a tooltip on hover', async () => {
    stubApi({ 'GET /auth/me': () => jsonResponse(SESSION_ADMIN) })
    renderSidebar(<Sidebar collapsed />)

    const link = await screen.findByRole('link', { name: 'Models' })
    expect(link).not.toHaveAttribute('title')
    expect(link).toHaveAttribute('aria-current', 'page')
    expect(link).toHaveClass('justify-center', 'text-sidebar-primary')

    await userEvent.hover(link)
    expect(await screen.findByText('Models')).toBeInTheDocument()
  })
})
