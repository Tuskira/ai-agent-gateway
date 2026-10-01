import { apiFetch } from '@/lib/api'
import type { SkillKind } from '@/lib/skills'

/** Data contract for the Profiles page and the Profile Studio tool manager. */

interface Page<T> {
  items: T[]
  total: number
}

export interface Profile {
  id: string
  name: string
  slug: string
  description?: string | null
  /** Free-text prompt appended to every session using this profile, on
   * top of the skill/command index (see `SKILLS-CONTRACT.md`, Phase 2's
   * `initialize` section). At most 8 KiB. */
  instructions?: string
  metadata?: Record<string, unknown>
  created_at: string
  updated_at: string
}

export interface ProfileTool {
  connector_id: string
  tool_name: string
  tool_namespace?: string
}

export interface ProfileInput {
  name: string
  description?: string
  instructions?: string
}

/** One entry of `GET /api/v1/profiles/{id}/skills` -- a skill or command
 * this profile is attached to, with enough of the registry row (name,
 * kind, description, latest_version) that the console doesn't need a
 * second round trip per row. `version: null` means "track latest". */
export interface ProfileSkillView {
  skill_id: string
  name: string
  kind: SkillKind
  version: number | null
  latest_version: number
  description: string
}

/** `PUT /api/v1/profiles/{id}/skills` body entry -- omit `version` (or
 * pass `undefined`) to track latest. */
export interface ProfileSkillInput {
  skill_id: string
  version?: number
}

/** `GET /api/v1/profiles` */
export function listProfiles(): Promise<Page<Profile>> {
  return apiFetch<Page<Profile>>('/profiles')
}

/** `GET /api/v1/profiles/{id}` */
export function getProfile(id: string): Promise<Profile> {
  return apiFetch<Profile>(`/profiles/${encodeURIComponent(id)}`)
}

/** `POST /api/v1/profiles` */
export function createProfile(input: ProfileInput): Promise<Profile> {
  return apiFetch<Profile>('/profiles', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

/** `PUT /api/v1/profiles/{id}` */
export function updateProfile(id: string, input: ProfileInput): Promise<Profile> {
  return apiFetch<Profile>(`/profiles/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

/** `DELETE /api/v1/profiles/{id}` */
export function deleteProfile(id: string): Promise<void> {
  return apiFetch<void>(`/profiles/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/** `GET /api/v1/profiles/{id}/tools` */
export function listProfileTools(id: string): Promise<Page<ProfileTool>> {
  return apiFetch<Page<ProfileTool>>(`/profiles/${encodeURIComponent(id)}/tools`)
}

/** `PUT /api/v1/profiles/{id}/tools` — replaces the full tool set. */
export function setProfileTools(
  id: string,
  tools: ProfileTool[],
): Promise<Page<ProfileTool>> {
  return apiFetch<Page<ProfileTool>>(`/profiles/${encodeURIComponent(id)}/tools`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ tools }),
  })
}

/** Stable key for a (connector, tool) pair — used as a Set/Map key when
 * tracking tool-manager selection state. */
export function profileToolKey(connectorId: string, toolName: string): string {
  return `${connectorId}::${toolName}`
}

/** `GET /api/v1/profiles/{id}/skills` */
export function listProfileSkills(id: string): Promise<Page<ProfileSkillView>> {
  return apiFetch<Page<ProfileSkillView>>(`/profiles/${encodeURIComponent(id)}/skills`)
}

/** `PUT /api/v1/profiles/{id}/skills` — replaces the full attached set
 * (same replace semantics as `setProfileTools`). */
export function setProfileSkills(
  id: string,
  items: ProfileSkillInput[],
): Promise<Page<ProfileSkillView>> {
  return apiFetch<Page<ProfileSkillView>>(`/profiles/${encodeURIComponent(id)}/skills`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ items }),
  })
}
