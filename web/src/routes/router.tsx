import type { ComponentType } from 'react'
import { createBrowserRouter, Navigate } from 'react-router-dom'
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
  '/tool-search': ToolSearchPage,
  '/cache': CachePage,
  '/api-keys': ApiKeysPage,
  '/credentials': CredentialsPage,
  '/access-logs': AccessLogsPage,
  '/llm-logs': LlmLogsPage,
  '/session-timeline': SessionTimelinePage,
  '/token-monitoring': TokenMonitoringPage,
}

const comingSoonItems = navGroups
  .flatMap((group) => group.items)
  .filter((item) => item.path !== OVERVIEW_PATH && !builtPages[item.path])

const builtItems = navGroups
  .flatMap((group) => group.items)
  .filter((item) => !!builtPages[item.path])

export const router = createBrowserRouter([
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
        children: [
          { index: true, element: <OverviewPage /> },
          ...builtItems.map((item) => ({
            path: item.path,
            element: (() => {
              const Page = builtPages[item.path]!
              return <Page />
            })(),
          })),
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
        ],
      },
    ],
  },
  {
    path: '*',
    element: <Navigate to={OVERVIEW_PATH} replace />,
  },
])
