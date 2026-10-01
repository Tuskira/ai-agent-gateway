import { useEffect, useMemo, useRef, useState } from 'react'
import { toast } from 'sonner'
import {
  useAllConnectorTools,
  useConnectorsList,
  useProfile,
  useProfileTools,
  useSetProfileTools,
} from '@/lib/queries'
import { profileToolKey, type ProfileTool } from '@/lib/profiles'

export interface SelectedTool {
  connectorId: string
  connectorName: string
  toolName: string
  toolNamespace?: string
}

/**
 * Shared tool-grant editor logic for a single profile: loads every
 * connector's cached tools, seeds a local editable selection from the
 * profile's currently-saved tools exactly once, and exposes grant/revoke +
 * save. Used by both `/profiles/{id}/tools` (`ProfileToolsPage`) and the
 * Profile Studio modal's Tools tab, so the two stay in sync by construction
 * rather than by keeping two copies of this logic in sync by hand.
 */
export function useProfileToolsManager(id: string) {
  const profileQuery = useProfile(id)
  const connectorsQuery = useConnectorsList()
  const connectors = useMemo(
    () => connectorsQuery.data?.items ?? [],
    [connectorsQuery.data],
  )
  const toolsQueries = useAllConnectorTools(connectors)
  const profileToolsQuery = useProfileTools(id)
  const setProfileTools = useSetProfileTools(id)

  const [filter, setFilter] = useState('')
  const [selected, setSelected] = useState<Map<string, SelectedTool>>(new Map())
  // A ref, not state: tracking "have we seeded from the server yet" doesn't
  // itself need to trigger a render.
  const seededRef = useRef(false)

  // Sync the profile's saved tool set into local editable state exactly
  // once, the moment it first loads. After that, `selected` is the
  // person's in-progress draft and must not be overwritten by a background
  // refetch — this one-time sync from an external (server) source is a
  // legitimate effect, not a value derivable during render.
  useEffect(() => {
    if (seededRef.current || !profileToolsQuery.data) return
    const next = new Map<string, SelectedTool>()
    for (const t of profileToolsQuery.data.items) {
      const connector = connectors.find((c) => c.id === t.connector_id)
      next.set(profileToolKey(t.connector_id, t.tool_name), {
        connectorId: t.connector_id,
        connectorName: connector?.name ?? t.connector_id,
        toolName: t.tool_name,
        toolNamespace: t.tool_namespace,
      })
    }
    seededRef.current = true
    setSelected(next)
    // Only re-seed when the raw server data changes; `connectors` is used
    // only to resolve display names and shouldn't retrigger seeding.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [profileToolsQuery.data])

  function toggleTool(
    connectorId: string,
    connectorName: string,
    toolName: string,
    toolNamespace?: string,
  ) {
    setSelected((prev) => {
      const key = profileToolKey(connectorId, toolName)
      const next = new Map(prev)
      if (next.has(key)) next.delete(key)
      else next.set(key, { connectorId, connectorName, toolName, toolNamespace })
      return next
    })
  }

  function removeTool(key: string) {
    setSelected((prev) => {
      const next = new Map(prev)
      next.delete(key)
      return next
    })
  }

  const filterLower = filter.trim().toLowerCase()
  const connectorGroups = useMemo(
    () =>
      connectors.map((connector, i) => {
        const tools = toolsQueries[i]?.data?.items ?? []
        const filtered = filterLower
          ? tools.filter(
              (t) =>
                t.tool_name.toLowerCase().includes(filterLower) ||
                connector.name.toLowerCase().includes(filterLower),
            )
          : tools
        return {
          connector,
          tools: filtered,
          isLoading: toolsQueries[i]?.isLoading ?? false,
        }
      }),
    [connectors, toolsQueries, filterLower],
  )

  const selectedList = Array.from(selected.entries())

  function handleSave() {
    const tools: ProfileTool[] = selectedList.map(([, t]) => ({
      connector_id: t.connectorId,
      tool_name: t.toolName,
      tool_namespace: t.toolNamespace,
    }))
    setProfileTools.mutate(tools, {
      onSuccess: () => {
        toast.success('Profile tools saved')
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to save profile tools')
      },
    })
  }

  return {
    profileQuery,
    connectorsQuery,
    connectorGroups,
    filter,
    setFilter,
    selected,
    selectedList,
    toggleTool,
    removeTool,
    handleSave,
    isSaving: setProfileTools.isPending,
  }
}
