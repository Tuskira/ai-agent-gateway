import {
  Bot,
  ChartPie,
  Clock,
  Database,
  KeyRound,
  LayoutDashboard,
  LibraryBig,
  MessagesSquare,
  Plug,
  ScrollText,
  Search,
  Share2,
  ShieldCheck,
  Sparkles,
  Users,
  type LucideIcon,
} from 'lucide-react'

export interface NavItem {
  label: string
  path: string
  icon: LucideIcon
  /** One-sentence description used on the ComingSoon placeholder page. */
  description: string
  /** Hide from the nav unless the principal has the `admin` role. The API
   * still enforces permissions; this only avoids showing a dead link. */
  adminOnly?: boolean
}

export interface NavGroup {
  label: string
  items: NavItem[]
}

export const OVERVIEW_PATH = '/'

export const navGroups: NavGroup[] = [
  {
    label: 'Overview',
    items: [
      {
        label: 'Overview',
        path: OVERVIEW_PATH,
        icon: LayoutDashboard,
        description: 'Gateway status at a glance.',
      },
    ],
  },
  {
    label: 'Configure',
    items: [
      {
        label: 'MCPs',
        path: '/connectors',
        icon: Plug,
        description: 'Manage the MCP servers the gateway fronts.',
      },
      {
        label: 'Models',
        path: '/models',
        icon: Bot,
        description: 'LLMs available through the gateway, and their usage.',
      },
      {
        label: 'Model catalog',
        path: '/model-catalog',
        icon: LibraryBig,
        description:
          'Platform-level providers and models tenants can connect from the Models page.',
      },
      {
        label: 'Profiles',
        path: '/profiles',
        icon: Share2,
        description: 'Define routing and policy profiles for MCP clients.',
      },
      {
        label: 'Skills',
        path: '/skills',
        icon: Sparkles,
        description: 'Text-only skills and commands agents can load through a profile.',
      },
      {
        label: 'Users',
        path: '/users',
        icon: Users,
        description:
          'Manage who can sign in to the console, and review the auth audit log.',
        adminOnly: true,
      },
    ],
  },
  {
    label: 'Observe',
    items: [
      {
        label: 'Access Logs',
        path: '/access-logs',
        icon: ScrollText,
        description: 'Inspect requests handled by the gateway.',
      },
      {
        label: 'LLM Logs',
        path: '/llm-logs',
        icon: MessagesSquare,
        description: 'Inspect prompts and completions proxied by the gateway.',
      },
      {
        label: 'Session Timeline',
        path: '/session-timeline',
        icon: Clock,
        description: 'Follow the LLM and MCP calls of one session in order.',
      },
      {
        label: 'Token Monitoring',
        path: '/token-monitoring',
        icon: ChartPie,
        description: 'Token usage and cost by model, caller and role.',
      },
    ],
  },
  {
    label: 'Operations',
    items: [
      {
        label: 'Tool Search',
        path: '/tool-search',
        icon: Search,
        description: 'Search and inspect tools exposed to MCP clients.',
      },
      {
        label: 'Cache',
        path: '/cache',
        icon: Database,
        description: 'Inspect and manage the gateway response cache.',
      },
    ],
  },
  {
    label: 'Access',
    items: [
      {
        label: 'API Keys',
        path: '/api-keys',
        icon: KeyRound,
        description: 'Issue and revoke API keys for the gateway.',
      },
      {
        label: 'Credentials',
        path: '/credentials',
        icon: ShieldCheck,
        description: 'Manage credentials used by upstream MCPs.',
      },
    ],
  },
]

export const navItems: NavItem[] = navGroups.flatMap((group) => group.items)

/** The nav item for path: an exact match, else the deepest item the path
 * sits under by whole segments (a drill-down names its section). */
export function findNavItem(path: string): NavItem | undefined {
  const exact = navItems.find((item) => item.path === path)
  if (exact) return exact
  return navItems
    .filter((item) => item.path !== OVERVIEW_PATH && path.startsWith(item.path + '/'))
    .sort((a, b) => b.path.length - a.path.length)[0]
}
