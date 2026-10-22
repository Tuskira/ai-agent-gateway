import { useState } from 'react'
import { ArrowRightLeft, Building2, Pencil, Plus } from 'lucide-react'
import { toast } from 'sonner'
import { useAuth } from '@/auth/AuthContext'
import { ApiError } from '@/lib/api'
import { useTenants } from '@/lib/queries'
import type { Tenant } from '@/lib/tenants'
import { formatDateTime } from '@/lib/utils'
import { CopyButton } from '@/components/app/CopyButton'
import {
  DataTable,
  DataTablePage,
  type ColumnDef,
  type RowAction,
} from '@/components/app/data-table'
import { StatusPill } from '@/components/app/StatusPill'
import { CreateTenantDialog } from '@/components/app/tenants/CreateTenantDialog'
import { RenameTenantDialog } from '@/components/app/tenants/RenameTenantDialog'
import { Button } from '@/components/ui/button'

/**
 * Platform-wide list of tenants (`GET /tenants`, platform.admin only). This
 * page does not follow the acting tenant: whichever tenant the switcher
 * selects, the same rows show. "Act in this tenant" is a shortcut to the
 * switcher for the chosen row.
 */
export default function TenantsPage() {
  const { isPlatformAdmin, actingTenantId, switchTenant } = useAuth()
  const tenantsQuery = useTenants(isPlatformAdmin)
  const [createOpen, setCreateOpen] = useState(false)
  const [renaming, setRenaming] = useState<Tenant | null>(null)

  const isForbidden =
    !isPlatformAdmin ||
    (tenantsQuery.error instanceof ApiError && tenantsQuery.error.status === 403)
  const items = tenantsQuery.data?.items ?? []
  const total = tenantsQuery.data?.total ?? items.length

  function actIn(tenant: Tenant) {
    switchTenant(tenant.id)
    toast.success(`Now acting in ${tenant.name}`)
  }

  const columns: ColumnDef<Tenant>[] = [
    {
      id: 'name',
      accessorKey: 'name',
      header: 'Name',
      size: 220,
      cell: ({ row }) => (
        <span className="flex items-center gap-2">
          <span className="font-medium text-foreground">{row.original.name}</span>
          {row.original.id === actingTenantId ? (
            <StatusPill tone="positive" label="Acting" />
          ) : null}
        </span>
      ),
    },
    {
      id: 'slug',
      accessorKey: 'slug',
      header: 'Slug',
      size: 160,
      cell: ({ row }) => (
        <span className="font-mono text-xs text-text-muted">{row.original.slug}</span>
      ),
    },
    {
      id: 'id',
      accessorKey: 'id',
      header: 'ID',
      meta: { tooltip: false },
      cell: ({ row }) => (
        <span className="flex items-center gap-1.5">
          <span className="truncate font-mono text-xs text-text-muted">
            {row.original.id}
          </span>
          <CopyButton
            value={row.original.id}
            label={`Copy id of ${row.original.name}`}
            size="icon-xs"
            variant="ghost"
          />
        </span>
      ),
    },
    {
      id: 'created_at',
      accessorKey: 'created_at',
      header: 'Created',
      size: 190,
      meta: { tooltip: false },
      cell: ({ row }) => (
        <span className="text-text-muted">{formatDateTime(row.original.created_at)}</span>
      ),
    },
  ]

  const rowActions: RowAction<Tenant>[] = [
    {
      label: 'Rename',
      icon: <Pencil aria-hidden="true" />,
      onClick: setRenaming,
    },
    {
      label: 'Act in this tenant',
      icon: <ArrowRightLeft aria-hidden="true" />,
      hidden: (t) => t.id === actingTenantId,
      onClick: actIn,
    },
  ]

  return (
    <DataTablePage>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-[22px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
            Tenants{tenantsQuery.isSuccess ? ` (${total})` : ''}
          </h1>
          <p className="text-[13px] text-text-subtle">
            Platform-wide: every tenant on this gateway, whichever tenant you are acting
            in.
          </p>
        </div>
        {!isForbidden ? (
          <Button onClick={() => setCreateOpen(true)}>
            <Plus className="size-4" aria-hidden="true" />
            Create tenant
          </Button>
        ) : null}
      </div>

      {isForbidden ? (
        <div className="flex flex-col items-center gap-3 rounded-r-5 border border-border bg-card px-6 py-14 text-center">
          <span className="flex size-[52px] items-center justify-center rounded-r-5 bg-bg-muted text-text-muted">
            <Building2 className="size-[22px]" aria-hidden="true" />
          </span>
          <div className="text-base font-semibold text-foreground">
            Platform admin required
          </div>
          <p className="max-w-[46ch] text-[13.5px] text-text-subtle">
            Tenants are managed by platform administrators. You&apos;re signed in with a
            role that can&apos;t.
          </p>
        </div>
      ) : (
        <DataTable
          storageKey="tenants"
          columns={columns}
          data={items}
          getRowId={(t) => t.id}
          getRowLabel={(t) => t.name}
          rowActions={rowActions}
          loading={tenantsQuery.isLoading}
          error={tenantsQuery.isError && "Couldn't load tenants."}
          emptyTitle="No tenants yet"
          emptyDescription="Create a tenant so its users can sign in."
        />
      )}

      <CreateTenantDialog open={createOpen} onOpenChange={setCreateOpen} />
      <RenameTenantDialog
        tenant={renaming}
        onOpenChange={(open) => {
          if (!open) setRenaming(null)
        }}
      />
    </DataTablePage>
  )
}
