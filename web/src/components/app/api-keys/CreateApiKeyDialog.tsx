import { zodResolver } from '@hookform/resolvers/zod'
import { useForm } from 'react-hook-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { useCreateApiKey, useProfiles } from '@/lib/queries'
import { endOfLocalDayISO } from '@/lib/dates'
import type { ApiKeyCreated, ApiKeyRole } from '@/lib/api-keys'
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

const NO_PROFILE = '__none__'

const schema = z.object({
  name: z.string().trim().min(1, 'Name is required').max(200),
  role: z.enum(['admin', 'agent', 'interceptor']),
  profileId: z.string().optional(),
  expiresAt: z
    .string()
    .optional()
    .refine(
      (value) => {
        if (!value) return true
        const year = Number(value.slice(0, 4))
        const month = Number(value.slice(5, 7))
        const day = Number(value.slice(8, 10))
        const picked = new Date(year, month - 1, day)
        const today = new Date()
        today.setHours(0, 0, 0, 0)
        return picked.getTime() >= today.getTime()
      },
      { message: 'Expiry date must be today or later' },
    ),
})

type FormValues = z.infer<typeof schema>

interface CreateApiKeyDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** Called right after a successful create, with the plaintext key — the
   * caller opens the "shown once" reveal dialog with it. */
  onCreated: (created: ApiKeyCreated) => void
}

export function CreateApiKeyDialog({
  open,
  onOpenChange,
  onCreated,
}: CreateApiKeyDialogProps) {
  const createApiKey = useCreateApiKey()
  const profiles = useProfiles()
  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: '',
      role: 'agent' as ApiKeyRole,
      profileId: '',
      expiresAt: '',
    },
  })

  function handleOpenChange(next: boolean) {
    if (!next) form.reset()
    onOpenChange(next)
  }

  function onSubmit(values: FormValues) {
    createApiKey.mutate(
      {
        name: values.name,
        role: values.role as ApiKeyRole,
        expires_at: values.expiresAt ? endOfLocalDayISO(values.expiresAt) : undefined,
        profile_id: values.profileId || undefined,
      },
      {
        onSuccess: (created) => {
          toast.success('API key created')
          handleOpenChange(false)
          onCreated(created)
        },
        onError: (err) => {
          toast.error(err instanceof Error ? err.message : 'Failed to create key')
        },
      },
    )
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="sm:max-w-md">
        <Form {...form}>
          <form onSubmit={form.handleSubmit(onSubmit)}>
            <DialogHeader>
              <DialogTitle>Create key</DialogTitle>
              <DialogDescription>
                Keys are shown once at creation and stored hashed. Rotate or revoke
                anytime.
              </DialogDescription>
            </DialogHeader>
            <div className="flex flex-col gap-4 py-4">
              <FormField
                control={form.control}
                name="name"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Name</FormLabel>
                    <FormControl>
                      <Input placeholder="e.g. CI pipeline" {...field} />
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
                        <SelectItem value="agent">agent</SelectItem>
                        <SelectItem value="interceptor">interceptor</SelectItem>
                      </SelectContent>
                    </Select>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="profileId"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Bind to profile (optional)</FormLabel>
                    <Select
                      value={field.value || NO_PROFILE}
                      onValueChange={(v) => field.onChange(v === NO_PROFILE ? '' : v)}
                    >
                      <FormControl>
                        <SelectTrigger className="w-full">
                          <SelectValue placeholder="Not bound" />
                        </SelectTrigger>
                      </FormControl>
                      <SelectContent>
                        <SelectItem value={NO_PROFILE}>Not bound</SelectItem>
                        {(profiles.data?.items ?? []).map((p) => (
                          <SelectItem key={p.id} value={p.id}>
                            {p.name}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <FormDescription>
                      A bound key can only use this profile&apos;s tools; the
                      X-Agent-Profile-Name header cannot widen it.
                    </FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="expiresAt"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Expires (optional)</FormLabel>
                    <FormControl>
                      <Input type="date" {...field} />
                    </FormControl>
                    <FormDescription>
                      Key expires at the end of this day (local time)
                    </FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </div>
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={() => handleOpenChange(false)}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={createApiKey.isPending}>
                {createApiKey.isPending ? 'Creating…' : 'Create key'}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  )
}
