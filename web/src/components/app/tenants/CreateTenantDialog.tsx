import { useState } from 'react'
import { zodResolver } from '@hookform/resolvers/zod'
import { useForm } from 'react-hook-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { ApiError } from '@/lib/api'
import { useCreateTenant } from '@/lib/queries'
import type { Tenant } from '@/lib/tenants'
import { slugify } from '@/lib/utils'
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

/** Mirrors the server: name 1–200 characters, slug 1–100. The slug is what
 * people type at sign-in, so keep it to lowercase letters, digits and
 * dashes even though the API accepts more. */
const schema = z.object({
  name: z
    .string()
    .trim()
    .min(1, 'Name is required')
    .max(200, 'Use at most 200 characters'),
  slug: z
    .string()
    .trim()
    .min(1, 'Slug is required')
    .max(100, 'Use at most 100 characters')
    .regex(/^[a-z0-9]+(?:-[a-z0-9]+)*$/, 'Lowercase letters, digits and dashes only'),
})

type FormValues = z.infer<typeof schema>

interface CreateTenantDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** Called with the new tenant so the page can offer to act in it. */
  onCreated?: (tenant: Tenant) => void
}

function describeTenantError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    if (err.status === 409) return 'A tenant with this slug already exists.'
    if (err.status === 403) return 'Creating tenants needs a platform admin.'
    return err.message || fallback
  }
  return fallback
}

export function CreateTenantDialog({
  open,
  onOpenChange,
  onCreated,
}: CreateTenantDialogProps) {
  const createTenant = useCreateTenant()
  const [error, setError] = useState<string | null>(null)
  // Auto-fill the slug from the name until the person edits the slug.
  const [slugTouched, setSlugTouched] = useState(false)
  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: { name: '', slug: '' },
  })

  function handleOpenChange(next: boolean) {
    if (!next) {
      form.reset()
      setError(null)
      setSlugTouched(false)
    }
    onOpenChange(next)
  }

  function onSubmit(values: FormValues) {
    setError(null)
    createTenant.mutate(
      { slug: values.slug.trim(), name: values.name.trim() },
      {
        onSuccess: (created) => {
          toast.success(`Tenant ${created.name} created`)
          handleOpenChange(false)
          onCreated?.(created)
        },
        onError: (err) => setError(describeTenantError(err, 'Failed to create tenant')),
      },
    )
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="sm:max-w-md">
        <Form {...form}>
          <form onSubmit={form.handleSubmit(onSubmit)} noValidate>
            <DialogHeader>
              <DialogTitle>Create tenant</DialogTitle>
              <DialogDescription>
                A tenant is an isolated space with its own users, keys, MCPs and models.
                The slug is what its users type at sign-in.
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
                name="name"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Name</FormLabel>
                    <FormControl>
                      <Input
                        autoComplete="off"
                        placeholder="e.g. Acme Corp"
                        {...field}
                        onChange={(event) => {
                          field.onChange(event)
                          if (!slugTouched) {
                            form.setValue('slug', slugify(event.target.value), {
                              shouldValidate: form.formState.isSubmitted,
                            })
                          }
                        }}
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="slug"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Slug</FormLabel>
                    <FormControl>
                      <Input
                        autoComplete="off"
                        autoCapitalize="off"
                        spellCheck={false}
                        placeholder="acme-corp"
                        className="font-mono"
                        {...field}
                        onChange={(event) => {
                          setSlugTouched(true)
                          field.onChange(event)
                        }}
                      />
                    </FormControl>
                    <FormDescription>
                      Lowercase letters, digits and dashes. Can&apos;t be changed later.
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
              <Button type="submit" disabled={createTenant.isPending}>
                {createTenant.isPending ? 'Creating…' : 'Create tenant'}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  )
}
