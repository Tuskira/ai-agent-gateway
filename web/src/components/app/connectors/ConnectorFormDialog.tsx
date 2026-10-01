import { useState } from 'react'
import { zodResolver } from '@hookform/resolvers/zod'
import { useForm } from 'react-hook-form'
import { z } from 'zod'
import { Plus, Trash2 } from 'lucide-react'
import { Link } from 'react-router-dom'
import { toast } from 'sonner'
import { useCreateConnector, useUpdateConnector } from '@/lib/queries'
import type { Connector } from '@/lib/connectors'
import { cn, slugify } from '@/lib/utils'
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
import { HeaderRowEditor } from '@/components/app/connectors/HeaderRowEditor'
import {
  headerRowError,
  headersToRows,
  newHeaderRow,
  rowsToHeaders,
  type HeaderRowState,
} from '@/components/app/connectors/header-rows'
import {
  newOverrideRow,
  overrideRowError,
  overridesToRows,
  rowsToOverrides,
  type OverrideRowState,
} from '@/components/app/connectors/tool-overrides'

const schema = z.object({
  name: z.string().trim().min(1, 'Name is required'),
  slug: z
    .string()
    .trim()
    .min(1, 'Slug is required')
    .regex(/^[a-z0-9-]+$/, 'Lowercase letters, numbers, and dashes only'),
  description: z.string().optional(),
  endpoint: z.string().trim().min(1, 'Endpoint is required'),
  // Kept as a string in the form (native number inputs round-trip through
  // strings) and parsed to a number when building the request payload.
  timeoutMs: z
    .string()
    .optional()
    .refine((v) => !v || (/^\d+$/.test(v) && Number(v) > 0), 'Must be a positive number'),
})

type FormValues = z.infer<typeof schema>

const REQUIRED_FIELD_COUNT = 3
// Tailwind class per possible "n of 3 required fields" count — width can't
// be computed inline (no `style={}` outside components/ui), so it's a
// lookup over the small, fixed set of valid counts instead.
const REQUIRED_FIELD_WIDTH = ['w-0', 'w-1/3', 'w-2/3', 'w-full'] as const

function authTypeSummary(rows: HeaderRowState[]): string {
  const first = rows[0]
  if (!first) return 'None'
  const types = new Set(rows.map((r) => r.type))
  if (types.size > 1) return 'Multiple'
  switch (first.type) {
    case 'static':
      return 'Static'
    case 'token_field':
      return 'Token field'
    case 'incoming_field':
      return 'Incoming field'
    case 'external':
      return 'Secret store'
    default:
      return 'None'
  }
}

interface ConnectorFormDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** When set, the dialog edits this connector instead of creating one. */
  connector?: Connector | null
  /** Create mode: prefill the name (and its derived slug), e.g. when
   * registering a Discovered MCP server. */
  initialName?: string
}

/**
 * Outer shell: only mounts `ConnectorFormBody` while the dialog is open, and
 * keys it by the target connector's id. That gives every open a fresh
 * mount, so `ConnectorFormBody` can initialize all of its state straight
 * from props — no effect-based "reset on prop change" needed.
 */
