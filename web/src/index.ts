/**
 * Public surface of `@tuskira/gateway-web`. Only what is exported here is
 * supported; deep imports into `src/` are not. Import `@tuskira/gateway-web/style.css`
 * once (and optionally `/fonts.css`), call `configureGateway` before rendering, and
 * mount `gatewayPageRoutes()` inside your own router.
 */

// Compiled into dist-lib/style.css (Tailwind utilities + design tokens)
import './index.css'

// Embedding configuration and API client
export * from './lib/api'
export * from './lib/queries'
export * from './lib/types'

// Auth
export * from './auth/AuthContext'
export * from './auth/RequireAuth'

// Navigation and routes
export { navGroups, navItems, findNavItem, OVERVIEW_PATH } from './lib/nav'
export type { NavItem, NavGroup } from './lib/nav'
export { gatewayRoutes, gatewayPageRoutes } from './routes/gateway-routes'

// Pages
export { default as AccessLogsPage } from './routes/AccessLogsPage'
export { default as ApiKeysPage } from './routes/ApiKeysPage'
export { default as CachePage } from './routes/CachePage'
export { default as ChangePasswordPage } from './routes/ChangePasswordPage'
export { default as ConnectorsPage } from './routes/ConnectorsPage'
export { default as CredentialsPage } from './routes/CredentialsPage'
export { default as DocsPage } from './routes/DocsPage'
export { default as LlmLogsPage } from './routes/LlmLogsPage'
export { default as LoginPage } from './routes/LoginPage'
export { default as ModelCatalogPage } from './routes/ModelCatalogPage'
export { default as ModelsPage } from './routes/ModelsPage'
export { default as OverviewPage } from './routes/OverviewPage'
export { default as ProfilesPage } from './routes/ProfilesPage'
export { default as ProfileToolsPage } from './routes/ProfileToolsPage'
export { default as SessionTimelinePage } from './routes/SessionTimelinePage'
export { default as SkillsPage } from './routes/SkillsPage'
export { default as TenantsPage } from './routes/TenantsPage'
export { default as TokenMonitoringDetailPage } from './routes/TokenMonitoringDetailPage'
export { default as TokenMonitoringPage } from './routes/TokenMonitoringPage'
export { default as ToolSearchPage } from './routes/ToolSearchPage'
export { default as UsersPage } from './routes/UsersPage'

// Layout
export { AppShell } from './components/layout/AppShell'
export { Sidebar } from './components/layout/Sidebar'
export { TopBar } from './components/layout/TopBar'
export { TenantSwitcher } from './components/layout/TenantSwitcher'
export { ThemeToggle } from './components/layout/ThemeToggle'

// Data table
export * from './components/app/data-table'

// Design tokens
export * from './styles/tokens'

// shadcn/ui components
export * from './components/ui/accordion'
export * from './components/ui/alert-dialog'
export * from './components/ui/alert'
export * from './components/ui/aspect-ratio'
export * from './components/ui/attachment'
export * from './components/ui/avatar'
export * from './components/ui/badge'
export * from './components/ui/breadcrumb'
export * from './components/ui/bubble'
export * from './components/ui/button-group'
export * from './components/ui/button'
export * from './components/ui/calendar'
export * from './components/ui/card'
export * from './components/ui/carousel'
export * from './components/ui/chart'
export * from './components/ui/checkbox'
export * from './components/ui/collapsible'
export * from './components/ui/combobox'
export * from './components/ui/command'
export * from './components/ui/context-menu'
export * from './components/ui/dialog'
export * from './components/ui/direction'
export * from './components/ui/drawer'
export * from './components/ui/dropdown-menu'
export * from './components/ui/empty'
export * from './components/ui/field'
export * from './components/ui/form'
export * from './components/ui/hover-card'
export * from './components/ui/input-group'
export * from './components/ui/input-otp'
export * from './components/ui/input'
export * from './components/ui/item'
export * from './components/ui/kbd'
export * from './components/ui/label'
export * from './components/ui/marker'
export * from './components/ui/menubar'
export * from './components/ui/message-scroller'
export * from './components/ui/message'
export * from './components/ui/native-select'
export * from './components/ui/navigation-menu'
export * from './components/ui/pagination'
export * from './components/ui/popover'
export * from './components/ui/progress'
export * from './components/ui/questionnaire'
export * from './components/ui/radio-group'
export * from './components/ui/resizable'
export * from './components/ui/scroll-area'
export * from './components/ui/select'
export * from './components/ui/separator'
export * from './components/ui/sheet'
export * from './components/ui/sidebar'
export * from './components/ui/skeleton'
export * from './components/ui/slider'
export * from './components/ui/sonner'
export * from './components/ui/spinner'
export * from './components/ui/switch'
export * from './components/ui/table'
export * from './components/ui/tabs'
export * from './components/ui/textarea'
// toast.tsx also exports a `Toaster`; the sonner one (used by the console) keeps that name.
export {
  Toaster as ToastManagerToaster,
  Toast,
  ToastAction,
  ToastClose,
  ToastContent,
  ToastDescription,
  ToastPortal,
  ToastProvider,
  ToastTitle,
  ToastViewport,
  createToastManager,
  toast,
  useToastManager,
} from './components/ui/toast'
export * from './components/ui/toggle-group'
export * from './components/ui/toggle'
export * from './components/ui/tooltip'
