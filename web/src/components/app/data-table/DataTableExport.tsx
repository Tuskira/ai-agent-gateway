import type { Table } from '@tanstack/react-table'
import { Download } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { downloadCsv, tableToCsv } from './csv'
import { LABELS } from './labels'

interface DataTableExportProps<TData> {
  table: Table<TData>
  /** Client mode exports every row; server mode only has the current page. */
  allRows: boolean
  fileName: string
  /** When set, replaces the built-in CSV download. */
  onExport?: () => void
}

export function DataTableExport<TData>({
  table,
  allRows,
  fileName,
  onExport,
}: DataTableExportProps<TData>) {
  return (
    <Button
      variant="ghost"
      size="sm"
      onClick={() => {
        if (onExport) onExport()
        else downloadCsv(tableToCsv(table, allRows), fileName)
      }}
    >
      <Download className="size-4" aria-hidden="true" />
      {LABELS.export}
    </Button>
  )
}
