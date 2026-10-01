import { zodResolver } from '@hookform/resolvers/zod'
import { useForm, useFieldArray } from 'react-hook-form'
import { z } from 'zod'
import { Plus, X } from 'lucide-react'
import { toast } from 'sonner'
import { useCreateCredential } from '@/lib/queries'
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

const fieldRowSchema = z.object({
  name: z.string().trim().min(1, 'Required'),
  value: z.string().min(1, 'Required'),
})

const schema = z.object({
  name: z.string().trim().min(1, 'Name is required'),
  type: z.string().trim().min(1, 'Type is required'),
  fields: z.array(fieldRowSchema).min(1, 'Add at least one field'),
})

type FormValues = z.infer<typeof schema>

interface CredentialFormDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function CredentialFormDialog({ open, onOpenChange }: CredentialFormDialogProps) {
  const createCredential = useCreateCredential()
  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: { name: '', type: '', fields: [{ name: '', value: '' }] },
  })
  const fieldArray = useFieldArray({ control: form.control, name: 'fields' })

  function handleOpenChange(next: boolean) {
    if (!next) form.reset({ name: '', type: '', fields: [{ name: '', value: '' }] })
    onOpenChange(next)
  }

  function onSubmit(values: FormValues) {
    const payload: Record<string, string> = {}
    for (const f of values.fields) payload[f.name] = f.value
    createCredential.mutate(
      { name: values.name, type: values.type, payload },
      {
        onSuccess: () => {
          toast.success('Credential created')
          handleOpenChange(false)
        },
        onError: (err) => {
          toast.error(err instanceof Error ? err.message : 'Failed to create credential')
        },
      },
    )
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <Form {...form}>
          <form onSubmit={form.handleSubmit(onSubmit)}>
            <DialogHeader>
              <DialogTitle>Add credential</DialogTitle>
              <DialogDescription>
                Values are encrypted at rest and never shown after creation.
              </DialogDescription>
            </DialogHeader>
            <div className="flex flex-col gap-4 py-4">
              <div className="grid grid-cols-2 gap-3">
                <FormField
                  control={form.control}
                  name="name"
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>Name</FormLabel>
                      <FormControl>
                        <Input placeholder="e.g. opencti" {...field} />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name="type"
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>Type</FormLabel>
                      <FormControl>
                        <Input placeholder="e.g. api_key" {...field} />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              </div>

              <div className="flex flex-col gap-2">
                <div className="flex items-center justify-between">
                  <span className="text-xs font-medium text-text-muted">Fields</span>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={() => fieldArray.append({ name: '', value: '' })}
                  >
                    <Plus className="size-3.5" aria-hidden="true" />
                    Add field
                  </Button>
                </div>
                {fieldArray.fields.map((row, index) => (
                  <div key={row.id} className="flex items-start gap-2">
                    <FormField
                      control={form.control}
                      name={`fields.${index}.name`}
                      render={({ field }) => (
                        <FormItem className="flex-1">
                          <FormControl>
                            <Input
                              placeholder="field name"
                              className="font-mono"
                              {...field}
                            />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`fields.${index}.value`}
                      render={({ field }) => (
                        <FormItem className="flex-1">
                          <FormControl>
                            <Input
                              type="password"
                              placeholder="value"
                              className="font-mono"
                              {...field}
                            />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon-sm"
                      aria-label="Remove field"
                      disabled={fieldArray.fields.length <= 1}
                      onClick={() => fieldArray.remove(index)}
                    >
                      <X className="size-3.5" aria-hidden="true" />
                    </Button>
                  </div>
                ))}
              </div>
            </div>
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={() => handleOpenChange(false)}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={createCredential.isPending}>
                {createCredential.isPending ? 'Creating…' : 'Add credential'}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  )
}
