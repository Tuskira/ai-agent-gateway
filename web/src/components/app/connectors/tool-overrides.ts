import type { ToolArgOverrides } from '@/lib/connectors'

/** Local, editable row for one pinned tool-argument override. Mirrors the
 * `header-rows.ts` pattern used for authentication headers: flat rows while
 * editing, converted to/from the API's nested
 * `{ [toolName]: { [argName]: value } }` shape on save/load. */
export interface OverrideRowState {
  id: string
  toolName: string
  argName: string
  value: string
}

export function newOverrideRow(): OverrideRowState {
  return { id: crypto.randomUUID(), toolName: '', argName: '', value: '' }
}

export function overridesToRows(overrides?: ToolArgOverrides): OverrideRowState[] {
  if (!overrides) return []
  const rows: OverrideRowState[] = []
  for (const [toolName, args] of Object.entries(overrides)) {
    for (const [argName, value] of Object.entries(args)) {
      rows.push({ id: crypto.randomUUID(), toolName, argName, value })
    }
  }
  return rows
}

export function rowsToOverrides(rows: OverrideRowState[]): ToolArgOverrides | undefined {
  const overrides: ToolArgOverrides = {}
  for (const row of rows) {
    const tool = row.toolName.trim()
    const arg = row.argName.trim()
    if (!tool || !arg) continue
    overrides[tool] = { ...overrides[tool], [arg]: row.value }
  }
  return Object.keys(overrides).length > 0 ? overrides : undefined
}

export function overrideRowError(row: OverrideRowState): string | null {
  if (!row.toolName.trim()) return 'Tool name is required'
  if (!row.argName.trim()) return 'Argument name is required'
  return null
}
