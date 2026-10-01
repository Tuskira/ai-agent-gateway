import type { SkillFileInput, SkillKind, SkillVersionFile } from '@/lib/skills'
import { SKILL_MD_PATH, skillMdTemplate } from '@/lib/skill-validation'

/** One row of the file editor -- a stable `id` so React (and per-row error
 * lookups) survive reordering/removal, independent of the (possibly
 * duplicate or still-being-typed) `path`. */
export interface SkillFileRow {
  id: string
  path: string
  content: string
}

let counter = 0
function nextId(prefix: string): string {
  counter += 1
  return `${prefix}-${counter}`
}

/** The always-present first row: `SKILL.md`, prefilled with a template
 * using the typed name -- see `skillMdTemplate`. */
export function defaultSkillMdRow(name: string, kind: SkillKind): SkillFileRow {
  return { id: nextId('file'), path: SKILL_MD_PATH, content: skillMdTemplate(name, kind) }
}

export function newSkillFileRow(): SkillFileRow {
  return { id: nextId('file'), path: '', content: '' }
}

export function rowsToFiles(rows: SkillFileRow[]): SkillFileInput[] {
  return rows.map((r) => ({ path: r.path.trim(), content: r.content }))
}

export function filesToRows(files: (SkillVersionFile | SkillFileInput)[]): SkillFileRow[] {
  return files.map((f) => ({ id: nextId('file'), path: f.path, content: f.content }))
}
