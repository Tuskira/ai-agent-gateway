import { useState } from 'react'
import { toast } from 'sonner'
import { useUpdateUser } from '@/lib/queries'
import { describeUserError, type User } from '@/lib/users'
import type { UserRole } from '@/lib/types'
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

interface EditUserDialogProps {
  user: User | null
  onOpenChange: (open: boolean) => void
  /** Offer the `platform-admin` role (platform sessions only; the API
   * answers 403 to anyone else). */
  allowPlatformAdmin?: boolean
}

/** Outer shell: the form is keyed by user id so its draft state starts fresh
 * for each user without an effect. */
export function EditUserDialog({
  user,
  onOpenChange,
  allowPlatformAdmin = false,
}: EditUserDialogProps) {
  return (
    <Dialog open={user !== null} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        {user ? (
          <EditUserForm
            key={user.id}
            user={user}
            allowPlatformAdmin={allowPlatformAdmin}
            onClose={() => onOpenChange(false)}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  )
}

function EditUserForm({
  user,
  allowPlatformAdmin,
  onClose,
}: {
  user: User
  allowPlatformAdmin: boolean
  onClose: () => void
}) {
  const updateUser = useUpdateUser()
  const [displayName, setDisplayName] = useState(user.display_name)
  const [role, setRole] = useState<UserRole>(user.role)
  const [error, setError] = useState<string | null>(null)

  function handleSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setError(null)
    updateUser.mutate(
      {
        id: user.id,
        input: {
          ...(displayName.trim() !== user.display_name
            ? { display_name: displayName.trim() }
            : {}),
          ...(role !== user.role ? { role } : {}),
        },
      },
      {
        onSuccess: () => {
          toast.success('User updated')
          onClose()
        },
        onError: (err) => setError(describeUserError(err, 'Failed to update user')),
      },
    )
  }

  return (
    <form onSubmit={handleSubmit}>
      <DialogHeader>
        <DialogTitle>Edit {user.username}</DialogTitle>
        <DialogDescription>The username can&apos;t be changed.</DialogDescription>
      </DialogHeader>
      <div className="flex flex-col gap-4 py-4">
        {error ? (
          <div
            role="alert"
            className="rounded-r-3 border border-destructive/20 bg-destructive/10 px-3 py-2 text-sm text-destructive"
          >
            {error}
          </div>
        ) : null}
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="edit-display-name">Display name</Label>
          <Input
            id="edit-display-name"
            autoComplete="off"
            value={displayName}
            onChange={(event) => setDisplayName(event.target.value)}
          />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="edit-role">Role</Label>
          <Select value={role} onValueChange={(v) => setRole(v as UserRole)}>
            <SelectTrigger id="edit-role" className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="admin">admin</SelectItem>
              <SelectItem value="viewer">viewer</SelectItem>
              {/* A tenant admin cannot grant or revoke platform-admin, but
                  the current role must stay selectable so the picker is
                  not blank when they open a platform admin's row. */}
              {allowPlatformAdmin || user.role === 'platform-admin' ? (
                <SelectItem value="platform-admin" disabled={!allowPlatformAdmin}>
                  platform-admin
                </SelectItem>
              ) : null}
            </SelectContent>
          </Select>
        </div>
      </div>
      <DialogFooter>
        <Button type="button" variant="outline" onClick={onClose}>
          Cancel
        </Button>
        <Button type="submit" disabled={updateUser.isPending}>
          {updateUser.isPending ? 'Saving…' : 'Save'}
        </Button>
      </DialogFooter>
    </form>
  )
}
