import { useState } from 'react'
import { zodResolver } from '@hookform/resolvers/zod'
import { useForm, useWatch } from 'react-hook-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { PASSWORD_MAX_LENGTH, PASSWORD_MIN_LENGTH } from '@/lib/password-policy'
import { useCreateUser } from '@/lib/queries'
import { describeUserError } from '@/lib/users'
import type { RevealedPassword } from '@/components/app/users/RevealPasswordDialog'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'

const USERNAME_RE = /^[a-z0-9][a-z0-9._@+-]{2,63}$/

const schema = z
  .object({
    username: z
      .string()
      .trim()
      .toLowerCase()
      .regex(
        USERNAME_RE,
        '3–64 characters: lowercase letters, digits and . _ @ + - (email addresses work)',
      ),
    display_name: z.string().trim().max(200),
    role: z.enum(['admin', 'viewer']),
    generate: z.boolean(),
    password: z.string(),
  })
  .superRefine((values, ctx) => {
    if (values.generate) return
    if (values.password.length < PASSWORD_MIN_LENGTH) {
      ctx.addIssue({
        code: 'custom',
        path: ['password'],
        message: `Use at least ${PASSWORD_MIN_LENGTH} characters`,
      })
    } else if (values.password.length > PASSWORD_MAX_LENGTH) {
      ctx.addIssue({
        code: 'custom',
        path: ['password'],
        message: `Use at most ${PASSWORD_MAX_LENGTH} characters`,
      })
    } else if (values.password.toLowerCase() === values.username) {
      ctx.addIssue({
        code: 'custom',
        path: ['password'],
        message: 'Password can’t be the same as the username',
      })
    }
  })

type FormValues = z.input<typeof schema>

interface CreateUserDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** Called with the generated temporary password so the caller can show it
   * once. Not called when the admin chose the password. */
  onTemporaryPassword: (revealed: RevealedPassword) => void
}

export function CreateUserDialog({
  open,
  onOpenChange,
  onTemporaryPassword,
}: CreateUserDialogProps) {
  const createUser = useCreateUser()
  const [error, setError] = useState<string | null>(null)
  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: {
      username: '',
      display_name: '',
      role: 'viewer',
      generate: true,
      password: '',
    },
  })
  const generate = useWatch({ control: form.control, name: 'generate' })

  function handleOpenChange(next: boolean) {
    if (!next) {
      form.reset()
      setError(null)
    }
    onOpenChange(next)
  }

  function onSubmit(values: FormValues) {
    setError(null)
    const username = values.username.trim().toLowerCase()
    createUser.mutate(
      {
        username,
        ...(values.display_name.trim()
          ? { display_name: values.display_name.trim() }
          : {}),
        role: values.role,
        ...(values.generate ? {} : { password: values.password }),
      },
      {
        onSuccess: (created) => {
          toast.success(`User ${created.user.username} created`)
          handleOpenChange(false)
          if (created.temporary_password) {
            onTemporaryPassword({
              username: created.user.username,
              password: created.temporary_password,
            })
          }
        },
        onError: (err) => setError(describeUserError(err, 'Failed to create user')),
      },
    )
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="sm:max-w-md">
        <Form {...form}>
          <form onSubmit={form.handleSubmit(onSubmit)} noValidate>
            <DialogHeader>
              <DialogTitle>Create user</DialogTitle>
              <DialogDescription>
                The user signs in with this username and must change the password at first
                sign-in.
              </DialogDescription>
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
              <FormField
                control={form.control}
                name="username"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Username</FormLabel>
                    <FormControl>
                      <Input
                        autoComplete="off"
                        autoCapitalize="off"
                        spellCheck={false}
                        placeholder="e.g. jane.doe"
                        {...field}
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="display_name"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Display name (optional)</FormLabel>
                    <FormControl>
                      <Input autoComplete="off" placeholder="Jane Doe" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="role"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Role</FormLabel>
                    <Select value={field.value} onValueChange={field.onChange}>
                      <FormControl>
                        <SelectTrigger className="w-full">
                          <SelectValue placeholder="Select a role" />
                        </SelectTrigger>
                      </FormControl>
                      <SelectContent>
                        <SelectItem value="admin">admin</SelectItem>
                        <SelectItem value="viewer">viewer</SelectItem>
                      </SelectContent>
                    </Select>
                    <FormDescription>
                      Admins manage the gateway. Viewers can read but not change anything.
                    </FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="generate"
                render={({ field }) => (
                  <FormItem className="flex flex-row items-center justify-between gap-3">
                    <FormLabel>Generate a temporary password</FormLabel>
                    <FormControl>
                      <Switch checked={field.value} onCheckedChange={field.onChange} />
                    </FormControl>
                  </FormItem>
                )}
              />
              {generate ? null : (
                <FormField
                  control={form.control}
                  name="password"
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>Password</FormLabel>
                      <FormControl>
                        <Input type="password" autoComplete="new-password" {...field} />
                      </FormControl>
                      <FormDescription>
                        {PASSWORD_MIN_LENGTH}–{PASSWORD_MAX_LENGTH} characters. The user
                        still has to change it at first sign-in.
                      </FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              )}
            </div>
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={() => handleOpenChange(false)}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={createUser.isPending}>
                {createUser.isPending ? 'Creating…' : 'Create user'}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  )
}
