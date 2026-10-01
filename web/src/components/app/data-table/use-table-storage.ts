import { useEffect, useState } from 'react'
import type { ColumnSizingState, VisibilityState } from '@tanstack/react-table'
import type { SortModel } from './types'

/**
 * What is saved per `storageKey`. Column order, pagination and row selection
 * are intentionally left out.
 */
export interface PersistedTableState {
  columnVisibility: VisibilityState
  columnSizing: ColumnSizingState
  sorting: SortModel[]
}

interface Options {
  storageKey: string
  defaultVisibility?: VisibilityState
  /** When set, replaces any saved sort. */
  defaultSorting?: SortModel[]
  version?: number
}

export const TABLE_STORAGE_PREFIX = 'gateway.table.'

interface Envelope {
  v: number
  state: PersistedTableState
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

// Saved state is read back as untrusted input: it may have been written by an
// older build, edited by hand, or damaged. Each reader keeps the entries that
// are well formed and drops the rest, so one bad value never costs the others.

function readVisibility(value: unknown): VisibilityState {
  if (!isRecord(value)) return {}
  const out: VisibilityState = {}
  for (const [id, visible] of Object.entries(value)) {
    if (typeof visible === 'boolean') out[id] = visible
  }
  return out
}

function readSizing(value: unknown): ColumnSizingState {
  if (!isRecord(value)) return {}
  const out: ColumnSizingState = {}
  for (const [id, width] of Object.entries(value)) {
    if (typeof width === 'number' && Number.isFinite(width) && width > 0) out[id] = width
  }
  return out
}

function readSorting(value: unknown): SortModel[] {
  if (!Array.isArray(value)) return []
  return value.flatMap((entry): SortModel[] => {
    if (!isRecord(entry) || typeof entry.field !== 'string') return []
    if (entry.sort !== 'asc' && entry.sort !== 'desc') return []
    return [{ field: entry.field, sort: entry.sort }]
  })
}

function readInitial(
  storageKey: string,
  defaultVisibility: VisibilityState,
  defaultSorting: SortModel[] | undefined,
  version: number,
): PersistedTableState {
  const defaults: PersistedTableState = {
    columnVisibility: { ...defaultVisibility },
    columnSizing: {},
    sorting: defaultSorting ?? [],
  }
  if (typeof window === 'undefined') return defaults

  try {
    const raw = window.localStorage.getItem(TABLE_STORAGE_PREFIX + storageKey)
    if (!raw) return defaults
    const parsed: unknown = JSON.parse(raw)
    if (!isRecord(parsed) || parsed.v !== version || !isRecord(parsed.state)) {
      return defaults
    }
    return {
      columnVisibility: {
        ...defaults.columnVisibility,
        ...readVisibility(parsed.state.columnVisibility),
      },
      columnSizing: readSizing(parsed.state.columnSizing),
      sorting: defaultSorting ?? readSorting(parsed.state.sorting),
    }
  } catch {
    return defaults
  }
}

/**
 * Column visibility, widths and sorting, saved to localStorage under a
 * versioned envelope. Unreadable or outdated entries fall back to defaults.
 */
export function useTableStorage({
  storageKey,
  defaultVisibility = {},
  defaultSorting,
  version = 1,
}: Options) {
  const [state, setState] = useState<PersistedTableState>(() =>
    readInitial(storageKey, defaultVisibility, defaultSorting, version),
  )

  useEffect(() => {
    if (typeof window === 'undefined') return
    try {
      const envelope: Envelope = { v: version, state }
      window.localStorage.setItem(
        TABLE_STORAGE_PREFIX + storageKey,
        JSON.stringify(envelope),
      )
    } catch {
      // Quota exceeded or storage disabled: keep working without persistence.
    }
  }, [storageKey, version, state])

  return [state, setState] as const
}
