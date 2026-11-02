import type { ComponentType } from 'react'
import { Navigate, type RouteObject } from 'react-router-dom'
import { RequireAuth } from '@/auth/RequireAuth'
import { AppShell } from '@/components/layout/AppShell'
import { ComingSoon } from '@/components/app/ComingSoon'
import { navGroups, OVERVIEW_PATH } from '@/lib/nav'
import DocsPage from '@/routes/DocsPage'
import LoginPage from '@/routes/LoginPage'
import OverviewPage from '@/routes/OverviewPage'
import ConnectorsPage from '@/routes/ConnectorsPage'
import ModelsPage from '@/routes/ModelsPage'
import ModelCatalogPage from '@/routes/ModelCatalogPage'
import ProfilesPage from '@/routes/ProfilesPage'
import ProfileToolsPage from '@/routes/ProfileToolsPage'
import SkillsPage from '@/routes/SkillsPage'
import ToolSearchPage from '@/routes/ToolSearchPage'
import UsersPage from '@/routes/UsersPage'
import TenantsPage from '@/routes/TenantsPage'
import ChangePasswordPage from '@/routes/ChangePasswordPage'
import CachePage from '@/routes/CachePage'
import ApiKeysPage from '@/routes/ApiKeysPage'
import CredentialsPage from '@/routes/CredentialsPage'
import AccessLogsPage from '@/routes/AccessLogsPage'
import LlmLogsPage from '@/routes/LlmLogsPage'
import SessionTimelinePage from '@/routes/SessionTimelinePage'
import TokenMonitoringPage from '@/routes/TokenMonitoringPage'
import TokenMonitoringDetailPage from '@/routes/TokenMonitoringDetailPage'

/** Phase-1 pages that replace their ComingSoon placeholder. Keyed by the
 * nav path so any path not listed here still falls back to ComingSoon. */
const builtPages: Record<string, ComponentType> = {
  '/connectors': ConnectorsPage,
  '/models': ModelsPage,
  '/model-catalog': ModelCatalogPage,
  '/profiles': ProfilesPage,
  '/skills': SkillsPage,
  '/users': UsersPage,
  '/tenants': TenantsPage,
  '/tool-search': ToolSearchPage,
  '/cache': CachePage,
  '/api-keys': ApiKeysPage,
  '/credentials': CredentialsPage,
  '/access-logs': AccessLogsPage,
  '/llm-logs': LlmLogsPage,
  '/session-timeline': SessionTimelinePage,
  '/token-monitoring': TokenMonitoringPage,
}

/**
 * The routes rendered inside the app shell: overview, every built page, the
 * detail pages and the coming-soon placeholders. Mount these under your own
 * layout or router (with a `basename`) when embedding the console.
 */
export function gatewayPageRoutes(): RouteObject[] {
  const items = navGroups.flatMap((group) => group.items)
  const builtItems = items.filter((item) => !!builtPages[item.path])
  const comingSoonItems = items.filter(
    (item) => item.path !== OVERVIEW_PATH && !builtPages[item.path],
  )

  return [
    { index: true, element: <OverviewPage /> },
    ...builtItems.map((item) => {
      const Page = builtPages[item.path]!
      return { path: item.path, element: <Page /> }
    }),
    { path: '/profiles/:id/tools', element: <ProfileToolsPage /> },
    {
      path: '/token-monitoring/model',
      element: <TokenMonitoringDetailPage kind="model" />,
    },
    {
      path: '/token-monitoring/keys/:id',
      element: <TokenMonitoringDetailPage kind="key" />,
    },
    ...comingSoonItems.map((item) => ({
      path: item.path,
      element: <ComingSoon title={item.label} description={item.description} />,
    })),
  ]
}

/** The full console route tree: login, docs, the authenticated app shell and
 * the catch-all redirect. This is what the embedded console mounts. */
export function gatewayRoutes(): RouteObject[] {
  return [
    {
      path: '/login',
      element: <LoginPage />,
    },
    {
      path: '/docs',
      element: <DocsPage />,
    },
    {
      element: <RequireAuth />,
      children: [
        // Standalone (no app shell): while a password change is forced this is
        // the only page a session may use.
        { path: '/change-password', element: <ChangePasswordPage /> },
        {
          element: <AppShell />,
          children: gatewayPageRoutes(),
        },
      ],
    },
    {
      path: '*',
      element: <Navigate to={OVERVIEW_PATH} replace />,
    },
  ]
}
