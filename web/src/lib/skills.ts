import { apiFetch } from '@/lib/api'

/**
 * Data contract for the Skills page (`/skills`) and its Add dialog /
 * detail sheet -- the skill/command registry (`/api/v1/skills`,
 * Phase 1 of the Skills & Commands on Profiles project; see
 * `SKILLS-CONTRACT.md`). One table backs both "skills" (loaded on demand
 * by an agent via the `gateway__skill` tool) and "commands" (rendered as
 * MCP prompts) -- `kind` tells them apart.
 */

interface Page<T> {
  items: T[]
  total: number
}

export type SkillKind = 'skill' | 'command'

/** "platform" = a tenant-id-NULL row seeded at startup, visible to every
 * tenant and read-only in the console (same convention as
 * `RegisteredModel.scope`); "tenant" = this tenant's own row. */
export type SkillScope = 'tenant' | 'platform'

export interface CommandArgument {
  /** `^[a-z][a-z0-9_]{0,31}$` */
  name: string
  description?: string
  required: boolean
}

/** A file as the API returns it -- always has a digest and size (used to
 * corroborate what MCP `resources/read` serves, per Phase 3). */
export interface SkillVersionFile {
  path: string
  content: string
  sha256: string
  size: number
}

/** A file as the console sends it -- the server computes the digest and
 * size itself. */
export interface SkillFileInput {
  path: string
  content: string
}

/** A skill/command row, without its file contents (those live on a
 * version). Every key is always present: `description` is `""`,
 * `frontmatter`/`metadata` are `{}`, `arguments` is `[]` for a plain
 * skill. */
export interface Skill {
  id: string
  name: string
  kind: SkillKind
  description: string
  frontmatter: Record<string, unknown>
  arguments: CommandArgument[]
  latest_version: number
  enabled: boolean
  metadata: Record<string, unknown>
  scope: SkillScope
  created_at: string
  updated_at: string
}

/** `GET /api/v1/skills/{id}` -- the row plus its latest version's files. */
export interface SkillDetail extends Skill {
  latest: {
    version: number
    files: SkillVersionFile[]
  }
}

/** One entry of `GET /api/v1/skills/{id}/versions` -- no file contents
 * (see the store facet's `ListVersions` doc comment). */
export interface SkillVersionSummary {
  version: number
  created_by: string
  created_at: string
  file_count: number
}

/** `GET /api/v1/skills/{id}/versions/{v}` and the response of
 * `POST /api/v1/skills/{id}/versions` -- with file contents. */
export interface SkillVersion {
  version: number
  created_by: string
  created_at: string
  files: SkillVersionFile[]
}

/** `POST /api/v1/skills` body. `description` is not a field here: the
 * registry always derives it from the submitted SKILL.md's frontmatter
 * `description` key (see `Skill.description`), so the console never sends
 * one. */
export interface CreateSkillInput {
  name: string
  kind: SkillKind
  enabled?: boolean
  metadata?: Record<string, unknown>
  arguments?: CommandArgument[]
  files: SkillFileInput[]
}

/** `PUT /api/v1/skills/{id}` body -- name, kind, and files are immutable
 * after creation. `description` is read-only here too, for the same
 * reason as `CreateSkillInput`: a new version's SKILL.md frontmatter is
 * how it changes (`AddVersion` refreshes `description`/`frontmatter` from
 * the new SKILL.md), never a PUT. */
export interface UpdateSkillInput {
  enabled?: boolean
  metadata?: Record<string, unknown>
  arguments?: CommandArgument[]
}

/** `GET /api/v1/skills?kind=skill|command` */
export function listSkills(kind?: SkillKind): Promise<Page<Skill>> {
  const query = kind ? `?kind=${encodeURIComponent(kind)}` : ''
  return apiFetch<Page<Skill>>(`/skills${query}`)
}

/** `GET /api/v1/skills/{id}` */
export function getSkill(id: string): Promise<SkillDetail> {
  return apiFetch<SkillDetail>(`/skills/${encodeURIComponent(id)}`)
}

/** `POST /api/v1/skills` */
export function createSkill(body: CreateSkillInput): Promise<Skill> {
  return apiFetch<Skill>('/skills', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

/** `PUT /api/v1/skills/{id}` -- tenant-scope rows only; the registry 403s
 * on a platform row (see `Skill.scope`). */
export function updateSkill(id: string, body: UpdateSkillInput): Promise<Skill> {
  return apiFetch<Skill>(`/skills/${encodeURIComponent(id)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

/** `DELETE /api/v1/skills/{id}` (soft delete; platform rows -> 403). */
export function deleteSkill(id: string): Promise<void> {
  return apiFetch<void>(`/skills/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/** `GET /api/v1/skills/{id}/versions` */
export function listSkillVersions(id: string): Promise<Page<SkillVersionSummary>> {
  return apiFetch<Page<SkillVersionSummary>>(`/skills/${encodeURIComponent(id)}/versions`)
}

/** `GET /api/v1/skills/{id}/versions/{v}` */
export function getSkillVersion(id: string, version: number): Promise<SkillVersion> {
  return apiFetch<SkillVersion>(`/skills/${encodeURIComponent(id)}/versions/${version}`)
}

/** `POST /api/v1/skills/{id}/versions` -- adds a new version (files only);
 * bumps `latest_version` and refreshes `description`/`frontmatter`. */
export function createSkillVersion(
  id: string,
  files: SkillFileInput[],
): Promise<SkillVersion> {
  return apiFetch<SkillVersion>(`/skills/${encodeURIComponent(id)}/versions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ files }),
  })
}
