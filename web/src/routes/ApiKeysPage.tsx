import { useState } from 'react'
import { KeyRound, Plus, RefreshCw, Ban } from 'lucide-react'
import { toast } from 'sonner'
import { useAuth } from '@/auth/AuthContext'
import { ApiError } from '@/lib/api'
import { useApiKeysList, useDeleteApiKey, useRotateApiKey } from '@/lib/queries'
import type { ApiKey, ApiKeyCreated } from '@/lib/api-keys'
import { formatDate, shortId } from '@/lib/utils'
import {
  DataTable,
  DataTablePage,
  type ColumnDef,
  type RowAction,
} from '@/components/app/data-table'
import { ConfirmDialog } from '@/components/app/ConfirmDialog'
import { DismissibleBanner } from '@/components/app/DismissibleBanner'
import { StatusPill } from '@/components/app/StatusPill'
import { CreateApiKeyDialog } from '@/components/app/api-keys/CreateApiKeyDialog'
import { RevealApiKeyDialog } from '@/components/app/api-keys/RevealApiKeyDialog'
import { Button } from '@/components/ui/button'

const ROLE_CLASS: Record<string, string> = {
  admin: 'bg-status-open-bg text-status-open',
  agent: 'bg-bg-muted text-text-muted',
  interceptor: 'bg-sev-medium-bg text-sev-medium',
}

