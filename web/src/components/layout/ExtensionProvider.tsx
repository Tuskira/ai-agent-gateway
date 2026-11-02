import { useMemo, type ReactNode } from 'react'
import { NavGroupsContext, type ConsoleExtension } from '@/lib/extension'
import { mergeNavGroups, navGroups } from '@/lib/nav'

/** Makes the sidebar and top bar below it use the core nav merged with the
 * extension's groups. `AppShell` mounts this for its `extension` prop. */
export function ExtensionProvider({
  extension,
  children,
}: {
  extension?: ConsoleExtension
  children: ReactNode
}) {
  const extra = extension?.navGroups
  const groups = useMemo(
    () => (extra?.length ? mergeNavGroups(navGroups, extra) : navGroups),
    [extra],
  )
  return <NavGroupsContext.Provider value={groups}>{children}</NavGroupsContext.Provider>
}
