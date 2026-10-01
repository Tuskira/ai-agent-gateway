/**
 * Client-side mirrors of the skill/command registry's server-side
 * validation rules (see `SKILLS-CONTRACT.md`, "Names and limits"). Every
 * rule here has a twin enforced by the API — failing here should mean the
 * API would also reject the request with a 400 `validation_error`. This
 * file exists so `SkillFormDialog` and the "New version" editor can fail
 * fast instead of round-tripping one.
 */

import type { CommandArgument, SkillFileInput, SkillKind } from '@/lib/skills'

/** `^[a-z0-9][a-z0-9._-]{0,63}$` -- the registry's own name rule. */
export const SKILL_NAME_PATTERN = /^[a-z0-9][a-z0-9._-]{0,63}$/

/** Names the registry refuses outright, regardless of the pattern above. */
export const RESERVED_SKILL_NAMES: readonly string[] = ['gateway']

/** `^[A-Za-z0-9][A-Za-z0-9._/-]*$` -- a file path within a skill/command. */
export const SKILL_PATH_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._/-]*$/

export const ALLOWED_SKILL_FILE_EXTENSIONS: readonly string[] = [
  '.md',
  '.txt',
  '.json',
  '.yaml',
  '.yml',
  '.csv',
  '.xml',
  '.toml',
]

export const SKILL_MD_PATH = 'SKILL.md'
export const MIN_SKILL_FILES = 1
export const MAX_SKILL_FILES = 20
export const SKILL_MD_MAX_BYTES = 64 * 1024
export const SKILL_FILE_MAX_BYTES = 256 * 1024
export const SKILL_TOTAL_MAX_BYTES = 512 * 1024

/** Matches the frontmatter `description` limit (1..1024 chars); also used
 * as the limit for the dialog's separate top-level "Description" field. */
export const SKILL_DESCRIPTION_MAX = 1024

/** Portable Agent Skills fields plus the Claude Code fields the server
 * allows in SKILL.md frontmatter. `hooks` is deliberately absent -- it is
 * always rejected (it can run shell commands). */
export const ALLOWED_FRONTMATTER_KEYS: readonly string[] = [
  'name',
  'description',
  'license',
  'compatibility',
  'metadata',
  'allowed-tools',
  'user-invocable',
  'disable-model-invocation',
  'context',
  'agent',
  'background',
  'model',
  'effort',
  'paths',
  'shell',
  'arguments',
  'argument-hint',
  'when_to_use',
]

/** `^[a-z][a-z0-9_]{0,31}$` -- a command argument's name. */
export const COMMAND_ARGUMENT_NAME_PATTERN = /^[a-z][a-z0-9_]{0,31}$/
export const COMMAND_ARGUMENT_DESCRIPTION_MAX = 256
export const MAX_COMMAND_ARGUMENTS = 10

/** UTF-8 byte length -- the server's size limits are byte counts, not JS
 * string (UTF-16 code unit) lengths. */
export function byteSize(content: string): number {
  return new TextEncoder().encode(content).length
}

function formatKiB(bytes: number): string {
  return `${Math.round(bytes / 1024)} KiB`
}

/** `^[a-z0-9][a-z0-9._-]{0,63}$`, no `__` (reserved for the
 * `<connector>__<tool>` MCP naming scheme), and not the reserved name
 * `gateway`. */
export function validateSkillName(name: string): string | null {
  const trimmed = name.trim()
  if (!trimmed) return 'Name is required'
  if (!SKILL_NAME_PATTERN.test(trimmed)) {
    return 'Lowercase letters, numbers, dot, underscore, and dash only (max 64 chars), starting with a letter or number'
  }
  if (trimmed.includes('__')) {
    return 'Name cannot contain "__" (reserved for MCP tool names)'
  }
  if (RESERVED_SKILL_NAMES.includes(trimmed)) {
    return `"${trimmed}" is a reserved name`
  }
  return null
}

/** The dialog's separate top-level "Description" field -- optional, capped
 * the same as the frontmatter description. */
export function validateSkillDescription(description: string | undefined): string | null {
  if (description && description.length > SKILL_DESCRIPTION_MAX) {
    return `At most ${SKILL_DESCRIPTION_MAX} characters`
  }
  return null
}

