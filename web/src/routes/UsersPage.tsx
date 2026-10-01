import { useState } from 'react'
import {
  Ban,
  CircleCheck,
  KeyRound,
  LogOut,
  Pencil,
  Plus,
  RotateCcw,
  Trash2,
  X,
} from 'lucide-react'
import { toast } from 'sonner'
import { useAuth } from '@/auth/AuthContext'
import { ApiError } from '@/lib/api'
import {
  useAuthAudit,
  useDeleteUser,
  useResetUserPassword,
  useRevokeUserSessions,
  useUpdateUser,
  useUsersList,
} from '@/lib/queries'
import { describeUserError, type AuditEntry, type User } from '@/lib/users'
import { formatDateTime } from '@/lib/utils'
import {
  DataTable,
  DataTablePage,
  type ColumnDef,
  type RowAction,
} from '@/components/app/data-table'
import { ConfirmDialog } from '@/components/app/ConfirmDialog'
import { StatusPill } from '@/components/app/StatusPill'
import { CreateUserDialog } from '@/components/app/users/CreateUserDialog'
import { EditUserDialog } from '@/components/app/users/EditUserDialog'
import {
  RevealPasswordDialog,
  type RevealedPassword,
} from '@/components/app/users/RevealPasswordDialog'
import { Button } from '@/components/ui/button'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'

const ROLE_CLASS: Record<string, string> = {
  admin: 'bg-status-open-bg text-status-open',
  viewer: 'bg-bg-muted text-text-muted',
}

function RoleBadge({ role }: { role: string }) {
  return (
    <span
      className={`inline-flex h-[22px] items-center rounded-r-2 px-2 text-[11.5px] font-bold ${ROLE_CLASS[role] ?? ROLE_CLASS.viewer}`}
    >
      {role}
    </span>
  )
}

