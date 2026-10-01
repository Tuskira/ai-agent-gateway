import type { CommandArgument } from '@/lib/skills'

/** One row of the arguments editor -- a stable `id`, independent of the
 * (possibly duplicate or still-being-typed) `name`. */
export interface ArgumentRow {
  id: string
  name: string
  description: string
  required: boolean
}

let counter = 0
function nextId(): string {
  counter += 1
  return `arg-${counter}`
}

export function newArgumentRow(): ArgumentRow {
  return { id: nextId(), name: '', description: '', required: false }
}

export function rowsToArguments(rows: ArgumentRow[]): CommandArgument[] {
  return rows.map((r) => ({
    name: r.name.trim(),
    description: r.description.trim() || undefined,
    required: r.required,
  }))
}

export function argumentsToRows(args: CommandArgument[] | undefined): ArgumentRow[] {
  return (args ?? []).map((a) => ({
    id: nextId(),
    name: a.name,
    description: a.description ?? '',
    required: a.required,
  }))
}
