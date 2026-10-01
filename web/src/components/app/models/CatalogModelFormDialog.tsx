import { useState } from 'react'
import { zodResolver } from '@hookform/resolvers/zod'
import { useForm } from 'react-hook-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { useCreateCatalogModel, useUpdateCatalogModel } from '@/lib/queries'
import type { CatalogCapabilities, CatalogModel } from '@/lib/model-catalog'
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { Textarea } from '@/components/ui/textarea'

function optionalNonNegativeNumber(v: string | undefined) {
  return !v || (/^\d+(\.\d+)?$/.test(v) && Number(v) >= 0)
}

function optionalNonNegativeInteger(v: string | undefined) {
  return !v || /^\d+$/.test(v)
}

const priceFieldSchema = z
  .string()
  .optional()
  .refine(optionalNonNegativeNumber, 'Must be a non-negative number')

const schema = z
  .object({
    model_id: z.string().trim().min(1, 'Model id is required'),
    display_name: z.string().trim().optional(),
    suggested_name: z
      .string()
      .trim()
      .regex(/^[a-z0-9._-]{0,64}$/, 'Lowercase letters, numbers, dot, underscore, dash only')
      .optional(),
    priceInput: priceFieldSchema,
    priceOutput: priceFieldSchema,
    maxContext: z
      .string()
      .optional()
      .refine(optionalNonNegativeInteger, 'Must be a whole number, 0 or more'),
    notes: z.string().trim().max(512, 'Must be at most 512 characters').optional(),
  })
  .superRefine((values, ctx) => {
    const anyPrice = values.priceInput || values.priceOutput
    if (!anyPrice) return
    for (const field of ['priceInput', 'priceOutput'] as const) {
      if (!values[field]) {
        ctx.addIssue({ code: 'custom', path: [field], message: 'Required when a price is set' })
      }
    }
  })

type FormValues = z.infer<typeof schema>

/** `true`/`false`/`null` ("unknown") tri-state, as a string so it can live
 * in a native form field. */
type TriState = 'unknown' | 'yes' | 'no'

const TRI_STATE_ITEMS: { value: TriState; label: string }[] = [
  { value: 'unknown', label: 'Unknown' },
  { value: 'yes', label: 'Yes' },
  { value: 'no', label: 'No' },
]

function triStateFromBool(v: boolean | null): TriState {
  if (v === true) return 'yes'
  if (v === false) return 'no'
  return 'unknown'
}

function boolFromTriState(v: TriState): boolean | null {
  if (v === 'yes') return true
  if (v === 'no') return false
  return null
}

interface CatalogModelFormDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** Provider slug — used only to caption the dialog; the id comes from
   * `providerId`. */
  providerSlug: string
  providerId: string
  model?: CatalogModel | null
}