export default function UsersPage() {
  const { mode } = useAuth()
  const usersQuery = useUsersList()
  const updateUser = useUpdateUser()
  const deleteUser = useDeleteUser()
  const resetPassword = useResetUserPassword()
  const revokeSessions = useRevokeUserSessions()

  const [createOpen, setCreateOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<User | null>(null)
  const [resetTarget, setResetTarget] = useState<User | null>(null)
  const [revokeTarget, setRevokeTarget] = useState<User | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<User | null>(null)
  const [revealed, setRevealed] = useState<RevealedPassword | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)

  const isForbidden =
    usersQuery.error instanceof ApiError && usersQuery.error.status === 403
  const items = usersQuery.data?.items ?? []
  // Signed in with an API key and no admin user exists: turning API-key
  // sign-in off would lock everyone out.
  const noAdminUser =
    mode === 'api_key' &&
    usersQuery.isSuccess &&
    !items.some((u) => u.role === 'admin' && !u.disabled)

  function fail(err: unknown, fallback: string) {
    setActionError(describeUserError(err, fallback))
  }

  function setDisabled(user: User, disabled: boolean) {
    setActionError(null)
    updateUser.mutate(
      { id: user.id, input: { disabled } },
      {
        onSuccess: () => toast.success(disabled ? 'User disabled' : 'User enabled'),
        onError: (err) =>
          fail(err, disabled ? 'Failed to disable user' : 'Failed to enable user'),
      },
    )
  }

  function handleReset() {
    if (!resetTarget) return
    const target = resetTarget
    setActionError(null)
    resetPassword.mutate(target.id, {
      onSuccess: (res) => {
        setResetTarget(null)
        setRevealed({ username: target.username, password: res.temporary_password })
      },
      onError: (err) => {
        setResetTarget(null)
        fail(err, 'Failed to reset password')
      },
    })
  }

  function handleRevoke() {
    if (!revokeTarget) return
    setActionError(null)
    revokeSessions.mutate(revokeTarget.id, {
      onSuccess: () => {
        toast.success('Sessions revoked')
        setRevokeTarget(null)
      },
      onError: (err) => {
        setRevokeTarget(null)
        fail(err, 'Failed to revoke sessions')
      },
    })
  }

  function handleDelete() {
    if (!deleteTarget) return
    setActionError(null)
    deleteUser.mutate(deleteTarget.id, {
      onSuccess: () => {
        toast.success('User deleted')
        setDeleteTarget(null)
      },
      onError: (err) => {
        setDeleteTarget(null)
        fail(err, 'Failed to delete user')
      },
    })
  }

  const columns: ColumnDef<User>[] = [
    {
      id: 'username',
      accessorKey: 'username',
      header: 'Username',
      size: 220,
      cell: ({ row }) => (
        <span className="font-medium text-foreground">{row.original.username}</span>
      ),
    },
    {
      id: 'display_name',
      accessorKey: 'display_name',
      header: 'Display name',
      cell: ({ row }) => (
        <span className="text-text-muted">{row.original.display_name || '—'}</span>
      ),
    },
    {
      id: 'role',
      accessorKey: 'role',
      header: 'Role',
      size: 110,
      cell: ({ row }) => <RoleBadge role={row.original.role} />,
    },
    {
      id: 'status',
      accessorFn: (u) => (u.disabled ? 'Disabled' : 'Active'),
      header: 'Status',
      size: 120,
      cell: ({ row }) =>
        row.original.disabled ? (
          <StatusPill tone="negative" label="Disabled" />
        ) : (
          <StatusPill tone="positive" label="Active" />
        ),
    },
    {
      id: 'last_login_at',
      accessorFn: (u) => u.last_login_at ?? '',
      header: 'Last login',
      meta: { tooltip: false },
      cell: ({ row }) => (
        <span className="text-text-muted">
          {row.original.last_login_at
            ? formatDateTime(row.original.last_login_at)
            : 'Never'}
        </span>
      ),
    },
  ]

  const rowActions: RowAction<User>[] = [
    {
      label: 'Edit',
      icon: <Pencil aria-hidden="true" />,
      onClick: setEditTarget,
    },
    {
      label: 'Disable',
      icon: <Ban aria-hidden="true" />,
      hidden: (u) => u.disabled,
      onClick: (u) => setDisabled(u, true),
    },
    {
      label: 'Enable',
      icon: <CircleCheck aria-hidden="true" />,
      hidden: (u) => !u.disabled,
      onClick: (u) => setDisabled(u, false),
    },
    {
      label: 'Reset password',
      icon: <RotateCcw aria-hidden="true" />,
      onClick: setResetTarget,
    },
    {
      label: 'Revoke sessions',
      icon: <LogOut aria-hidden="true" />,
      onClick: setRevokeTarget,
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
            Users
          </h1>
          <p className="text-[13px] text-text-subtle">
            People who sign in to this console with a username and password.
          </p>
        </div>
        {!isForbidden ? (
          <Button onClick={() => setCreateOpen(true)}>
            <Plus className="size-4" aria-hidden="true" />
            Create user
          </Button>
        ) : null}
      </div>

      {isForbidden ? (
        <div className="flex flex-col items-center gap-3 rounded-r-5 border border-border bg-card px-6 py-14 text-center">
          <span className="flex size-[52px] items-center justify-center rounded-r-5 bg-bg-muted text-text-muted">
            <KeyRound className="size-[22px]" aria-hidden="true" />
          </span>
          <div className="text-base font-semibold text-foreground">Admin required</div>
          <p className="max-w-[46ch] text-[13.5px] text-text-subtle">
            Managing users needs an admin. You&apos;re signed in with a role that
            can&apos;t.
          </p>
        </div>
      ) : (
        <Tabs defaultValue="users" className="min-h-0 flex-1">
          <TabsList variant="line">
            <TabsTrigger value="users">Users</TabsTrigger>
            <TabsTrigger value="audit">Audit log</TabsTrigger>
          </TabsList>

          {noAdminUser ? (
            <div
              role="alert"
              data-testid="no-admin-warning"
              className="rounded-r-3 border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-sm text-foreground"
            >
              No active admin user exists. If API-key sign-in is turned off, nobody can
              sign in to this console. Create an admin user now.
            </div>
          ) : null}

          {actionError ? (
            <div
              role="alert"
              className="flex items-start gap-2 rounded-r-3 border border-destructive/20 bg-destructive/10 px-3 py-2 text-sm text-destructive"
            >
              <span className="flex-1">{actionError}</span>
              <button
                type="button"
                onClick={() => setActionError(null)}
                aria-label="Dismiss"
                className="-my-0.5 -mr-1 shrink-0 rounded-r-2 p-0.5 text-destructive/70 outline-none transition-colors hover:text-destructive focus-visible:ring-2 focus-visible:ring-ring"
              >
                <X className="size-4" aria-hidden="true" />
              </button>
            </div>
          ) : null}

          <TabsContent value="users" className="flex min-h-0 flex-1 flex-col">
            <DataTable
              storageKey="users"
              columns={columns}
              data={items}
              getRowId={(u) => u.id}
              getRowLabel={(u) => u.username}
              getRowClassName={(u) => (u.disabled ? 'opacity-55' : undefined)}
              rowActions={rowActions}
              loading={usersQuery.isLoading}
              error={usersQuery.isError && "Couldn't load users."}
              emptyTitle="No users yet"
              emptyDescription="Create a user so people can sign in with a username and password."
            />
          </TabsContent>
          <TabsContent value="audit" className="flex min-h-0 flex-1 flex-col">
            <AuditPanel users={items} />
          </TabsContent>
        </Tabs>
      )}

      <CreateUserDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        onTemporaryPassword={setRevealed}
      />
      <EditUserDialog
        user={editTarget}
        onOpenChange={(open) => !open && setEditTarget(null)}
      />
      <RevealPasswordDialog revealed={revealed} onOpenChange={() => setRevealed(null)} />
      <ConfirmDialog
        open={resetTarget !== null}
        onOpenChange={(open) => !open && setResetTarget(null)}
        title={`Reset password for "${resetTarget?.username}"?`}
        description="A new temporary password is generated and every session of this user is signed out. They must choose a new password at next sign-in."
        confirmLabel="Reset password"
        isLoading={resetPassword.isPending}
        onConfirm={handleReset}
      />
      <ConfirmDialog
        open={revokeTarget !== null}
        onOpenChange={(open) => !open && setRevokeTarget(null)}
        title={`Revoke sessions for "${revokeTarget?.username}"?`}
        description="Every signed-in browser of this user is signed out immediately. Their password stays the same."
        confirmLabel="Revoke sessions"
        isLoading={revokeSessions.isPending}
        onConfirm={handleRevoke}
      />
      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        title={`Delete "${deleteTarget?.username}"?`}
        description="This user can no longer sign in. This can't be undone."
        confirmLabel="Delete"
        destructive
        isLoading={deleteUser.isPending}
        onConfirm={handleDelete}
      />
    </DataTablePage>
  )
}

/** Tenant-scoped auth audit log. Only mounted while its tab is open, so the
 * request is made on demand. */
function AuditPanel({ users }: { users: User[] }) {
  const auditQuery = useAuthAudit()
  const nameById = new Map(users.map((u) => [u.id, u.username]))

  const columns: ColumnDef<AuditEntry>[] = [
    {
      id: 'at',
      accessorKey: 'at',
      header: 'Time',
      size: 190,
      meta: { tooltip: false },
      cell: ({ row }) => (
        <span className="text-text-muted">{formatDateTime(row.original.at)}</span>
      ),
    },
    {
      id: 'actor',
      accessorFn: (e) => (e.actor_id ? `${e.actor_kind}:${e.actor_id}` : e.actor_kind),
      header: 'Actor',
      cell: ({ row }) => (
        <span className="font-mono text-xs text-text-muted">
          {row.original.actor_id
            ? `${row.original.actor_kind}:${nameById.get(row.original.actor_id) ?? row.original.actor_id}`
            : row.original.actor_kind}
        </span>
      ),
    },
    {
      id: 'action',
      accessorKey: 'action',
      header: 'Action',
      cell: ({ row }) => (
        <span className="font-medium text-foreground">{row.original.action}</span>
      ),
    },
    {
      id: 'target',
      accessorFn: (e) =>
        e.target_user_id ? (nameById.get(e.target_user_id) ?? e.target_user_id) : '',
      header: 'Target',
      cell: ({ row }) => {
        const id = row.original.target_user_id
        return (
          <span className="text-text-muted">{id ? (nameById.get(id) ?? id) : '—'}</span>
        )
      },
    },
    {
      id: 'ip',
      accessorFn: (e) => e.ip ?? '',
      header: 'IP',
      size: 150,
      cell: ({ row }) => (
        <span className="font-mono text-xs text-text-muted">
          {row.original.ip || '—'}
        </span>
      ),
    },
  ]

  return (
    <DataTable
      storageKey="auth-audit"
      columns={columns}
      data={auditQuery.data?.items ?? []}
      getRowId={(e) => String(e.id)}
      getRowLabel={(e) => `${e.action} at ${e.at}`}
      loading={auditQuery.isLoading}
      error={auditQuery.isError && "Couldn't load the audit log."}
      emptyTitle="No audit events yet"
      emptyDescription="Sign-ins, password changes and user changes show up here."
    />
  )
}