export function ConnectorFormDialog({
  open,
  onOpenChange,
  connector = null,
  initialName = '',
}: ConnectorFormDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[calc(100vh-3rem)] w-full flex-col overflow-hidden sm:max-w-4xl">
        {open ? (
          <ConnectorFormBody
            key={connector?.id ?? `create:${initialName}`}
            connector={connector}
            initialName={initialName}
            onDone={() => onOpenChange(false)}
            onCancel={() => onOpenChange(false)}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  )
}

interface ConnectorFormBodyProps {
  connector: Connector | null
  initialName: string
  onDone: () => void
  onCancel: () => void
}

function ConnectorFormBody({
  connector,
  initialName,
  onDone,
  onCancel,
}: ConnectorFormBodyProps) {
  const isEdit = connector !== null
  const createConnector = useCreateConnector()
  const updateConnector = useUpdateConnector(connector?.id ?? '')
  const mutation = isEdit ? updateConnector : createConnector

  const [tlsSkipVerify, setTlsSkipVerify] = useState(
    () => connector?.metadata?.tls?.insecure_skip_verify ?? false,
  )
  const [headerRows, setHeaderRows] = useState<HeaderRowState[]>(() =>
    headersToRows(connector?.metadata?.headers),
  )
  const [overrideRows, setOverrideRows] = useState<OverrideRowState[]>(() =>
    overridesToRows(connector?.metadata?.tool_arg_overrides),
  )
  // Per-connector policy for MCP requests the server can send back to the
  // agent (sampling/createMessage, elicitation/create, roots/list). Every
  // field is a trust decision and defaults to false.
  const [serverRequests, setServerRequests] = useState(() => ({
    sampling: connector?.metadata?.server_requests?.sampling ?? false,
    elicitation: connector?.metadata?.server_requests?.elicitation ?? false,
    roots: connector?.metadata?.server_requests?.roots ?? false,
  }))
  // Once the person edits the slug field directly, stop auto-deriving it
  // from the name. An existing connector's slug is treated as already
  // "touched" — editing its name should never silently change its slug.
  const [slugTouched, setSlugTouched] = useState(isEdit)

  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: connector?.name ?? initialName,
      slug: connector?.slug ?? (initialName ? slugify(initialName) : ''),
      description: connector?.description ?? '',
      endpoint: connector?.endpoint ?? '',
      timeoutMs: connector ? String(connector.timeout_ms) : undefined,
    },
  })

  const nameValue = form.watch('name')
  const slugValue = form.watch('slug')
  const endpointValue = form.watch('endpoint')
  const timeoutValue = form.watch('timeoutMs')

  const requiredDone = [nameValue, slugValue, endpointValue].filter(
    (v) => v.trim().length > 0,
  ).length
  const overridesCount = overrideRows.filter((r) => overrideRowError(r) === null).length

  function onSubmit(values: FormValues) {
    const rowErrors = headerRows
      .map(headerRowError)
      .filter((e): e is string => e !== null)
    if (rowErrors.length > 0) {
      toast.error('Fix the highlighted headers before saving.')
      return
    }
    const overrideErrors = overrideRows
      .map(overrideRowError)
      .filter((e): e is string => e !== null)
    if (overrideErrors.length > 0) {
      toast.error('Fix the highlighted tool argument overrides before saving.')
      return
    }

    const serverRequestsEnabled =
      serverRequests.sampling || serverRequests.elicitation || serverRequests.roots

    const input = {
      name: values.name,
      slug: values.slug,
      description: values.description || undefined,
      endpoint: values.endpoint,
      timeout_ms: values.timeoutMs ? Number(values.timeoutMs) : undefined,
      metadata: {
        headers: rowsToHeaders(headerRows),
        tool_arg_overrides: rowsToOverrides(overrideRows),
        tls: tlsSkipVerify ? { insecure_skip_verify: true } : undefined,
        server_requests: serverRequestsEnabled ? serverRequests : undefined,
        // Not edited here: keep a disabled connector disabled across an edit.
        enabled: connector?.metadata?.enabled === false ? false : undefined,
      },
    }

    mutation.mutate(input, {
      onSuccess: () => {
        toast.success(isEdit ? 'MCP updated' : 'MCP created')
        onDone()
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to save MCP')
      },
    })
  }

  return (
    <Form {...form}>
      <form
        onSubmit={form.handleSubmit(onSubmit)}
        className="flex min-h-0 flex-1 flex-col"
      >
        <DialogHeader>
          <DialogTitle>{isEdit ? `Edit ${connector?.name}` : 'Add MCP'}</DialogTitle>
          <DialogDescription>
            MCP tool server the gateway fronts. Agents only see tools granted through a
            profile.
          </DialogDescription>
        </DialogHeader>

        <div className="-mx-1 grid min-h-0 flex-1 grid-cols-1 overflow-hidden lg:grid-cols-[minmax(0,1fr)_264px]">
          <div className="flex min-h-0 flex-col gap-5 overflow-y-auto px-1 py-4 lg:pr-5">
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <FormField
                control={form.control}
                name="name"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Name *</FormLabel>
                    <FormControl>
                      <Input
                        placeholder="e.g. opencti"
                        {...field}
                        onChange={(e) => {
                          field.onChange(e)
                          if (!slugTouched) {
                            form.setValue('slug', slugify(e.target.value), {
                              shouldValidate: true,
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
                    <FormLabel>Slug *</FormLabel>
                    <FormControl>
                      <Input
                        className="font-mono"
                        placeholder="opencti"
                        {...field}
                        onChange={(e) => {
                          setSlugTouched(true)
                          field.onChange(e)
                        }}
                      />
                    </FormControl>
                    <p className="text-xs text-text-subtle">
                      Unique identifier (lowercase, no spaces)
                    </p>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </div>
            <FormField
              control={form.control}
              name="description"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Description</FormLabel>
                  <FormControl>
                    <Textarea
                      rows={2}
                      placeholder="What does this MCP expose?"
                      {...field}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_160px]">
              <FormField
                control={form.control}
                name="endpoint"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Endpoint URL *</FormLabel>
                    <FormControl>
                      <Input
                        className="font-mono"
                        placeholder="http://mcp-server:8001/mcp"
                        {...field}
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="timeoutMs"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Timeout (ms)</FormLabel>
                    <FormControl>
                      <Input type="number" placeholder="30000" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </div>

            <div className="flex flex-col gap-3">
              <div className="flex items-start justify-between gap-3">
                <div>
                  <div className="text-[14.5px] font-semibold text-foreground">
                    Authentication Headers
                  </div>
                  <p className="text-xs text-text-subtle">
                    Headers sent with every request to the backend MCP server.{' '}
                    <Link to="/credentials" className="font-medium text-text-link">
                      Manage credentials →
                    </Link>
                  </p>
                </div>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => setHeaderRows((rows) => [...rows, newHeaderRow()])}
                >
                  <Plus className="size-3.5" aria-hidden="true" />
                  Add Header
                </Button>
              </div>
              {headerRows.length === 0 ? (
                <div className="rounded-r-4 border border-dashed border-border-strong bg-bg-subtle px-4 py-5 text-center text-xs text-text-subtle">
                  No headers configured
                </div>
              ) : (
                <div className="flex flex-col gap-3">
                  {headerRows.map((row) => (
                    <HeaderRowEditor
                      key={row.id}
                      row={row}
                      onChange={(next) =>
                        setHeaderRows((rows) =>
                          rows.map((r) => (r.id === row.id ? next : r)),
                        )
                      }
                      onRemove={() =>
                        setHeaderRows((rows) => rows.filter((r) => r.id !== row.id))
                      }
                    />
                  ))}
                </div>
              )}
            </div>

            <div className="flex flex-col gap-3">
              <div className="flex items-start justify-between gap-3">
                <div>
                  <div className="text-[14.5px] font-semibold text-foreground">
                    Tool Argument Overrides
                  </div>
                  <p className="text-xs text-text-subtle">
                    Pin an argument to a fixed value for a specific cached tool.
                  </p>
                </div>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => setOverrideRows((rows) => [...rows, newOverrideRow()])}
                >
                  <Plus className="size-3.5" aria-hidden="true" />
                  Add Override
                </Button>
              </div>
              {overrideRows.length === 0 ? (
                <div className="rounded-r-4 border border-dashed border-border-strong bg-bg-subtle px-4 py-5 text-center text-xs text-text-subtle">
                  Click &quot;Add Override&quot; to pin tool arguments (e.g. projectId,
                  customerId, region)
                </div>
              ) : (
                <div className="flex flex-col gap-2">
                  {overrideRows.map((row) => {
                    const error = overrideRowError(row)
                    return (
                      <div key={row.id} className="flex flex-col gap-1.5">
                        <div className="flex items-center gap-2">
                          <Input
                            placeholder="Tool name"
                            className="flex-1 font-mono text-xs"
                            value={row.toolName}
                            onChange={(e) =>
                              setOverrideRows((rows) =>
                                rows.map((r) =>
                                  r.id === row.id
                                    ? { ...r, toolName: e.target.value }
                                    : r,
                                ),
                              )
                            }
                          />
                          <Input
                            placeholder="Argument (e.g. customerId)"
                            className="flex-1 font-mono text-xs"
                            value={row.argName}
                            onChange={(e) =>
                              setOverrideRows((rows) =>
                                rows.map((r) =>
                                  r.id === row.id ? { ...r, argName: e.target.value } : r,
                                ),
                              )
                            }
                          />
                          <span className="text-text-subtle">=</span>
                          <Input
                            placeholder="Value"
                            className="flex-1 font-mono text-xs"
                            value={row.value}
                            onChange={(e) =>
                              setOverrideRows((rows) =>
                                rows.map((r) =>
                                  r.id === row.id ? { ...r, value: e.target.value } : r,
                                ),
                              )
                            }
                          />
                          <Button
                            type="button"
                            variant="ghost"
                            size="icon-sm"
                            aria-label="Remove override"
                            className="shrink-0 text-sev-high hover:bg-sev-high-bg hover:text-sev-high"
                            onClick={() =>
                              setOverrideRows((rows) =>
                                rows.filter((r) => r.id !== row.id),
                              )
                            }
                          >
                            <Trash2 className="size-3.5" aria-hidden="true" />
                          </Button>
                        </div>
                        {error ? (
                          <p className="text-xs font-medium text-destructive">{error}</p>
                        ) : null}
                      </div>
                    )
                  })}
                </div>
              )}
            </div>

            <div className="flex items-center justify-between gap-3 rounded-r-4 border border-border px-3.5 py-3">
              <div>
                <Label>Skip TLS verification</Label>
                <p className="text-xs text-text-subtle">
                  Only for self-signed certs in trusted networks.
                </p>
              </div>
              <Switch checked={tlsSkipVerify} onCheckedChange={setTlsSkipVerify} />
            </div>

            <div className="flex flex-col gap-3">
              <div>
                <div className="text-[14.5px] font-semibold text-foreground">
                  Server-initiated requests
                </div>
                <p className="text-xs text-text-subtle">
                  Off by default. Turn on only for servers you trust.
                </p>
              </div>
              <div className="flex flex-col gap-2">
                <div className="flex items-center justify-between gap-3 rounded-r-4 border border-border px-3.5 py-3">
                  <div>
                    <Label>Sampling</Label>
                    <p className="text-xs text-text-subtle">
                      Let this server ask the agent&apos;s model to generate text. Uses
                      the agent&apos;s LLM budget.
                    </p>
                  </div>
                  <Switch
                    aria-label="Sampling"
                    checked={serverRequests.sampling}
                    onCheckedChange={(checked) =>
                      setServerRequests((s) => ({ ...s, sampling: checked }))
                    }
                  />
                </div>
                <div className="flex items-center justify-between gap-3 rounded-r-4 border border-border px-3.5 py-3">
                  <div>
                    <Label>Elicitation</Label>
                    <p className="text-xs text-text-subtle">
                      Let this server ask the person using the agent for input.
                    </p>
                  </div>
                  <Switch
                    aria-label="Elicitation"
                    checked={serverRequests.elicitation}
                    onCheckedChange={(checked) =>
                      setServerRequests((s) => ({ ...s, elicitation: checked }))
                    }
                  />
                </div>
                <div className="flex items-center justify-between gap-3 rounded-r-4 border border-border px-3.5 py-3">
                  <div>
                    <Label>Roots</Label>
                    <p className="text-xs text-text-subtle">
                      Let this server ask which folders the agent is working in.
                    </p>
                  </div>
                  <Switch
                    aria-label="Roots"
                    checked={serverRequests.roots}
                    onCheckedChange={(checked) =>
                      setServerRequests((s) => ({ ...s, roots: checked }))
                    }
                  />
                </div>
              </div>
            </div>
          </div>

          <div className="flex min-h-0 flex-col gap-4 overflow-y-auto border-t border-border px-1 pt-4 lg:border-t-0 lg:border-l lg:pt-0 lg:pl-5">
            <div className="text-[11px] font-semibold tracking-[.08em] text-text-subtle uppercase">
              Live Summary
            </div>
            <div className="flex flex-col gap-1.5">
              <div className="flex justify-between text-xs text-text-muted">
                <span>
                  {requiredDone} of {REQUIRED_FIELD_COUNT} required fields
                </span>
              </div>
              <div className="h-1.5 overflow-hidden rounded-r-pill bg-bg-muted">
                <div
                  className={cn(
                    'h-full rounded-r-pill transition-[width]',
                    REQUIRED_FIELD_WIDTH[requiredDone],
                    requiredDone === REQUIRED_FIELD_COUNT
                      ? 'bg-status-resolved'
                      : 'bg-primary',
                  )}
                />
              </div>
            </div>

            <div className="rounded-r-4 border border-border bg-bg-elevated px-3.5">
              <div className="border-b border-border py-2.5">
                <div className="text-[11.5px] text-text-subtle">Name</div>
                <div className="text-[13.5px] font-semibold break-all text-foreground">
                  {nameValue || '—'}
                </div>
              </div>
              <div className="border-b border-border py-2.5">
                <div className="text-[11.5px] text-text-subtle">Endpoint</div>
                <div className="font-mono text-xs break-all text-foreground">
                  {endpointValue || '—'}
                </div>
                <div className="mt-0.5 text-[11.5px] text-text-subtle">
                  timeout {timeoutValue || '30000'}ms
                </div>
              </div>
              <div className="border-b border-border py-2.5">
                <div className="text-[11.5px] text-text-subtle">Auth type</div>
                <div className="text-[13px] text-foreground">
                  {authTypeSummary(headerRows)}
                </div>
              </div>
              <div className="py-2.5">
                <div className="text-[11.5px] text-text-subtle">Overrides</div>
                <div className="text-[13px] text-foreground">{overridesCount}</div>
              </div>
            </div>

            <p className="text-xs text-text-subtle">
              After creation the gateway runs initialize + tools/list and caches the
              discovered tools. Grant them to agents through a profile.
            </p>
          </div>
        </div>

        <DialogFooter>
          <Button type="button" variant="outline" onClick={onCancel}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={mutation.isPending || requiredDone < REQUIRED_FIELD_COUNT}
          >
            {mutation.isPending ? 'Saving…' : isEdit ? 'Save changes' : 'Create'}
          </Button>
        </DialogFooter>
      </form>
    </Form>
  )
}
