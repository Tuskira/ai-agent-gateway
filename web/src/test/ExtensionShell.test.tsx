import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import { Plug } from 'lucide-react'
import { MemoryRouter } from 'react-router-dom'
import { Sidebar } from '@/components/layout/Sidebar'
import { ExtensionProvider } from '@/components/layout/ExtensionProvider'
import { setCsrfToken } from '@/lib/api'
import { SESSION_ADMIN, jsonResponse, renderWithAuth, stubApi } from './authHelpers'

const icon = Plug
const extension = {
  navGroups: [
    {
      label: 'Billing',
      before: 'Observe',
      items: [{ label: 'Invoices', path: '/invoices', icon, description: 'x' }],
    },
    {
      label: 'Access',
      items: [{ label: 'Audit', path: '/audit', icon, description: 'x', end: true }],
    },
  ],
}

describe('Sidebar inside an ExtensionProvider', () => {
  beforeEach(() => setCsrfToken(null))
  afterEach(() => vi.unstubAllGlobals())

  it('shows the extension groups and items next to the core ones', async () => {
    stubApi({ 'GET /auth/me': () => jsonResponse(SESSION_ADMIN) })
    renderWithAuth(
      <MemoryRouter initialEntries={['/models']}>
        <ExtensionProvider extension={extension}>
          <Sidebar />
        </ExtensionProvider>
      </MemoryRouter>,
    )
    expect(await screen.findByRole('link', { name: 'Invoices' })).toHaveAttribute(
      'href',
      '/invoices',
    )
    expect(screen.getByRole('link', { name: 'Audit' })).toBeInTheDocument()
    expect(screen.getByText('Billing')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Models' })).toBeInTheDocument()
  })

  it('shows only the core nav without a provider', async () => {
    stubApi({ 'GET /auth/me': () => jsonResponse(SESSION_ADMIN) })
    renderWithAuth(
      <MemoryRouter initialEntries={['/models']}>
        <Sidebar />
      </MemoryRouter>,
    )
    await screen.findByRole('link', { name: 'Users' })
    expect(screen.queryByRole('link', { name: 'Invoices' })).not.toBeInTheDocument()
  })
})
