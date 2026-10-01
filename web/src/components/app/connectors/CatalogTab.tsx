import { useState } from 'react'
import {
  Activity,
  BookOpen,
  CreditCard,
  Database,
  GitBranch,
  HardDrive,
  Kanban,
  Library,
  Pencil,
  Plug,
  Plus,
  Shapes,
  Smile,
  Trash2,
  type LucideIcon,
} from 'lucide-react'
import { toast } from 'sonner'
import { useDeleteCatalogEntry, useMcpCatalog } from '@/lib/queries'
import { authKindLabel, type AddCatalogResult, type CatalogEntry } from '@/lib/mcpCatalog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/app/ConfirmDialog'
import { CatalogAddDialog } from '@/components/app/connectors/CatalogAddDialog'
import { CatalogEntryFormDialog } from '@/components/app/connectors/CatalogEntryFormDialog'
import { LifecyclePill } from '@/components/app/LifecycleBits'
import { catalogEntryState, type LifecycleState } from '@/lib/lifecycle'

const ICONS: Record<string, LucideIcon> = {
  activity: Activity,
  'book-open': BookOpen,
  'credit-card': CreditCard,
  database: Database,
  'git-branch': GitBranch,
  'hard-drive': HardDrive,
  kanban: Kanban,
  library: Library,
  shapes: Shapes,
  smile: Smile,
}

function CatalogCard({
  entry,
  canManage,
  state,
  onAdd,
  onEdit,
  onDelete,
}: {
  entry: CatalogEntry
  state: LifecycleState
  canManage: boolean
  onAdd: (entry: CatalogEntry) => void
  onEdit: (entry: CatalogEntry) => void
  onDelete: (entry: CatalogEntry) => void
}) {
  const Icon = ICONS[entry.icon] ?? Plug
  // Platform entries are shared by every tenant: read-only in the console.
  const editable = canManage && entry.scope === 'tenant'
  return (
    <div
      data-testid={`catalog-card-${entry.slug}`}
      className="flex flex-col gap-3 rounded-xl border border-border bg-card p-4"
    >
      <div className="flex items-start gap-3">
        <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-muted text-foreground">
          <Icon className="size-4" aria-hidden="true" />
        </span>
        <div className="min-w-0 flex-1">
          <div className="truncate text-[14px] font-semibold text-foreground">
            {entry.name}
          </div>
          <div className="text-[11.5px] text-text-subtle">
            {authKindLabel(entry.auth.kind)}
            {entry.category ? ` · ${entry.category}` : ''}
          </div>
        </div>
        <div className="flex shrink-0 flex-col items-end gap-1">
          {entry.enabled || entry.added ? <LifecyclePill state={state} /> : null}
          {entry.scope === 'platform' ? (
            <Badge variant="outline" className="text-[10px] uppercase">
              Platform
            </Badge>
          ) : null}
          {!entry.enabled ? (
            <Badge variant="secondary" className="text-[10px] uppercase">
              Disabled
            </Badge>
          ) : null}
        </div>
      </div>
      <p className="flex-1 text-[13px] text-text-muted">{entry.description}</p>
      {!entry.supported ? (
        <p className="text-[12px] text-text-subtle">Requires OAuth — not supported yet</p>
      ) : null}
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-1">
          {editable ? (
            <>
              <Button
                size="icon"
                variant="ghost"
                aria-label={`Edit ${entry.name}`}
                onClick={() => onEdit(entry)}
              >
                <Pencil className="size-4" aria-hidden="true" />
              </Button>
              <Button
                size="icon"
                variant="ghost"
                aria-label={`Delete ${entry.name}`}
                onClick={() => onDelete(entry)}
              >
                <Trash2 className="size-4" aria-hidden="true" />
              </Button>
            </>
          ) : null}
        </div>
        {entry.added ? (
          <Button size="sm" variant="outline" disabled>
            Added
          </Button>
        ) : (
          <Button
            size="sm"
            disabled={!entry.supported || !entry.enabled}
            aria-label={`Add ${entry.name}`}
            onClick={() => onAdd(entry)}
          >
            Add
          </Button>
        )}
      </div>
    </div>
  )
}

/**
 * The "Catalog" tab of the MCPs page: curated MCP servers to add to the
 * tenant. With `canManage`, admins can also create, edit and delete the
 * tenant's own entries (platform entries stay read-only).
 */
export function CatalogTab({
  canManage = false,
  onAdded,
  stateByConnectorId,
}: {
  canManage?: boolean
  onAdded?: (result: AddCatalogResult) => void
  /** State of each tenant connector, so an added entry shows its real state
   * (Active / Registered / Disabled); a not-added entry is Available. */
  stateByConnectorId?: ReadonlyMap<string, LifecycleState>
}) {
  const catalog = useMcpCatalog()
  const deleteEntry = useDeleteCatalogEntry()
  const [addTarget, setAddTarget] = useState<CatalogEntry | null>(null)
  const [formOpen, setFormOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<CatalogEntry | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<CatalogEntry | null>(null)

  if (catalog.isLoading) {
    return <p className="text-[13px] text-text-subtle">Loading catalog...</p>
  }
  if (catalog.isError) {
    return <p className="text-[13px] text-destructive">Couldn&apos;t load the catalog.</p>
  }
  const entries = catalog.data?.items ?? []

  function handleDelete() {
    if (!deleteTarget) return
    deleteEntry.mutate(deleteTarget.slug, {
      onSuccess: () => {
        toast.success('Catalog entry deleted')
        setDeleteTarget(null)
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to delete catalog entry')
      },
    })
  }

  return (
    <>
      {canManage ? (
        <div className="flex justify-end">
          <Button
            size="sm"
            onClick={() => {
              setEditTarget(null)
              setFormOpen(true)
            }}
          >
            <Plus className="size-4" aria-hidden="true" />
            New catalog entry
          </Button>
        </div>
      ) : null}

      {entries.length === 0 ? (
        <p className="text-[13px] text-text-subtle">The catalog is empty.</p>
      ) : (
        <div className="grid grid-cols-[repeat(auto-fill,minmax(260px,1fr))] gap-4">
          {entries.map((e) => (
            <CatalogCard
              key={e.id}
              entry={e}
              state={catalogEntryState(e, stateByConnectorId)}
              canManage={canManage}
              onAdd={setAddTarget}
              onEdit={(entry) => {
                setEditTarget(entry)
                setFormOpen(true)
              }}
              onDelete={setDeleteTarget}
            />
          ))}
        </div>
      )}

      <CatalogAddDialog
        entry={addTarget}
        onOpenChange={(open) => !open && setAddTarget(null)}
        onAdded={onAdded}
      />
      <CatalogEntryFormDialog
        open={formOpen}
        entry={editTarget}
        onOpenChange={(open) => {
          setFormOpen(open)
          if (!open) setEditTarget(null)
        }}
      />
      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        title={`Delete "${deleteTarget?.name}"?`}
        description="MCPs already added from this entry keep working. A platform entry with the same slug becomes visible again."
        confirmLabel="Delete"
        destructive
        isLoading={deleteEntry.isPending}
        onConfirm={handleDelete}
      />
    </>
  )
}
