import { useState } from 'react'
import { Link } from 'react-router-dom'
import { Pencil, Plug, Plus, Power, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import {
  useCredentialSourcesList,
  useDeleteCredentialSource,
  useHeaderProviders,
  useTestCredentialSource,
  useUpdateCredentialSource,
} from '@/lib/queries'
import type { CredentialSource } from '@/lib/credential-sources'
import { DataTable, type ColumnDef, type RowAction } from '@/components/app/data-table'
import { ConfirmDialog } from '@/components/app/ConfirmDialog'
import { StatusPill } from '@/components/app/StatusPill'
import { SourceFormDialog } from '@/components/app/credentials/SourceFormDialog'
import { Button } from '@/components/ui/button'

/** Outcome of the last "Test" click on a source, kept for this visit only. */
type TestState =
  { status: 'testing' } | { status: 'ok' } | { status: 'failed'; error: string }

function errorMessage(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback
}

interface SourcesTabProps {
  /** Opens the create dialog; owned by the page so its header button can. */
  createOpen: boolean
  onCreateOpenChange: (open: boolean) => void
}

/** The Sources tab of the Credentials page: the tenant's credential sources
 * with add / edit / enable / disable / delete / test. The API needs
 * `admin.manage` for all of it; the page only mounts this for admins. */
export function SourcesTab({ createOpen, onCreateOpenChange }: SourcesTabProps) {
  const sourcesQuery = useCredentialSourcesList()
  const providersQuery = useHeaderProviders()
  const updateSource = useUpdateCredentialSource()
  const deleteSource = useDeleteCredentialSource()
  const testSource = useTestCredentialSource()

  const [editTarget, setEditTarget] = useState<CredentialSource | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<CredentialSource | null>(null)
  const [deleteError, setDeleteError] = useState<string | null>(null)
  const [tests, setTests] = useState<Record<string, TestState>>({})

  const items = sourcesQuery.data?.items ?? []
  const providerNames = new Map(
    (providersQuery.data?.providers ?? []).map((p) => [p.id, p.name]),
  )

  function runTest(source: CredentialSource) {
    setTests((t) => ({ ...t, [source.id]: { status: 'testing' } }))
    testSource.mutate(source.id, {
      onSuccess: (result) => {
        setTests((t) => ({
          ...t,
          [source.id]: result.ok
            ? { status: 'ok' }
            : { status: 'failed', error: result.error ?? 'Test failed' },
        }))
        if (result.ok) toast.success(`${source.name} is reachable`)
        else toast.error(result.error ?? 'Test failed')
      },
      onError: (err) => {
        const error = errorMessage(err, 'Test failed')
        setTests((t) => ({ ...t, [source.id]: { status: 'failed', error } }))
        toast.error(error)
      },
    })
  }

  function toggleEnabled(source: CredentialSource, enabled: boolean) {
    updateSource.mutate(
      { id: source.id, input: { enabled } },
      {
        onSuccess: () => toast.success(enabled ? 'Source enabled' : 'Source disabled'),
        onError: (err) => toast.error(errorMessage(err, 'Failed to update source')),
      },
    )
  }

  function handleDelete() {
    if (!deleteTarget) return
    deleteSource.mutate(deleteTarget.id, {
      onSuccess: () => {
        toast.success('Credential source deleted')
        setDeleteTarget(null)
      },
      // A 409 means a connector header still names it: keep the dialog open
      // and show the API's message (which lists the connectors) in place.
      onError: (err) => setDeleteError(errorMessage(err, 'Failed to delete source')),
    })
  }

  const columns: ColumnDef<CredentialSource>[] = [
    {
      id: 'name',
      accessorKey: 'name',
      header: 'Name',
      size: 200,
      cell: ({ row }) => (
        <span className="truncate font-mono font-semibold text-foreground">
          {row.original.name}
        </span>
      ),
    },
    {
      id: 'provider',
      accessorKey: 'provider',
      header: 'Provider',
      cell: ({ row }) => (
        <span className="text-text-muted">
          {providerNames.get(row.original.provider) ?? row.original.provider}
        </span>
      ),
    },
    {
      id: 'url',
      accessorFn: (s) => (typeof s.config.url === 'string' ? s.config.url : ''),
      header: 'URL',
      cell: ({ getValue }) => (
        <span className="truncate font-mono text-[12.5px] text-text-muted">
          {getValue<string>() || '—'}
        </span>
      ),
    },
    {
      id: 'enabled',
      accessorFn: (s) => (s.enabled ? 'Enabled' : 'Disabled'),
      header: 'State',
      cell: ({ row }) => (
        <StatusPill
          tone={row.original.enabled ? 'positive' : 'neutral'}
          label={row.original.enabled ? 'Enabled' : 'Disabled'}
        />
      ),
    },
    {
      id: 'used_by',
      accessorFn: (s) => s.used_by.length,
      header: 'Used by',
      cell: ({ row }) => {
        const usedBy = row.original.used_by
        if (usedBy.length === 0) return <span className="text-text-subtle">(unused)</span>
        return (
          <div className="flex flex-col gap-0.5">
            {usedBy.map((connector) => (
              <Link
                key={connector.id}
                to="/connectors"
                className="truncate font-medium text-text-link hover:underline"
              >
                <span className="font-mono">{connector.name}</span> MCP
              </Link>
            ))}
          </div>
        )
      },
    },
    {
      id: 'connection',
      header: 'Connection',
      enableSorting: false,
      meta: { tooltip: false },
      cell: ({ row }) => {
        const source = row.original
        const state = tests[source.id]
        return (
          <div className="flex min-w-0 items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              aria-label={`Test ${source.name}`}
              disabled={state?.status === 'testing'}
              onClick={() => runTest(source)}
            >
              {state?.status === 'testing' ? 'Testing…' : 'Test'}
            </Button>
            {state?.status === 'ok' ? <StatusPill tone="positive" label="OK" /> : null}
            {state?.status === 'failed' ? (
              <span
                className="truncate text-xs text-destructive"
                title={state.error}
                role="status"
              >
                {state.error}
              </span>
            ) : null}
          </div>
        )
      },
    },
  ]

  const rowActions: RowAction<CredentialSource>[] = [
    { label: 'Edit', icon: <Pencil aria-hidden="true" />, onClick: setEditTarget },
    {
      label: 'Test connection',
      icon: <Plug aria-hidden="true" />,
      onClick: runTest,
    },
    {
      label: 'Disable',
      icon: <Power aria-hidden="true" />,
      hidden: (s) => !s.enabled,
      disabled: () => updateSource.isPending,
      onClick: (s) => toggleEnabled(s, false),
    },
    {
      label: 'Enable',
      icon: <Power aria-hidden="true" />,
      hidden: (s) => s.enabled,
      disabled: () => updateSource.isPending,
      onClick: (s) => toggleEnabled(s, true),
    },
    {
      label: 'Delete',
      icon: <Trash2 aria-hidden="true" />,
      variant: 'destructive',
      separatorBefore: true,
      onClick: (s) => {
        setDeleteError(null)
        setDeleteTarget(s)
      },
    },
  ]

  return (
    <>
      <DataTable
        storageKey="credential-sources"
        columns={columns}
        data={items}
        getRowId={(s) => s.id}
        getRowLabel={(s) => s.name}
        rowActions={rowActions}
        loading={sourcesQuery.isLoading}
        error={sourcesQuery.isError && "Couldn't load credential sources."}
        emptyTitle="No credential sources yet"
        emptyDescription="Register an outside credential service so MCP headers can read their secrets from it."
        emptyAction={
          <Button onClick={() => onCreateOpenChange(true)}>
            <Plus className="size-4" aria-hidden="true" />
            Add source
          </Button>
        }
      />

      <SourceFormDialog
        open={createOpen}
        source={null}
        onOpenChange={onCreateOpenChange}
      />
      <SourceFormDialog
        open={editTarget !== null}
        source={editTarget}
        onOpenChange={(open) => !open && setEditTarget(null)}
      />
      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setDeleteTarget(null)
            setDeleteError(null)
          }
        }}
        title={`Delete "${deleteTarget?.name}"?`}
        description={
          <span className="flex flex-col gap-2">
            <span className="block">
              MCP headers cannot name this source afterwards. This can&apos;t be undone.
            </span>
            {deleteError ? (
              <span
                role="alert"
                className="block rounded-r-3 border border-sev-high/40 bg-sev-high-bg px-2.5 py-2 text-sev-high-fg"
              >
                {deleteError}
              </span>
            ) : null}
          </span>
        }
        confirmLabel="Delete"
        destructive
        isLoading={deleteSource.isPending}
        onConfirm={handleDelete}
      />
    </>
  )
}
