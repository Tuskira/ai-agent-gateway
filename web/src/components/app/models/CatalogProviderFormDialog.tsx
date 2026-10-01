import { useState } from 'react'
import { zodResolver } from '@hookform/resolvers/zod'
import { useForm } from 'react-hook-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { useCreateCatalogProvider, useUpdateCatalogProvider } from '@/lib/queries'
import type { CatalogProvider } from '@/lib/model-catalog'
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
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'

/** `^[a-z0-9][a-z0-9-]{0,31}$` — the catalog's own slug rule. */
const SLUG_PATTERN = /^[a-z0-9][a-z0-9-]{0,31}$/

const schema = z.object({
  slug: z
    .string()
    .trim()
    .min(1, 'Slug is required')
    .regex(SLUG_PATTERN, 'Lowercase letters, numbers and dash only (max 32 chars)'),
  display_name: z.string().trim().min(1, 'Display name is required'),
  base_url: z.string().trim().min(1, 'Base URL is required'),
  docs_url: z.string().trim().optional(),
})

type FormValues = z.infer<typeof schema>

interface CatalogProviderFormDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  provider?: CatalogProvider | null
}

/** Outer shell: only mounts the body while open, keyed by the target
 * provider's id, so the body can initialize its state straight from props
 * — same pattern as `ModelFormDialog`. */
export function CatalogProviderFormDialog({
  open,
  onOpenChange,
  provider = null,
}: CatalogProviderFormDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        {open ? (
          <CatalogProviderFormBody
            key={provider?.id ?? 'create'}
            provider={provider}
            onDone={() => onOpenChange(false)}
            onCancel={() => onOpenChange(false)}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  )
}

interface CatalogProviderFormBodyProps {
  provider: CatalogProvider | null
  onDone: () => void
  onCancel: () => void
}

function CatalogProviderFormBody({ provider, onDone, onCancel }: CatalogProviderFormBodyProps) {
  const isEdit = provider !== null
  const createProvider = useCreateCatalogProvider()
  const updateProvider = useUpdateCatalogProvider(provider?.id ?? '')
  const mutation = isEdit ? updateProvider : createProvider

  const [enabled, setEnabled] = useState(provider?.enabled ?? true)
  const [warnings, setWarnings] = useState<string[]>([])
  const [saved, setSaved] = useState(false)

  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: {
      slug: provider?.slug ?? '',
      display_name: provider?.display_name ?? '',
      base_url: provider?.base_url ?? '',
      docs_url: provider?.docs_url ?? '',
    },
  })

  function onSubmit(values: FormValues) {
    mutation.mutate(
      {
        slug: values.slug,
        display_name: values.display_name,
        base_url: values.base_url,
        docs_url: values.docs_url || undefined,
        enabled,
      },
      {
        onSuccess: (result) => {
          toast.success(isEdit ? 'Provider updated' : 'Provider created')
          if (result.warnings.length > 0) {
            setWarnings(result.warnings)
            setSaved(true)
            return
          }
          onDone()
        },
        onError: (err) => {
          toast.error(err instanceof Error ? err.message : 'Failed to save provider')
        },
      },
    )
  }

  return (
    <Form {...form}>
      <form onSubmit={form.handleSubmit(onSubmit)}>
        <DialogHeader>
          <DialogTitle>{isEdit ? `Edit ${provider?.display_name}` : 'Add provider'}</DialogTitle>
          <DialogDescription>
            A platform-level provider tenants can connect from the Models page.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-4 py-4">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <FormField
              control={form.control}
              name="slug"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Slug *</FormLabel>
                  <FormControl>
                    <Input placeholder="e.g. nebius" className="font-mono" {...field} />
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
                  <FormLabel>Display name *</FormLabel>
                  <FormControl>
                    <Input placeholder="e.g. Nebius AI Studio" {...field} />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
          </div>

          <FormField
            control={form.control}
            name="base_url"
            render={({ field }) => (
              <FormItem>
                <FormLabel>Base URL *</FormLabel>
                <FormControl>
                  <Input
                    placeholder="https://api.example.com/v1"
                    className="font-mono"
                    {...field}
                  />
                </FormControl>
                <p className="text-xs text-text-subtle">
                  OpenAI-compatible, including the version segment, no trailing slash.
                </p>
                {warnings.length > 0 ? (
                  <ul className="flex flex-col gap-0.5">
                    {warnings.map((w) => (
                      <li key={w} className="text-xs font-medium text-sev-medium-fg">
                        {w}
                      </li>
                    ))}
                  </ul>
                ) : null}
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name="docs_url"
            render={({ field }) => (
              <FormItem>
                <FormLabel>Docs URL</FormLabel>
                <FormControl>
                  <Input placeholder="https://docs.example.com" {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />

          <div className="flex flex-col gap-1.5">
            <Label>Enabled</Label>
            <div className="flex h-9 items-center">
              <Switch checked={enabled} onCheckedChange={setEnabled} />
            </div>
          </div>
        </div>

        <DialogFooter>
          <Button type="button" variant="outline" onClick={onCancel}>
            {saved ? 'Done' : 'Cancel'}
          </Button>
          {!saved ? (
            <Button type="submit" disabled={mutation.isPending}>
              {mutation.isPending ? 'Saving…' : isEdit ? 'Save changes' : 'Create'}
            </Button>
          ) : null}
        </DialogFooter>
      </form>
    </Form>
  )
}
