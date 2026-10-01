import { useEffect, useRef, useState } from 'react'
import { toast } from 'sonner'
import { useProfile, useUpdateProfile } from '@/lib/queries'
import { byteSize } from '@/lib/skill-validation'

/** `instructions` is capped at 8 KiB server-side (see `SKILLS-CONTRACT.md`,
 * "Names and limits"). */
export const PROFILE_INSTRUCTIONS_MAX_BYTES = 8 * 1024

/**
 * Editor logic for Profile Studio's "Instructions" tab: seeds a local
 * draft from the profile's saved `instructions` exactly once (same
 * seed-once pattern as `useProfileToolsManager`/`useProfileSkillsManager`),
 * rejects any edit that would push the draft over the byte limit, and
 * saves via a full `PUT /api/v1/profiles/{id}` carrying the profile's
 * existing name/description alongside the new instructions so this tab
 * never clobbers what the "Edit profile" dialog set.
 */
export function useProfileInstructionsManager(profileId: string) {
  const profileQuery = useProfile(profileId)
  const updateProfile = useUpdateProfile(profileId)

  const [value, setValueState] = useState('')
  const seededRef = useRef(false)

  useEffect(() => {
    if (seededRef.current || !profileQuery.data) return
    seededRef.current = true
    setValueState(profileQuery.data.instructions ?? '')
  }, [profileQuery.data])

  const byteLength = byteSize(value)
  const overLimit = byteLength > PROFILE_INSTRUCTIONS_MAX_BYTES

  /** Ignores an edit that would exceed the byte limit -- the textarea's
   * displayed value simply never grows past it, rather than allowing an
   * over-limit draft the person then has to notice and trim. */
  function setValue(next: string) {
    if (byteSize(next) > PROFILE_INSTRUCTIONS_MAX_BYTES) return
    setValueState(next)
  }

  function handleSave() {
    const profile = profileQuery.data
    if (!profile || overLimit) return
    updateProfile.mutate(
      {
        name: profile.name,
        description: profile.description ?? undefined,
        instructions: value,
      },
      {
        onSuccess: () => {
          toast.success('Instructions saved')
        },
        onError: (err) => {
          toast.error(err instanceof Error ? err.message : 'Failed to save instructions')
        },
      },
    )
  }

  return {
    profileQuery,
    value,
    setValue,
    byteLength,
    maxBytes: PROFILE_INSTRUCTIONS_MAX_BYTES,
    overLimit,
    handleSave,
    isSaving: updateProfile.isPending,
  }
}
