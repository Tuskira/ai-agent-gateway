import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { Pencil, Plus, RefreshCw, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { useCredentialsList, useDeleteCredential, useModelsRegistryList } from '@/lib/queries'
import type { Credential } from '@/lib/credentials'
import { formatDate } from '@/lib/utils'
import {
  DataTable,
  DataTablePage,
  type ColumnDef,
  type RowAction,
} from '@/components/app/data-table'
import { ConfirmDialog } from '@/components/app/ConfirmDialog'
import { DismissibleBanner } from '@/components/app/DismissibleBanner'
import { CredentialFormDialog } from '@/components/app/credentials/CredentialFormDialog'
import { RotateCredentialDialog } from '@/components/app/credentials/RotateCredentialDialog'
import { Button } from '@/components/ui/button'

export default function CredentialsPage() {
  const credentialsQuery = useCredentialsList()
  const deleteCredential = useDeleteCredential()
  // Model registry rows, only to warn a credential delete about models that
  // reference it (`targets[].credential`) — a 403 here (an agent-key
  // session) just means "no warning to show", never a broken page.
  const modelsQuery = useModelsRegistryList()

  const [createOpen, setCreateOpen] = useState(false)
  const [rotateTarget, setRotateTarget] = useState<Credential | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<Credential | null>(null)

  const items = credentialsQuery.data?.items ?? []

  const referencingModels = useMemo(() => {
    const models = modelsQuery.data?.items ?? []
    if (!deleteTarget) return []
    return models.filter((m) => m.targets.some((t) => t.credential === deleteTarget.name))
  }, [modelsQuery.data, deleteTarget])

  function handleDelete() {
    if (!deleteTarget) return
    deleteCredential.mutate(deleteTarget.name, {
      onSuccess: () => {
        toast.success('Credential deleted')
        setDeleteTarget(null)
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to delete credential')
      },
    })
  }

  const columns: ColumnDef<Credential>[] = [
    {
      id: 'name',
      accessorKey: 'name',
      header: 'Name',
      size: 240,
      cell: ({ row }) => (
        <div className="flex items-center gap-2">
          <span className="truncate font-mono font-semibold text-foreground">
            {row.original.name}
          </span>
          <span className="inline-flex h-5 shrink-0 items-center rounded-r-pill border border-border bg-bg-subtle px-2 text-[11px] text-text-subtle">
            Secret store
          </span>
        </div>
      ),
    },
    {
      id: 'fields',
      accessorFn: (c) => c.field_names.length,
      header: 'Fields',
      cell: ({ row }) => {
        const names = row.original.field_names
        return (
          <div>
            <div className="text-[13px] text-foreground">
              {names.length} field{names.length === 1 ? '' : 's'}
            </div>
            <div className="truncate font-mono text-[11.5px] text-text-subtle">
              {names.join(', ')}
            </div>
          </div>
        )
      },
    },
    {
      id: 'used_by',
      accessorFn: (c) => (c.used_by ?? []).length,
      header: 'Used by',
      cell: ({ row }) => {
        const usedBy = row.original.used_by ?? []
        if (usedBy.length === 0) {
          return <span className="text-text-subtle">(unused)</span>
        }
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
      id: 'created_at',
      accessorKey: 'created_at',
      header: 'Created',
      meta: { tooltip: false },
      cell: ({ row }) => (
        <span className="text-text-muted">{formatDate(row.original.created_at)}</span>
      ),
    },
    {
      id: 'updated_at',
      accessorFn: (c) => c.rotated_at ?? c.created_at,
      header: 'Updated',
      meta: { tooltip: false },
      cell: ({ row }) => (
        <span className="text-text-muted">
          {formatDate(row.original.rotated_at ?? row.original.created_at)}
        </span>
      ),
    },
  ]

  // Edit and Rotate value both open the rotate dialog: a credential's value is
  // the only thing about it that can change.
  const rowActions: RowAction<Credential>[] = [
    { label: 'Edit', icon: <Pencil aria-hidden="true" />, onClick: setRotateTarget },
    {
      label: 'Rotate value',
      icon: <RefreshCw aria-hidden="true" />,
      onClick: setRotateTarget,
    },
    {
      label: 'Delete',
      icon: <Trash2 aria-hidden="true" />,
      variant: 'destructive',
      separatorBefore: true,
      onClick: setDeleteTarget,
    },
  ]

  return (
    <DataTablePage>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
            Credentials
          </h1>
          <p className="text-[13px] text-text-subtle">
            Secrets the gateway injects when calling your MCPs.
          </p>
        </div>
        <Button onClick={() => setCreateOpen(true)}>
          <Plus className="size-4" aria-hidden="true" />
          Add credential
        </Button>
      </div>

      <DismissibleBanner storageKey="gateway.credentialsBannerDismissed">
        Values are encrypted at rest (AES-256-GCM) and never shown after creation. Rotate
        to replace a value.
      </DismissibleBanner>

      <DataTable
        storageKey="credentials"
        columns={columns}
        data={items}
        getRowId={(c) => c.id}
        getRowLabel={(c) => c.name}
        rowActions={rowActions}
        loading={credentialsQuery.isLoading}
        error={credentialsQuery.isError && "Couldn't load credentials."}
        emptyTitle="No credentials yet"
        emptyDescription="Add a secret so the gateway can authenticate to your MCPs."
      />

      <CredentialFormDialog open={createOpen} onOpenChange={setCreateOpen} />
      <RotateCredentialDialog
        credential={rotateTarget}
        onOpenChange={(open) => !open && setRotateTarget(null)}
      />
      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        title={`Delete "${deleteTarget?.name}"?`}
        description={
          // ConfirmDialog's description renders inside Base UI's
          // AlertDialog.Description, a <p> -- only phrasing content is
          // valid inside it, so these are <span>s laid out as blocks via
          // Tailwind rather than actual <div>/<p> elements.
          <span className="flex flex-col gap-2">
            <span className="block">
              Any MCP referencing this credential will fail to authenticate. This
              can&apos;t be undone.
            </span>
            {referencingModels.length > 0 ? (
              <span className="block rounded-r-3 border border-sev-medium/40 bg-sev-medium-bg px-2.5 py-2 text-sev-medium-fg">
                Also used by {referencingModels.length} model
                {referencingModels.length === 1 ? '' : 's'}:{' '}
                {referencingModels.map((m) => m.name).join(', ')}. Calls to{' '}
                {referencingModels.length === 1 ? 'it' : 'them'} will start failing too.
              </span>
            ) : null}
          </span>
        }
        confirmLabel="Delete"
        destructive
        isLoading={deleteCredential.isPending}
        onConfirm={handleDelete}
      />
    </DataTablePage>
  )
}
