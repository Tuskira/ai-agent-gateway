import { useState } from 'react'
import { toast } from 'sonner'
import { ApiError } from '@/lib/api'
import { useRenameTenant } from '@/lib/queries'
import { TENANT_NAME_MAX, type Tenant } from '@/lib/tenants'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

interface RenameTenantDialogProps {
  /** The tenant being renamed; the dialog is open while this is set. */
  tenant: Tenant | null
  onOpenChange: (open: boolean) => void
}

function describeError(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 403) return 'Renaming tenants needs a platform admin.'
    if (err.status === 404) return 'This tenant no longer exists.'
    return err.message || 'Failed to rename tenant'
  }
  return 'Failed to rename tenant'
}

export function RenameTenantDialog({ tenant, onOpenChange }: RenameTenantDialogProps) {
  // Re-mounted per tenant (see key below) so the field starts from its name.
  return (
    <Dialog open={tenant !== null} onOpenChange={onOpenChange}>
      {tenant ? (
        <RenameForm key={tenant.id} tenant={tenant} onOpenChange={onOpenChange} />
      ) : null}
    </Dialog>
  )
}

function RenameForm({
  tenant,
  onOpenChange,
}: {
  tenant: Tenant
  onOpenChange: (open: boolean) => void
}) {
  const rename = useRenameTenant()
  const [name, setName] = useState(tenant.name)
  const [error, setError] = useState<string | null>(null)

  function submit(event: React.FormEvent) {
    event.preventDefault()
    const next = name.trim()
    if (next.length < 1 || next.length > TENANT_NAME_MAX) {
      setError(`Name must be 1 to ${TENANT_NAME_MAX} characters.`)
      return
    }
    setError(null)
    rename.mutate(
      { id: tenant.id, name: next },
      {
        onSuccess: (updated) => {
          toast.success(`Tenant renamed to ${updated.name}`)
          onOpenChange(false)
        },
        onError: (err) => setError(describeError(err)),
      },
    )
  }

  return (
    <DialogContent className="sm:max-w-md">
      <form onSubmit={submit} noValidate>
        <DialogHeader>
          <DialogTitle>Rename tenant</DialogTitle>
          <DialogDescription>
            Changes the display name only. The slug ({tenant.slug}) and id stay the same.
          </DialogDescription>
        </DialogHeader>
        <div className="flex flex-col gap-3 py-4">
          {error ? (
            <div
              role="alert"
              className="rounded-r-3 border border-destructive/20 bg-destructive/10 px-3 py-2 text-sm text-destructive"
            >
              {error}
            </div>
          ) : null}
          <Label htmlFor="rename-tenant-name">Name</Label>
          <Input
            id="rename-tenant-name"
            autoComplete="off"
            autoFocus
            value={name}
            onChange={(event) => setName(event.target.value)}
          />
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={rename.isPending || name.trim() === tenant.name}
          >
            {rename.isPending ? 'Saving…' : 'Save'}
          </Button>
        </DialogFooter>
      </form>
    </DialogContent>
  )
}
