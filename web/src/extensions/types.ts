import type { ComponentType } from 'react'
import type { NavGroup } from '@/lib/nav'

/** A page an extension adds inside the console's app shell. */
export interface ExtensionRoute {
  path: string
  Component: ComponentType
}

/**
 * What a build of the console adds to it. The open-source console ships
 * none (./index.ts); a distribution that builds this console from source can
 * alias `@/extensions` to its own module exporting `extension`.
 *
 * - navGroups are merged into the sidebar: a group whose label matches a
 *   core group adds its items to that group; any other group is inserted
 *   before the core group named by its `before` (at the end without one).
 * - routes are rendered inside the app shell, behind the login, like the
 *   core pages; a nav item's path needs a route here to be more than a
 *   "coming soon" placeholder.
 */
export interface ConsoleExtension {
  navGroups?: NavGroup[]
  routes?: ExtensionRoute[]
}