export default function ApiKeysPage() {
  const { principal } = useAuth()
  const apiKeysQuery = useApiKeysList()
  const rotateApiKey = useRotateApiKey()
  const deleteApiKey = useDeleteApiKey()

  const [createOpen, setCreateOpen] = useState(false)
  const [revealed, setRevealed] = useState<ApiKeyCreated | null>(null)
  const [rotateTarget, setRotateTarget] = useState<ApiKey | null>(null)
  const [revokeTarget, setRevokeTarget] = useState<ApiKey | null>(null)

  const isForbidden =
    apiKeysQuery.error instanceof ApiError && apiKeysQuery.error.status === 403
  const items = apiKeysQuery.data?.items ?? []

  function handleRotate() {
    if (!rotateTarget) return
    rotateApiKey.mutate(rotateTarget.id, {
      onSuccess: (created) => {
        toast.success('Key rotated')
        setRotateTarget(null)
        setRevealed(created)
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to rotate key')
      },
    })
  }

  function handleRevoke() {
    if (!revokeTarget) return
    deleteApiKey.mutate(revokeTarget.id, {
      onSuccess: () => {
        toast.success('Key revoked')
        setRevokeTarget(null)
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to revoke key')
      },
    })
  }

  const columns: ColumnDef<ApiKey>[] = [
    {
      id: 'name',
      accessorKey: 'name',
      header: 'Name',
      size: 200,
      cell: ({ row }) => (
        <span className="font-medium text-foreground">{row.original.name}</span>
      ),
    },
    {
      id: 'prefix',
      accessorKey: 'prefix',
      header: 'Prefix',
      cell: ({ row }) => (
        <span className="font-mono text-xs text-text-muted">{row.original.prefix}</span>
      ),
    },
    {
      id: 'role',
      accessorKey: 'role',
      header: 'Role',
      size: 110,
      cell: ({ row }) => (
        <span
          className={`inline-flex h-[22px] items-center rounded-r-2 px-2 text-[11.5px] font-bold ${ROLE_CLASS[row.original.role] ?? ROLE_CLASS.agent}`}
        >
          {row.original.role}
        </span>
      ),
    },
    {
      id: 'profile',
      accessorFn: (k) => k.profile_name ?? '',
      header: 'Profile',
      cell: ({ row }) =>
        row.original.profile_id ? (
          <span className="text-xs">
            {row.original.profile_name || shortId(row.original.profile_id)}
          </span>
        ) : (
          <span className="text-xs text-text-muted">any</span>
        ),
    },
    {
      id: 'tenant',
      header: 'Tenant',
      enableSorting: false,
      cell: () => (
        <span className="font-mono text-xs text-text-muted">
          {principal ? shortId(principal.tenant_id) : '—'}
        </span>
      ),
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
      id: 'last_used_at',
      accessorFn: (k) => k.last_used_at ?? '',
      header: 'Last used',
      meta: { tooltip: false },
      cell: ({ row }) => (
        <span className="text-text-muted">{formatDate(row.original.last_used_at)}</span>
      ),
    },
    {
      id: 'expires_at',
      accessorFn: (k) => k.expires_at ?? '',
      header: 'Expires',
      meta: { tooltip: false },
      cell: ({ row }) => (
        <span className="text-text-muted">{formatDate(row.original.expires_at)}</span>
      ),
    },
    {
      id: 'status',
      accessorFn: (k) => (k.revoked_at ? 'Revoked' : 'Active'),
      header: 'Status',
      size: 120,
      cell: ({ row }) =>
        row.original.revoked_at ? (
          <StatusPill tone="negative" label="Revoked" />
        ) : (
          <StatusPill tone="positive" label="Active" />
        ),
    },
  ]

  // A revoked key can be neither rotated nor revoked again.
  const rowActions: RowAction<ApiKey>[] = [
    {
      label: 'Rotate',
      icon: <RefreshCw aria-hidden="true" />,
      hidden: (k) => Boolean(k.revoked_at),
      onClick: setRotateTarget,
    },
    {
      label: 'Revoke',
      icon: <Ban aria-hidden="true" />,
      variant: 'destructive',
      separatorBefore: true,
      hidden: (k) => Boolean(k.revoked_at),
      onClick: setRevokeTarget,
    },
  ]

  return (
    <DataTablePage>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
            API keys
          </h1>
          <p className="text-[13px] text-text-subtle">
            Keys that authenticate people and agents to the gateway.
          </p>
        </div>
        {!isForbidden ? (
          <Button onClick={() => setCreateOpen(true)}>
            <Plus className="size-4" aria-hidden="true" />
            Create key
          </Button>
        ) : null}
      </div>

      {isForbidden ? (
        <div className="flex flex-col items-center gap-3 rounded-r-5 border border-border bg-card px-6 py-14 text-center">
          <span className="flex size-[52px] items-center justify-center rounded-r-5 bg-bg-muted text-text-muted">
            <KeyRound className="size-[22px]" aria-hidden="true" />
          </span>
          <div className="text-base font-semibold text-foreground">
            Admin key required
          </div>
          <p className="max-w-[46ch] text-[13.5px] text-text-subtle">
            These settings need an admin key. You&apos;re signed in with an agent key.
          </p>
        </div>
      ) : (
        <>
          <DismissibleBanner storageKey="gateway.apiKeysBannerDismissed">
            Keys are shown once at creation and stored hashed. Rotate or revoke anytime.
          </DismissibleBanner>
          <DataTable
            storageKey="api-keys"
            columns={columns}
            data={items}
            getRowId={(k) => k.id}
            getRowLabel={(k) => k.name}
            getRowClassName={(k) => (k.revoked_at ? 'opacity-55' : undefined)}
            rowActions={rowActions}
            loading={apiKeysQuery.isLoading}
            error={apiKeysQuery.isError && "Couldn't load API keys."}
            emptyTitle="No API keys yet"
            emptyDescription="Create a key so people or agents can authenticate to the gateway."
          />
        </>
      )}

      <CreateApiKeyDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        onCreated={(created) => setRevealed(created)}
      />
      <RevealApiKeyDialog apiKey={revealed} onOpenChange={() => setRevealed(null)} />
      <ConfirmDialog
        open={rotateTarget !== null}
        onOpenChange={(open) => !open && setRotateTarget(null)}
        title={`Rotate "${rotateTarget?.name}"?`}
        description="The current key stops working immediately. A new key is issued in its place."
        confirmLabel="Rotate"
        isLoading={rotateApiKey.isPending}
        onConfirm={handleRotate}
      />
      <ConfirmDialog
        open={revokeTarget !== null}
        onOpenChange={(open) => !open && setRevokeTarget(null)}
        title={`Revoke "${revokeTarget?.name}"?`}
        description="This key stops working immediately. This can't be undone."
        confirmLabel="Revoke"
        destructive
        isLoading={deleteApiKey.isPending}
        onConfirm={handleRevoke}
      />
    </DataTablePage>
  )
}
