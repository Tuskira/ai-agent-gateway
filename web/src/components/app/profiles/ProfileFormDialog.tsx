import { useEffect } from 'react'
import { zodResolver } from '@hookform/resolvers/zod'
import { useForm } from 'react-hook-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { useCreateProfile, useUpdateProfile } from '@/lib/queries'
import type { Profile } from '@/lib/profiles'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'

const schema = z.object({
  name: z.string().trim().min(1, 'Name is required'),
  description: z.string().optional(),
})

type FormValues = z.infer<typeof schema>

interface ProfileFormDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  profile?: Profile | null
}

export function ProfileFormDialog({
  open,
  onOpenChange,
  profile = null,
}: ProfileFormDialogProps) {
  const isEdit = profile !== null
  const createProfile = useCreateProfile()
  const updateProfile = useUpdateProfile(profile?.id ?? '')
  const mutation = isEdit ? updateProfile : createProfile

  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: { name: '', description: '' },
  })

  useEffect(() => {
    if (!open) return
    form.reset({ name: profile?.name ?? '', description: profile?.description ?? '' })
  }, [open, profile, form])

  function handleOpenChange(next: boolean) {
    onOpenChange(next)
  }

  function onSubmit(values: FormValues) {
    mutation.mutate(
      { name: values.name, description: values.description || undefined },
      {
        onSuccess: () => {
          toast.success(isEdit ? 'Profile updated' : 'Profile created')
          handleOpenChange(false)
        },
        onError: (err) => {
          toast.error(err instanceof Error ? err.message : 'Failed to save profile')
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
              <DialogTitle>
                {isEdit ? `Edit ${profile?.name}` : 'Create profile'}
              </DialogTitle>
            </DialogHeader>
            <div className="flex flex-col gap-4 py-4">
              <FormField
                control={form.control}
                name="name"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Name</FormLabel>
                    <FormControl>
                      <Input placeholder="e.g. SOC Agent Profile" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="description"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Description</FormLabel>
                    <FormControl>
                      <Textarea
                        rows={3}
                        placeholder="What is this profile for?"
                        {...field}
                      />
                    </FormControl>
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
              <Button type="submit" disabled={mutation.isPending}>
                {mutation.isPending
                  ? 'Saving…'
                  : isEdit
                    ? 'Save changes'
                    : 'Create profile'}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  )
}
