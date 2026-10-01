import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { LayoutGrid, Pencil, Plus, Trash2, Wrench } from 'lucide-react'
import { toast } from 'sonner'
import { useAllProfileTools, useDeleteProfile, useProfilesList } from '@/lib/queries'
import type { Profile } from '@/lib/profiles'
import { formatDate } from '@/lib/utils'
import {
  DataTable,
  DataTablePage,
  type ColumnDef,
  type RowAction,
} from '@/components/app/data-table'
import { ConfirmDialog } from '@/components/app/ConfirmDialog'
import { ProfileFormDialog } from '@/components/app/profiles/ProfileFormDialog'
import { ProfileStudioModal } from '@/components/app/profiles/ProfileStudioModal'
import { Button } from '@/components/ui/button'

export default function ProfilesPage() {
  const navigate = useNavigate()
  const profilesQuery = useProfilesList()
  const deleteProfile = useDeleteProfile()
  const profiles = profilesQuery.data?.items ?? []
  const toolsQueries = useAllProfileTools(profiles)
  const toolCountByProfile = new Map(
    profiles.map((p, i) => [p.id, toolsQueries[i]?.data?.total]),
  )

  const [createOpen, setCreateOpen] = useState(false)
  const [studioOpen, setStudioOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<Profile | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<Profile | null>(null)

  function handleDelete() {
    if (!deleteTarget) return
    deleteProfile.mutate(deleteTarget.id, {
      onSuccess: () => {
        toast.success('Profile deleted')
        setDeleteTarget(null)
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to delete profile')
      },
    })
  }

  const columns: ColumnDef<Profile>[] = [
    {
      id: 'name',
      accessorKey: 'name',
      header: 'Name',
      size: 200,
      cell: ({ row }) => (
        <span className="font-semibold text-foreground">{row.original.name}</span>
      ),
    },
    {
      id: 'slug',
      accessorKey: 'slug',
      header: 'Slug',
      cell: ({ row }) => (
        <span className="font-mono text-xs text-text-muted">{row.original.slug}</span>
      ),
    },
    {
      id: 'description',
      accessorFn: (p) => p.description ?? '',
      header: 'Description',
      size: 280,
      cell: ({ row }) => (
        <span className="text-text-subtle">
          {row.original.description || 'No description'}
        </span>
      ),
    },
    {
      id: 'tools',
      accessorFn: (p) => toolCountByProfile.get(p.id),
      header: 'Tools',
      size: 90,
      sortUndefined: 'last',
      cell: ({ row }) => (
        <span className="tabular-nums">
          {toolCountByProfile.get(row.original.id) ?? '—'}
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
  ]

  const rowActions: RowAction<Profile>[] = [
    {
      label: 'Manage tools',
      icon: <Wrench aria-hidden="true" />,
      onClick: (p) => navigate(`/profiles/${p.id}/tools`),
    },
    { label: 'Edit', icon: <Pencil aria-hidden="true" />, onClick: setEditTarget },
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
            Agent Profiles
          </h1>
          <p className="text-[13px] text-text-subtle">
            Group tools from multiple MCPs into reusable profiles.
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" onClick={() => setStudioOpen(true)}>
            <LayoutGrid className="size-3.5" aria-hidden="true" />
            Profile Studio
          </Button>
          <Button onClick={() => setCreateOpen(true)}>
            <Plus className="size-4" aria-hidden="true" />
            Create Profile
          </Button>
        </div>
      </div>

      <DataTable
        storageKey="profiles"
        columns={columns}
        data={profiles}
        getRowId={(p) => p.id}
        getRowLabel={(p) => p.name}
        rowActions={rowActions}
        loading={profilesQuery.isLoading}
        error={profilesQuery.isError && "Couldn't load profiles."}
        emptyTitle="No profiles yet"
        emptyDescription="Create a profile to scope which tools an agent can use."
      />

      <ProfileStudioModal open={studioOpen} onOpenChange={setStudioOpen} />
      <ProfileFormDialog open={createOpen} onOpenChange={setCreateOpen} />
      <ProfileFormDialog
        open={editTarget !== null}
        onOpenChange={(open) => !open && setEditTarget(null)}
        profile={editTarget}
      />
      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        title={`Delete "${deleteTarget?.name}"?`}
        description="Agents using this profile will lose access to its tools. This can't be undone."
        confirmLabel="Delete"
        destructive
        isLoading={deleteProfile.isPending}
        onConfirm={handleDelete}
      />
    </DataTablePage>
  )
}
