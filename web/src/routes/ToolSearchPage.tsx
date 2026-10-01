import { useState, type FormEvent } from 'react'
import { Search } from 'lucide-react'
import { useCacheSearch } from '@/lib/queries'
import type { CacheSearchItem } from '@/lib/cache'
import { DataTable, DataTablePage, type ColumnDef } from '@/components/app/data-table'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'

export default function ToolSearchPage() {
  const [inputValue, setInputValue] = useState('')
  const [query, setQuery] = useState('')
  const [includeStale, setIncludeStale] = useState(false)
  const searchQuery = useCacheSearch(query, includeStale)

  function handleSubmit(e: FormEvent) {
    e.preventDefault()
    setQuery(inputValue.trim())
  }

  const columns: ColumnDef<CacheSearchItem>[] = [
    {
      id: 'tool_name',
      accessorKey: 'tool_name',
      header: 'Tool',
      size: 220,
      cell: ({ row }) => (
        <span className="font-mono font-medium text-foreground">
          {row.original.tool_name}
        </span>
      ),
    },
    {
      id: 'connector_name',
      accessorKey: 'connector_name',
      header: 'MCP',
      cell: ({ row }) => (
        <span className="font-mono text-xs text-text-muted">
          {row.original.connector_name}
        </span>
      ),
    },
    {
      id: 'description',
      accessorFn: (t) => t.description ?? '',
      header: 'Description',
      size: 320,
      cell: ({ row }) => (
        <span className="text-text-subtle">{row.original.description || '—'}</span>
      ),
    },
    {
      id: 'schema',
      header: 'Input schema',
      size: 320,
      enableSorting: false,
      cell: ({ row }) =>
        row.original.input_schema ? (
          <details>
            <summary className="cursor-pointer text-xs font-medium text-text-link">
              View schema
            </summary>
            <pre className="mt-2 overflow-x-auto rounded-r-3 border border-border bg-bg-subtle p-2.5 font-mono text-[11px] text-foreground">
              {JSON.stringify(row.original.input_schema, null, 2)}
            </pre>
          </details>
        ) : (
          <span className="text-xs text-text-subtle">—</span>
        ),
    },
  ]

  const hasSearched = query.length > 0
  const notDeployed = hasSearched && searchQuery.data === null
  const items = searchQuery.data?.items ?? []

  return (
    <DataTablePage>
      <div>
        <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
          Tool Search
        </h1>
        <p className="text-[13px] text-text-subtle">
          Search across all cached tools from your MCPs.
        </p>
      </div>

      <form
        onSubmit={handleSubmit}
        className="flex flex-col gap-3 rounded-r-5 border border-border bg-card p-4"
      >
        <label className="flex h-11 items-center gap-2.5 rounded-r-4 border border-border bg-background px-3.5 text-text-subtle">
          <Search className="size-[18px] shrink-0" aria-hidden="true" />
          <Input
            value={inputValue}
            onChange={(e) => setInputValue(e.target.value)}
            placeholder="Search by tool name or description…"
            className="h-auto flex-1 border-0 bg-transparent p-0 text-sm shadow-none focus-visible:ring-0"
          />
        </label>
        <div className="flex items-center justify-between gap-3">
          <label className="flex cursor-pointer items-center gap-2 text-[13px] text-foreground">
            <Checkbox
              checked={includeStale}
              onCheckedChange={(checked) => setIncludeStale(checked === true)}
            />
            Include stale results
          </label>
          <Button type="submit" className="uppercase tracking-[.04em]">
            <Search className="size-3.5" aria-hidden="true" />
            Search
          </Button>
        </div>
      </form>

      {!hasSearched ? (
        <p className="py-16 text-center text-[13.5px] text-text-subtle">
          Enter a search query to find tools across all MCPs.
        </p>
      ) : notDeployed ? (
        <p className="py-16 text-center text-[13.5px] text-text-subtle">
          Tool search isn&apos;t available yet on this gateway.
        </p>
      ) : (
        <DataTable
          storageKey="tool-search"
          columns={columns}
          data={items}
          getRowId={(t) => `${t.connector_id}::${t.tool_name}`}
          loading={searchQuery.isLoading}
          error={searchQuery.isError && 'Search failed.'}
          emptyTitle="No matching tools"
          emptyDescription="Try a different search term."
        />
      )}
    </DataTablePage>
  )
}
