import { Skeleton } from '@/components/ui/skeleton'
import { TableBody, TableCell, TableRow } from '@/components/ui/table'

interface DataTableSkeletonProps {
  columnCount: number
  rowCount: number
  rowHeight: number
}

export function DataTableSkeleton({
  columnCount,
  rowCount,
  rowHeight,
}: DataTableSkeletonProps) {
  return (
    <TableBody aria-busy="true">
      {Array.from({ length: rowCount }).map((_, i) => (
        <TableRow key={i} className="hover:bg-transparent" style={{ height: rowHeight }}>
          {Array.from({ length: columnCount }).map((_, j) => (
            <TableCell key={j}>
              <Skeleton className="h-4 w-[80%]" />
            </TableCell>
          ))}
        </TableRow>
      ))}
    </TableBody>
  )
}
