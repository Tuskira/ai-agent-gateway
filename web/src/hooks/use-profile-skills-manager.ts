import { useEffect, useMemo, useRef, useState } from 'react'
import { toast } from 'sonner'
import { useProfileSkills, useSetProfileSkills, useSkillsList } from '@/lib/queries'
import type { ProfileSkillInput } from '@/lib/profiles'

export interface SelectedSkill {
  /** `null` = track latest. */
  version: number | null
}

/**
 * Shared "Skills & commands" grant editor logic for a single profile:
 * loads the full tenant+platform skill/command registry, seeds a local
 * editable selection from the profile's currently-attached set exactly
 * once, and exposes grant/revoke + a version pin + save. Same
 * seed-once/local-draft/full-replace-PUT shape as
 * `useProfileToolsManager`, so the "Skills & commands" tab in Profile
 * Studio behaves the same way as the existing Tools tab.
 */
export function useProfileSkillsManager(profileId: string) {
  const skillsQuery = useSkillsList()
  const profileSkillsQuery = useProfileSkills(profileId)
  const setProfileSkills = useSetProfileSkills(profileId)

  const skills = useMemo(() => skillsQuery.data?.items ?? [], [skillsQuery.data])

  const [filter, setFilter] = useState('')
  const [selected, setSelected] = useState<Map<string, SelectedSkill>>(new Map())
  // A ref, not state: same reasoning as `useProfileToolsManager` -- "have
  // we seeded from the server yet" doesn't itself need to trigger a render.
  const seededRef = useRef(false)

  // Sync the profile's saved skill set into local editable state exactly
  // once. After that, `selected` is the person's in-progress draft and
  // must not be overwritten by a background refetch.
  useEffect(() => {
    if (seededRef.current || !profileSkillsQuery.data) return
    const next = new Map<string, SelectedSkill>()
    for (const s of profileSkillsQuery.data.items) {
      next.set(s.skill_id, { version: s.version })
    }
    seededRef.current = true
    setSelected(next)
  }, [profileSkillsQuery.data])

  function toggleSkill(skillId: string) {
    setSelected((prev) => {
      const next = new Map(prev)
      if (next.has(skillId)) next.delete(skillId)
      else next.set(skillId, { version: null })
      return next
    })
  }

  function setSkillVersion(skillId: string, version: number | null) {
    setSelected((prev) => {
      if (!prev.has(skillId)) return prev
      const next = new Map(prev)
      next.set(skillId, { version })
      return next
    })
  }

  const filterLower = filter.trim().toLowerCase()
  const filteredSkills = useMemo(
    () =>
      filterLower
        ? skills.filter(
            (s) =>
              s.name.toLowerCase().includes(filterLower) ||
              s.description.toLowerCase().includes(filterLower),
          )
        : skills,
    [skills, filterLower],
  )

  const selectedList = Array.from(selected.entries())

  function handleSave() {
    const items: ProfileSkillInput[] = selectedList.map(([skillId, sel]) => ({
      skill_id: skillId,
      version: sel.version ?? undefined,
    }))
    setProfileSkills.mutate(items, {
      onSuccess: () => {
        toast.success('Profile skills saved')
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to save profile skills')
      },
    })
  }

  return {
    skillsQuery,
    profileSkillsQuery,
    filteredSkills,
    filter,
    setFilter,
    selected,
    selectedList,
    toggleSkill,
    setSkillVersion,
    handleSave,
    isSaving: setProfileSkills.isPending,
  }
}
