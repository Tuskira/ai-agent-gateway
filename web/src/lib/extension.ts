import { createContext, useContext, type ComponentType } from 'react'
import { navGroups, type NavGroup } from '@/lib/nav'

/** A page a host adds inside the console's app shell. */
export interface ExtensionRoute {
  path: string
  Component: ComponentType
}

/**
 * What a host app adds to the console when it mounts it.
 *
 * - navGroups are merged into the sidebar: a group whose label matches a core
 *   group adds its items to that group; any other group is inserted before the
 *   core group named by its `before` (at the end without one).
 * - routes are rendered inside the app shell, behind the login, like the core
 *   pages; a nav item's path needs a route here to be more than a "coming
 *   soon" placeholder.
 */
export interface ConsoleExtension {
  navGroups?: NavGroup[]
  routes?: ExtensionRoute[]
}

export const NavGroupsContext = createContext<NavGroup[]>(navGroups)

/** The nav groups to render: the core nav, plus the extension's when inside an
 * `ExtensionProvider`. */
export function useNavGroups(): NavGroup[] {
  return useContext(NavGroupsContext)
}
