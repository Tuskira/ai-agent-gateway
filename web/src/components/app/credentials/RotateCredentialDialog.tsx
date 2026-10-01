import { useEffect } from 'react'
import { zodResolver } from '@hookform/resolvers/zod'
import { useForm, useFieldArray } from 'react-hook-form'
import { z } from 'zod'
import { Plus, X } from 'lucide-react'
import { toast } from 'sonner'
import { useUpdateCredential } from '@/lib/queries'
import type { Credential } from '@/lib/credentials'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Form, FormControl, FormField, FormItem, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'

const fieldRowSchema = z.object({
  name: z.string().trim().min(1, 'Required'),
  value: z.string().min(1, 'Required'),
})

const schema = z.object({
  fields: z.array(fieldRowSchema).min(1, 'Add at least one field'),
})

type FormValues = z.infer<typeof schema>

interface RotateCredentialDialogProps {
  credential: Credential | null
  onOpenChange: (open: boolean) => void
}

/** Prefills one row per existing field name (value left blank — the API
 * never returns it), so rotating means replacing every value. */
export function RotateCredentialDialog({
  credential,
  onOpenChange,
}: RotateCredentialDialogProps) {
  const updateCredential = useUpdateCredential(credential?.name ?? '')
  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: { fields: [{ name: '', value: '' }] },
  })
  const fieldArray = useFieldArray({ control: form.control, name: 'fields' })

  useEffect(() => {
    if (credential) {
      form.reset({
        fields:
          credential.field_names.length > 0
            ? credential.field_names.map((name) => ({ name, value: '' }))
            : [{ name: '', value: '' }],
      })
    }
  }, [credential, form])

  function handleOpenChange(next: boolean) {
    onOpenChange(next)
  }

  function onSubmit(values: FormValues) {
    const payload: Record<string, string> = {}
    for (const f of values.fields) payload[f.name] = f.value
    updateCredential.mutate(
      { payload },
      {
        onSuccess: () => {
          toast.success('Credential rotated')
          handleOpenChange(false)
        },
        onError: (err) => {
          toast.error(err instanceof Error ? err.message : 'Failed to rotate credential')
        },
      },
    )
  }

  return (
    <Dialog open={credential !== null} onOpenChange={handleOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <Form {...form}>
          <form onSubmit={form.handleSubmit(onSubmit)}>
            <DialogHeader>
              <DialogTitle>Rotate &quot;{credential?.name}&quot;</DialogTitle>
              <DialogDescription>
                Enter new values for every field — this replaces the stored payload.
              </DialogDescription>
            </DialogHeader>
            <div className="flex flex-col gap-2 py-4">
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
                            placeholder="new value"
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
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="self-start"
                onClick={() => fieldArray.append({ name: '', value: '' })}
              >
                <Plus className="size-3.5" aria-hidden="true" />
                Add field
              </Button>
            </div>
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={() => handleOpenChange(false)}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={updateCredential.isPending}>
                {updateCredential.isPending ? 'Rotating…' : 'Rotate'}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  )
}