/** No leading `/`, no trailing `/`, no `..` anywhere, matches the path
 * pattern, and ends in an allowed extension. */
export function validateSkillFilePath(path: string): string | null {
  if (!path) return 'Path is required'
  if (path.startsWith('/')) return 'Path cannot start with "/"'
  if (path.endsWith('/')) return 'Path cannot end with "/"'
  if (path.includes('..')) return 'Path cannot contain ".."'
  if (!SKILL_PATH_PATTERN.test(path)) {
    return 'Letters, numbers, dot, underscore, dash, and "/" only'
  }
  const dot = path.lastIndexOf('.')
  const ext = dot >= 0 ? path.slice(dot) : ''
  if (!ALLOWED_SKILL_FILE_EXTENSIONS.includes(ext)) {
    return `Extension must be one of ${ALLOWED_SKILL_FILE_EXTENSIONS.join(', ')}`
  }
  return null
}

/** `SKILL.md` may be up to 64 KiB; every other file up to 256 KiB. */
export function validateSkillFileSize(file: SkillFileInput): string | null {
  const size = byteSize(file.content)
  const limit = file.path === SKILL_MD_PATH ? SKILL_MD_MAX_BYTES : SKILL_FILE_MAX_BYTES
  if (size > limit) {
    return `Must be at most ${formatKiB(limit)} (currently ${formatKiB(size)})`
  }
  return null
}

/** Path then size -- the first failing rule for one file. */
export function skillFileError(file: SkillFileInput): string | null {
  return validateSkillFilePath(file.path) ?? validateSkillFileSize(file)
}

export interface SkillFilesValidation {
  /** Errors about the file set as a whole (count, missing SKILL.md, total
   * size) -- not tied to any one row. */
  errors: string[]
  /** Per-file error, parallel to the input array (`null` = no error). */
  fileErrors: (string | null)[]
}

/** 1..20 files, a root `SKILL.md`, no duplicate paths, each file's own
 * path/size rules, and a 512 KiB total. */
export function validateSkillFiles(files: SkillFileInput[]): SkillFilesValidation {
  const errors: string[] = []

  if (files.length < MIN_SKILL_FILES) {
    errors.push('At least one file is required')
  }
  if (files.length > MAX_SKILL_FILES) {
    errors.push(`At most ${MAX_SKILL_FILES} files are allowed`)
  }
  if (!files.some((f) => f.path.trim() === SKILL_MD_PATH)) {
    errors.push(`A root ${SKILL_MD_PATH} file is required`)
  }

  const pathCounts = new Map<string, number>()
  for (const file of files) {
    if (!file.path) continue
    pathCounts.set(file.path, (pathCounts.get(file.path) ?? 0) + 1)
  }

  let total = 0
  const fileErrors: (string | null)[] = files.map((file) => {
    total += byteSize(file.content)
    if (file.path && (pathCounts.get(file.path) ?? 0) > 1) {
      return 'Duplicate path'
    }
    return skillFileError(file)
  })

  if (total > SKILL_TOTAL_MAX_BYTES) {
    errors.push(
      `Total file size must be at most ${formatKiB(SKILL_TOTAL_MAX_BYTES)} (currently ${formatKiB(total)})`,
    )
  }

  return { errors, fileErrors }
}

export interface ParsedFrontmatter {
  /** Top-level scalar values only -- enough to check required keys, the
   * `name`/`description` values, and reject unknown/`hooks` keys. A nested
   * block under a key (e.g. `metadata:`) is not parsed further; its lines
   * are indented and skipped here. */
  keys: Record<string, string>
  /** Everything after the closing `---` -- the prompt template for a
   * command. */
  body: string
}

/** Splits SKILL.md into its YAML frontmatter block and the body. Returns
 * `null` when the content doesn't start with a `---`/`---`-delimited block
 * at all -- the server's own hard requirement. */
