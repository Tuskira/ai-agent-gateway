import type { Table } from '@tanstack/react-table'
import { isSystemColumn } from './types'

/** Leading characters a spreadsheet would treat as the start of a formula. */
const FORMULA_START = /^[=+\-@\t\r]/

/**
 * Escape one CSV cell. Text that a spreadsheet would run as a formula is
 * prefixed with an apostrophe so it opens as plain text; numbers are left
 * alone so negatives stay numeric. Cells holding a comma, quote or line break
 * are quoted.
 */
export function csvCell(value: unknown): string {
  let str = String(value ?? '')
  if (typeof value !== 'number' && FORMULA_START.test(str)) str = `'${str}`
  return /[",\n\r]/.test(str) ? `"${str.replace(/"/g, '""')}"` : str
}

/** Assemble a CSV string from a header row and body rows of arbitrary cell values. */
export function rowsToCsv(headers: string[], rows: unknown[][]): string {
  return [
    headers.map(csvCell).join(','),
    ...rows.map((row) => row.map(csvCell).join(',')),
  ].join('\n')
}

/**
 * CSV of a table's visible, non-system columns. Values come from column
 * accessors, so a column without one exports as empty. `allRows` writes every
 * row in the current sort order instead of only the current page.
 */
export function tableToCsv<TData>(table: Table<TData>, allRows: boolean): string {
  const columns = table.getVisibleLeafColumns().filter((col) => !isSystemColumn(col.id))
  const headers = columns.map((col) =>
    typeof col.columnDef.header === 'string' ? col.columnDef.header : col.id,
  )
  const rowModel = allRows ? table.getPrePaginationRowModel() : table.getRowModel()
  const rows = rowModel.rows.map((row) =>
    columns.map((col) => (col.accessorFn ? row.getValue(col.id) : '')),
  )
  return rowsToCsv(headers, rows)
}

/** Trigger a browser download of `csvContent` as `filename`. */
export function downloadCsv(csvContent: string, filename: string) {
  const blob = new Blob([csvContent], { type: 'text/csv;charset=utf-8;' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}
