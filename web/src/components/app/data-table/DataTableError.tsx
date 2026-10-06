import { AlertCircle } from 'lucide-react'
import { TableBody, TableCell, TableRow } from '@/components/ui/table'

interface DataTableErrorProps {
  columnCount: number
  message: string
}

export function DataTableError({ columnCount, message }: DataTableErrorProps) {
  return (
    <TableBody>
      <TableRow className="hover:bg-transparent">
        <TableCell colSpan={columnCount} className="h-48 p-0 whitespace-normal">
          <div
            role="alert"
            className="sticky left-0 flex w-[100cqw] flex-col items-center justify-center gap-2 px-2 text-center text-sm text-sev-high"
          >
            <AlertCircle className="size-5" aria-hidden="true" />
            {message}
          </div>
        </TableCell>
      </TableRow>
    </TableBody>
  )
}