export function parseFrontmatter(content: string): ParsedFrontmatter | null {
  const match = /^---\r?\n([\s\S]*?)\r?\n---[ \t]*\r?\n?/.exec(content)
  if (!match) return null
  const [full, raw] = match
  const keys: Record<string, string> = {}
  for (const line of raw!.split(/\r?\n/)) {
    if (!line || /^[ \t]/.test(line)) continue // blank or nested (indented) line
    const kv = /^([A-Za-z0-9_-]+):[ \t]?(.*)$/.exec(line)
    if (!kv) continue
    keys[kv[1]!] = kv[2]!.trim().replace(/^["']|["']$/g, '')
  }
  return { keys, body: content.slice(full!.length) }
}

/** Frontmatter present, `name` == the skill's own name, `description`
 * non-empty (<=1024 chars), no `hooks` key, and no key outside the
 * allowed list. */
export function validateFrontmatter(skillMdContent: string, skillName: string): string[] {
  const errors: string[] = []
  const parsed = parseFrontmatter(skillMdContent)
  if (!parsed) {
    errors.push('SKILL.md must start with YAML frontmatter delimited by "---" lines')
    return errors
  }
  const { keys } = parsed

  if (!('name' in keys)) {
    errors.push('Frontmatter must include "name"')
  } else if (keys.name !== skillName) {
    errors.push(`Frontmatter "name" must equal the skill name ("${skillName}")`)
  }

  if (!keys.description) {
    errors.push('Frontmatter must include a non-empty "description"')
  } else if (keys.description.length > SKILL_DESCRIPTION_MAX) {
    errors.push(`Frontmatter "description" must be at most ${SKILL_DESCRIPTION_MAX} characters`)
  }

  if ('hooks' in keys) {
    errors.push('Frontmatter key "hooks" is not allowed')
  }

  for (const key of Object.keys(keys)) {
    if (key === 'hooks') continue
    if (!ALLOWED_FRONTMATTER_KEYS.includes(key)) {
      errors.push(`Unknown frontmatter key "${key}"`)
    }
  }

  return errors
}

/** `^[a-z][a-z0-9_]{0,31}$` -- a command argument's name. */
export function validateCommandArgumentName(name: string): string | null {
  if (!name.trim()) return 'Name is required'
  if (!COMMAND_ARGUMENT_NAME_PATTERN.test(name)) {
    return 'Lowercase letters, numbers, and underscore only, starting with a letter (max 32 chars)'
  }
  return null
}

/** One argument row's error: bad name, an over-long description, or a
 * name that duplicates an earlier row's. */
export function commandArgumentError(
  arg: CommandArgument,
  index: number,
  all: CommandArgument[],
): string | null {
  const nameError = validateCommandArgumentName(arg.name)
  if (nameError) return nameError
  if (arg.description && arg.description.length > COMMAND_ARGUMENT_DESCRIPTION_MAX) {
    return `Description must be at most ${COMMAND_ARGUMENT_DESCRIPTION_MAX} characters`
  }
  const firstIndex = all.findIndex((a) => a.name === arg.name)
  if (firstIndex !== index) return `Duplicate argument name "${arg.name}"`
  return null
}

/** 0..10 arguments. */
export function validateCommandArgumentCount(args: CommandArgument[]): string | null {
  if (args.length > MAX_COMMAND_ARGUMENTS) {
    return `At most ${MAX_COMMAND_ARGUMENTS} arguments are allowed`
  }
  return null
}

const PLACEHOLDER_PATTERN = /\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}/g

/** Every `{{name}}` placeholder referenced in a command template. */
export function findPlaceholders(template: string): string[] {
  const names = new Set<string>()
  for (const m of template.matchAll(PLACEHOLDER_PATTERN)) {
    names.add(m[1]!)
  }
  return [...names]
}

/** Placeholders used in the template that aren't declared as arguments --
 * the server rejects these with a 400 at create/version time. */
export function findUndeclaredPlaceholders(
  template: string,
  args: CommandArgument[],
): string[] {
  const declared = new Set(args.map((a) => a.name))
  return findPlaceholders(template).filter((name) => !declared.has(name))
}

/** A SKILL.md template for a new skill/command, prefilled with valid
 * frontmatter using the typed name -- kept in sync with the name field
 * until the person edits the SKILL.md content directly. */
export function skillMdTemplate(name: string, kind: SkillKind): string {
  const skillName = name || 'my-skill'
  const bodyHint =
    kind === 'command'
      ? 'Write the prompt template here. Reference any declared argument with {{argument_name}}.'
      : 'Write the skill body here -- the instructions an agent follows once it loads this skill.'
  return `---
name: ${skillName}
description: TODO -- one line describing when to use this ${kind}.
---

${bodyHint}
`
}