/** Outer shell — same pattern as `ModelFormDialog`. */
export function CatalogModelFormDialog({
  open,
  onOpenChange,
  providerSlug,
  providerId,
  model = null,
}: CatalogModelFormDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        {open ? (
          <CatalogModelFormBody
            key={model?.id ?? 'create'}
            providerSlug={providerSlug}
            providerId={providerId}
            model={model}
            onDone={() => onOpenChange(false)}
            onCancel={() => onOpenChange(false)}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  )
}

interface CatalogModelFormBodyProps {
  providerSlug: string
  providerId: string
  model: CatalogModel | null
  onDone: () => void
  onCancel: () => void
}

function CatalogModelFormBody({
  providerSlug,
  providerId,
  model,
  onDone,
  onCancel,
}: CatalogModelFormBodyProps) {
  const isEdit = model !== null
  const createCatalogModel = useCreateCatalogModel(providerId)
  const updateCatalogModel = useUpdateCatalogModel(model?.id ?? '')
  const mutation = isEdit ? updateCatalogModel : createCatalogModel

  const [enabled, setEnabled] = useState(model?.enabled ?? true)
  const [tools, setTools] = useState<TriState>(triStateFromBool(model?.capabilities.tools ?? null))
  const [vision, setVision] = useState<TriState>(
    triStateFromBool(model?.capabilities.vision ?? null),
  )
  const [streaming, setStreaming] = useState<TriState>(
    triStateFromBool(model?.capabilities.streaming ?? null),
  )

  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: {
      model_id: model?.model_id ?? '',
      display_name: model?.display_name ?? '',
      suggested_name: model?.suggested_name ?? '',
      priceInput: model?.price ? String(model.price.input) : '',
      priceOutput: model?.price ? String(model.price.output) : '',
      maxContext:
        model?.capabilities.max_context !== null && model?.capabilities.max_context !== undefined
          ? String(model.capabilities.max_context)
          : '',
      notes: model?.notes ?? '',
    },
  })

  function onSubmit(values: FormValues) {
    const capabilities: CatalogCapabilities = {
      tools: boolFromTriState(tools),
      vision: boolFromTriState(vision),
      streaming: boolFromTriState(streaming),
      max_context: values.maxContext ? Number(values.maxContext) : null,
    }
    const price =
      values.priceInput && values.priceOutput
        ? { input: Number(values.priceInput), output: Number(values.priceOutput) }
        : undefined

    mutation.mutate(
      {
        model_id: values.model_id.trim(),
        display_name: values.display_name?.trim() || undefined,
        suggested_name: values.suggested_name?.trim() || undefined,
        price,
        capabilities,
        notes: values.notes?.trim() ?? '',
        enabled,
      },
      {
        onSuccess: () => {
          toast.success(isEdit ? 'Model updated' : 'Model added')
          onDone()
        },
        onError: (err) => {
          toast.error(err instanceof Error ? err.message : 'Failed to save model')
        },
      },
    )
  }

  return (
    <Form {...form}>
      <form onSubmit={form.handleSubmit(onSubmit)}>
        <DialogHeader>
          <DialogTitle>{isEdit ? `Edit ${model?.suggested_name}` : 'Add model'}</DialogTitle>
          <DialogDescription>
            A catalog template under <span className="font-mono">{providerSlug}</span>. Editing
            it doesn&apos;t change any tenant model already connected from it.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-4 py-4">
          <FormField
            control={form.control}
            name="model_id"
            render={({ field }) => (
              <FormItem>
                <FormLabel>Model id *</FormLabel>
                <FormControl>
                  <Input placeholder="e.g. zai-org/GLM-5.3" className="font-mono" {...field} />
                </FormControl>
                <p className="text-xs text-text-subtle">The vendor's own id for this model.</p>
                <FormMessage />
              </FormItem>
            )}
          />
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <FormField
              control={form.control}
              name="display_name"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Display name</FormLabel>
                  <FormControl>
                    <Input placeholder="e.g. GLM 5.3" {...field} />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name="suggested_name"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Suggested name</FormLabel>
                  <FormControl>
                    <Input
                      placeholder={`e.g. glm-5.3-${providerSlug}`}
                      className="font-mono"
                      {...field}
                    />
                  </FormControl>
                  <p className="text-xs text-text-subtle">
                    Registry name a tenant gets by default when connecting this model.
                  </p>
                  <FormMessage />
                </FormItem>
              )}
            />
          </div>

          <div className="flex flex-col gap-3">
            <div>
              <div className="text-[13px] font-semibold text-foreground">Price</div>
              <p className="text-xs text-text-subtle">
                USD per 1M tokens. Leave blank when unknown — never a guessed number.
              </p>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <FormField
                control={form.control}
                name="priceInput"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel className="text-xs">Input</FormLabel>
                    <FormControl>
                      <Input inputMode="decimal" placeholder="3.00" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="priceOutput"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel className="text-xs">Output</FormLabel>
                    <FormControl>
                      <Input inputMode="decimal" placeholder="15.00" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </div>
          </div>

          <div className="flex flex-col gap-3">
            <div>
              <div className="text-[13px] font-semibold text-foreground">Capabilities</div>
              <p className="text-xs text-text-subtle">Leave "Unknown" rather than guessing.</p>
            </div>
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
              <div className="flex flex-col gap-1.5">
                <Label className="text-xs text-text-muted">Tool calling</Label>
                <Select items={TRI_STATE_ITEMS} value={tools} onValueChange={(v) => setTools(v as TriState)}>
                  <SelectTrigger className="w-full" aria-label="Tool calling">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {TRI_STATE_ITEMS.map((item) => (
                      <SelectItem key={item.value} value={item.value}>
                        {item.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="flex flex-col gap-1.5">
                <Label className="text-xs text-text-muted">Vision</Label>
                <Select
                  items={TRI_STATE_ITEMS}
                  value={vision}
                  onValueChange={(v) => setVision(v as TriState)}
                >
                  <SelectTrigger className="w-full" aria-label="Vision">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {TRI_STATE_ITEMS.map((item) => (
                      <SelectItem key={item.value} value={item.value}>
                        {item.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="flex flex-col gap-1.5">
                <Label className="text-xs text-text-muted">Streaming</Label>
                <Select
                  items={TRI_STATE_ITEMS}
                  value={streaming}
                  onValueChange={(v) => setStreaming(v as TriState)}
                >
                  <SelectTrigger className="w-full" aria-label="Streaming">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {TRI_STATE_ITEMS.map((item) => (
                      <SelectItem key={item.value} value={item.value}>
                        {item.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            </div>
            <FormField
              control={form.control}
              name="maxContext"
              render={({ field }) => (
                <FormItem>
                  <FormLabel className="text-xs">Max context (tokens)</FormLabel>
                  <FormControl>
                    <Input inputMode="numeric" placeholder="131072" {...field} />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
          </div>

          <FormField
            control={form.control}
            name="notes"
            render={({ field }) => (
              <FormItem>
                <FormLabel>Notes</FormLabel>
                <FormControl>
                  <Textarea
                    rows={2}
                    placeholder="e.g. Multi-turn tool use through Chat Completions translation is not supported yet; plain chat works."
                    {...field}
                  />
                </FormControl>
                <p className="text-xs text-text-subtle">
                  Explains a limited or unknown capability above. Shown to tenants connecting
                  this model. Leave blank when there's nothing to say.
                </p>
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
            Cancel
          </Button>
          <Button type="submit" disabled={mutation.isPending}>
            {mutation.isPending ? 'Saving…' : isEdit ? 'Save changes' : 'Add model'}
          </Button>
        </DialogFooter>
      </form>
    </Form>
  )
}
