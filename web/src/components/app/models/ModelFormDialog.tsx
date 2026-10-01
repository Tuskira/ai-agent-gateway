import { useState } from 'react'
import { zodResolver } from '@hookform/resolvers/zod'
import { useForm } from 'react-hook-form'
import { useQueryClient } from '@tanstack/react-query'
import { z } from 'zod'
import { Plus } from 'lucide-react'
import { toast } from 'sonner'
import { updateCredential } from '@/lib/credentials'
import {
  useCreateCredential,
  useCreateModel,
  useCredentialsList,
  useUpdateModel,
} from '@/lib/queries'
import {
  MODEL_MAX_RPM,
  MODEL_NAME_PATTERN,
  type ModelInput,
  type ModelLimits,
  type ModelPrice,
  type RegisteredModel,
} from '@/lib/models'
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
import { Textarea } from '@/components/ui/textarea'
import { TargetRowEditor } from './TargetRowEditor'
import {
  apiKeyCredentialName,
  newTargetRow,
  rowsToTargets,
  targetRowError,
  targetsToRows,
  type TargetRowState,
} from './targets'

/** A non-negative number string, or empty (unset). Mirrors
 * ConnectorFormDialog's `timeoutMs` pattern: kept as a string in the form
 * (native number inputs round-trip through strings) and parsed when
 * building the request payload. */
function optionalNonNegativeNumber(v: string | undefined) {
  return !v || (/^\d+(\.\d+)?$/.test(v) && Number(v) >= 0)
}

/** A non-negative integer string, or empty (unset). */
function optionalNonNegativeInteger(v: string | undefined) {
  return !v || /^\d+$/.test(v)
}

const priceFieldSchema = z.string().optional().refine(optionalNonNegativeNumber, 'Must be a non-negative number')
const usdFieldSchema = z.string().optional().refine(optionalNonNegativeNumber, 'Must be a non-negative number')
const integerFieldSchema = z.string().optional().refine(optionalNonNegativeInteger, 'Must be a whole number, 0 or more')

const schema = z
  .object({
    name: z
      .string()
      .trim()
      .min(1, 'Name is required')
      .regex(MODEL_NAME_PATTERN, 'Lowercase letters, numbers, dot, underscore, and dash only (max 64 chars)'),
    description: z.string().max(1024, 'At most 1024 characters').optional(),
    priceInput: priceFieldSchema,
    priceOutput: priceFieldSchema,
    priceCacheRead: priceFieldSchema,
    priceCacheWrite: priceFieldSchema,
    limitDailyUsd: usdFieldSchema,
    limitMonthlyUsd: usdFieldSchema,
    limitRpm: integerFieldSchema.refine(
      (v) => !v || Number(v) <= MODEL_MAX_RPM,
      `At most ${MODEL_MAX_RPM}`,
    ),
    limitMaxTokens: integerFieldSchema,
  })
  .superRefine((values, ctx) => {
    // The API stores a price as a flat override: a missing input or output
    // rate would be saved as 0 (free), so both are required once any
    // price field is set.
    const anyPrice =
      values.priceInput || values.priceOutput || values.priceCacheRead || values.priceCacheWrite
    if (!anyPrice) return
    for (const field of ['priceInput', 'priceOutput'] as const) {
      if (!values[field]) {
        ctx.addIssue({
          code: 'custom',
          path: [field],
          message: 'Required when a pricing override is set',
        })
      }
    }
  })

type FormValues = z.infer<typeof schema>

function priceToForm(price?: ModelPrice | null) {
  return {
    priceInput: price ? String(price.input) : '',
    priceOutput: price ? String(price.output) : '',
    priceCacheRead: price?.cache_read !== undefined ? String(price.cache_read) : '',
    priceCacheWrite: price?.cache_write !== undefined ? String(price.cache_write) : '',
  }
}

function formToPrice(values: FormValues): ModelPrice | undefined {
  if (!values.priceInput || !values.priceOutput) return undefined
  const price: ModelPrice = {
    input: Number(values.priceInput),
    output: Number(values.priceOutput),
  }
  if (values.priceCacheRead) price.cache_read = Number(values.priceCacheRead)
  if (values.priceCacheWrite) price.cache_write = Number(values.priceCacheWrite)
  return price
}

function limitsToForm(limits?: ModelLimits | null) {
  return {
    limitDailyUsd: limits?.daily_usd !== undefined ? String(limits.daily_usd) : '',
    limitMonthlyUsd: limits?.monthly_usd !== undefined ? String(limits.monthly_usd) : '',
    limitRpm: limits?.rpm !== undefined ? String(limits.rpm) : '',
    limitMaxTokens: limits?.max_tokens !== undefined ? String(limits.max_tokens) : '',
  }
}

/** `undefined` when every field is blank: the API refuses an empty
 * `limits` object, and leaving it out of a PUT clears the limits. */
function formToLimits(values: FormValues): ModelLimits | undefined {
  const limits: ModelLimits = {}
  if (values.limitDailyUsd) limits.daily_usd = Number(values.limitDailyUsd)
  if (values.limitMonthlyUsd) limits.monthly_usd = Number(values.limitMonthlyUsd)
  if (values.limitRpm) limits.rpm = Number(values.limitRpm)
  if (values.limitMaxTokens) limits.max_tokens = Number(values.limitMaxTokens)
  return Object.keys(limits).length > 0 ? limits : undefined
}

interface ModelFormDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** When set, the dialog edits this model instead of creating one.
   * Platform-scope models never reach this dialog — the table hides the
   * edit action for them (they're read-only; see `ModelsPage`). */
  model?: RegisteredModel | null
}

/** Outer shell: only mounts the body while open, keyed by the target
 * model's id, so the body can initialize its state straight from props —
 * same pattern as `ConnectorFormDialog`. */
export function ModelFormDialog({ open, onOpenChange, model = null }: ModelFormDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[calc(100vh-3rem)] w-full flex-col overflow-hidden sm:max-w-2xl">
        {open ? (
          <ModelFormBody
            key={model?.id ?? 'create'}
            model={model}
            onDone={() => onOpenChange(false)}
            onCancel={() => onOpenChange(false)}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  )
}

interface ModelFormBodyProps {
  model: RegisteredModel | null
  onDone: () => void
  onCancel: () => void
}

function ModelFormBody({ model, onDone, onCancel }: ModelFormBodyProps) {
  const isEdit = model !== null
  const createModel = useCreateModel()
  const updateModel = useUpdateModel(model?.id ?? '')
  const mutation = isEdit ? updateModel : createModel
  const createCredential = useCreateCredential()
  const credentialsQuery = useCredentialsList()
  const queryClient = useQueryClient()
  const [replacingKey, setReplacingKey] = useState(false)
  const saving = mutation.isPending || createCredential.isPending || replacingKey

  const [enabled, setEnabled] = useState(model?.enabled ?? true)
  const [targetRows, setTargetRows] = useState<TargetRowState[]>(() => targetsToRows(model?.targets))

  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: model?.name ?? '',
      description: model?.description ?? '',
      ...priceToForm(model?.price),
      ...limitsToForm(model?.limits),
    },
  })

  /**
   * Stores each typed API key as ONE credential per provider and points
   * its row at it, in order -- `<label>-api-key` (`<vendor>-api-key` with
   * no label), not one per model: a credential already on file by that
   * name is replaced (`PUT`) in place rather than duplicated, so
   * connecting a second model to a provider you've already typed a key
   * for reuses the same credential (see ADDENDUM 1 item 5,
   * MODEL-CATALOG-CONTRACT.md). The model API refuses a credential name
   * that does not exist, so this runs before the model is saved. Rows
   * already stored are kept in state even when a later one fails, so a
   * retry never stores a key twice. Returns null (after a toast) when a
   * key could not be stored.
   */
  async function storeTypedKeys(): Promise<TargetRowState[] | null> {
    const existing = new Set((credentialsQuery.data?.items ?? []).map((c) => c.name))
    const rows: TargetRowState[] = []
    setReplacingKey(true)
    try {
      for (const row of targetRows) {
        if (row.apiKey === undefined) {
          rows.push(row)
          continue
        }
        const name = apiKeyCredentialName(row)
        const payload = { api_key: row.apiKey.trim() }
        try {
          if (existing.has(name)) {
            await updateCredential(name, { payload })
          } else {
            await createCredential.mutateAsync({ name, type: 'api_key', payload })
          }
        } catch (err) {
          setTargetRows([...rows, ...targetRows.slice(rows.length)])
          toast.error(
            `Could not store the API key for target ${rows.length + 1}: ${
              err instanceof Error ? err.message : 'request failed'
            }`,
          )
          return null
        }
        existing.add(name)
        rows.push({ ...row, credential: name, apiKey: undefined })
      }
    } finally {
      setReplacingKey(false)
    }
    setTargetRows(rows)
    // updateCredential (a direct call, not the useUpdateCredential hook)
    // doesn't invalidate the credentials cache on its own.
    queryClient.invalidateQueries({ queryKey: ['credentials'] })
    return rows
  }

  async function onSubmit(values: FormValues) {
    const rowErrors = targetRows.map(targetRowError).filter((e): e is string => e !== null)
    if (rowErrors.length > 0) {
      toast.error('Fix the highlighted targets before saving.')
      return
    }
    const rows = await storeTypedKeys()
    if (!rows) return
    const targets = rowsToTargets(rows)
    if (targets.length === 0) {
      toast.error('Add at least one target.')
      return
    }

    const input: ModelInput = {
      name: values.name,
      description: values.description || undefined,
      enabled,
      targets,
      price: formToPrice(values),
      limits: formToLimits(values),
    }

    mutation.mutate(input, {
      onSuccess: () => {
        toast.success(isEdit ? 'Model updated' : 'Model created')
        onDone()
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to save model')
      },
    })
  }

  function moveTarget(id: string, direction: -1 | 1) {
    setTargetRows((rows) => {
      const index = rows.findIndex((r) => r.id === id)
      const swapWith = index + direction
      if (index < 0 || swapWith < 0 || swapWith >= rows.length) return rows
      const next = [...rows]
      const moved = next[index]!
      next[index] = next[swapWith]!
      next[swapWith] = moved
      return next
    })
  }

  return (
    <Form {...form}>
      <form onSubmit={form.handleSubmit(onSubmit)} className="flex min-h-0 flex-1 flex-col">
        <DialogHeader>
          <DialogTitle>{isEdit ? `Edit ${model?.name}` : 'Add model'}</DialogTitle>
          <DialogDescription>
            Registers a model name agents can call through the gateway's existing endpoints,
            routed to the target(s) below.
          </DialogDescription>
        </DialogHeader>

        <div className="-mx-1 flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto px-1 py-4">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <FormField
              control={form.control}
              name="name"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Name *</FormLabel>
                  <FormControl>
                    <Input placeholder="e.g. claude-sonnet" className="font-mono" {...field} />
                  </FormControl>
                  <p className="text-xs text-text-subtle">
                    What clients send as "model". Lowercase letters, numbers, dot, underscore,
                    dash.
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

          <FormField
            control={form.control}
            name="description"
            render={({ field }) => (
              <FormItem>
                <FormLabel>Description</FormLabel>
                <FormControl>
                  <Textarea rows={2} placeholder="What is this model used for?" {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />

          <div className="flex flex-col gap-3">
            <div className="flex items-start justify-between gap-3">
              <div>
                <div className="text-[14.5px] font-semibold text-foreground">Targets</div>
                <p className="text-xs text-text-subtle">
                  Tried in order; the gateway falls back to the next one on a connection
                  failure before any response reaches the client.
                </p>
              </div>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => setTargetRows((rows) => [...rows, newTargetRow()])}
              >
                <Plus className="size-3.5" aria-hidden="true" />
                Add target
              </Button>
            </div>
            <div className="flex flex-col gap-3">
              {targetRows.map((row, index) => (
                <TargetRowEditor
                  key={row.id}
                  row={row}
                  index={index}
                  total={targetRows.length}
                  onChange={(next) =>
                    setTargetRows((rows) => rows.map((r) => (r.id === row.id ? next : r)))
                  }
                  onRemove={() =>
                    setTargetRows((rows) => rows.filter((r) => r.id !== row.id))
                  }
                  onMove={(direction) => moveTarget(row.id, direction)}
                />
              ))}
            </div>
          </div>

          <div className="flex flex-col gap-3">
            <div>
              <div className="text-[14.5px] font-semibold text-foreground">
                Pricing override
              </div>
              <p className="text-xs text-text-subtle">
                Optional — USD per 1M tokens. Leave blank to use the gateway's rate card.
              </p>
            </div>
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
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
              <FormField
                control={form.control}
                name="priceCacheRead"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel className="text-xs">Cache read</FormLabel>
                    <FormControl>
                      <Input inputMode="decimal" placeholder="0.30" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="priceCacheWrite"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel className="text-xs">Cache write</FormLabel>
                    <FormControl>
                      <Input inputMode="decimal" placeholder="3.75" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </div>
          </div>
          <div className="flex flex-col gap-3">
            <div>
              <div className="text-[14.5px] font-semibold text-foreground">Limits</div>
              <p className="text-xs text-text-subtle">
                Optional — applied to this model name across every key of the tenant, on top
                of each key's own limits. 0 blocks. Leave blank for no limit.
              </p>
            </div>
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
              <FormField
                control={form.control}
                name="limitDailyUsd"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel className="text-xs">Daily budget (USD)</FormLabel>
                    <FormControl>
                      <Input inputMode="decimal" placeholder="5.00" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="limitMonthlyUsd"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel className="text-xs">Monthly budget (USD)</FormLabel>
                    <FormControl>
                      <Input inputMode="decimal" placeholder="100.00" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="limitRpm"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel className="text-xs">Requests / minute</FormLabel>
                    <FormControl>
                      <Input inputMode="numeric" placeholder="60" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="limitMaxTokens"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel className="text-xs">Max tokens</FormLabel>
                    <FormControl>
                      <Input inputMode="numeric" placeholder="4096" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </div>
          </div>
        </div>

        <DialogFooter>
          <Button type="button" variant="outline" onClick={onCancel}>
            Cancel
          </Button>
          <Button type="submit" disabled={saving}>
            {saving ? 'Saving…' : isEdit ? 'Save changes' : 'Create'}
          </Button>
        </DialogFooter>
      </form>
    </Form>
  )
}
