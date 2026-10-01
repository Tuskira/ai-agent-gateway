import { useState, type FormEvent } from 'react'
import { Plus, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { useSaveCatalogEntry } from '@/lib/queries'
import {
  credentialFieldCount,
  entryToInput,
  type CatalogAuthKind,
  type CatalogEntry,
  type CatalogEntryInput,
  type CatalogField,
} from '@/lib/mcpCatalog'
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
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'

const KINDS: { value: CatalogAuthKind; label: string }[] = [
  { value: 'none', label: 'No auth' },
  { value: 'bearer', label: 'Bearer token' },
  { value: 'basic', label: 'Basic auth (two fields)' },
  { value: 'header', label: 'API key header' },
  { value: 'oauth', label: 'OAuth (not addable yet)' },
]

const BLANK: CatalogEntryInput = {
  slug: '',
  name: '',
  description: '',
  icon: '',
  category: '',
  url: '',
  url_overridable: false,
  auth: { kind: 'none', fields: [] },
  default_headers: {},
  suggested_tools: [],
  docs_url: '',
  enabled: true,
}

function blankField(): CatalogField {
  return { name: '', label: '', secret: false, required: false }
}

interface FormProps {
  entry: CatalogEntry | null
  onOpenChange: (open: boolean) => void
}

function EntryForm({ entry, onOpenChange }: FormProps) {
  const save = useSaveCatalogEntry()
  const isEdit = entry !== null
  const [draft, setDraft] = useState<CatalogEntryInput>(() =>
    entry ? entryToInput(entry) : BLANK,
  )
  const [slugTouched, setSlugTouched] = useState(false)
  const [error, setError] = useState<string | null>(null)

  function patch(p: Partial<CatalogEntryInput>) {
    setDraft((d) => ({ ...d, ...p }))
  }
  function setFields(fields: CatalogField[]) {
    setDraft((d) => ({ ...d, auth: { ...d.auth, fields } }))
  }
  function setKind(kind: CatalogAuthKind) {
    setDraft((d) => ({
      ...d,
      auth: {
        ...d.auth,
        kind,
        // Keep the URL-routed fields; resize the credential fields to what
        // the new kind takes.
        fields: resizeCredentialFields(d.auth.fields, credentialFieldCount(kind)),
        header_template:
          kind === 'header' ? (d.auth.header_template ?? { name: '' }) : undefined,
      },
    }))
  }

  function submit(e: FormEvent) {
    e.preventDefault()
    setError(null)
    if (draft.name.trim() === '') return setError('Name is required')
    if (!isEdit && (draft.slug ?? '') === '') return setError('Slug is required')
    if (draft.url.trim() === '') return setError('Server URL is required')
    const body: CatalogEntryInput = {
      ...draft,
      name: draft.name.trim(),
      url: draft.url.trim(),
    }
    save.mutate(
      { editSlug: entry?.slug, input: body },
      {
        onSuccess: () => {
          toast.success(isEdit ? 'Catalog entry updated' : 'Catalog entry created')
          onOpenChange(false)
        },
        onError: (err) =>
          setError(err instanceof Error ? err.message : 'Failed to save catalog entry'),
      },
    )
  }

  const credCount = credentialFieldCount(draft.auth.kind)

  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <DialogHeader>
        <DialogTitle>{isEdit ? `Edit ${entry.name}` : 'New catalog entry'}</DialogTitle>
        <DialogDescription>
          Entries you create are private to your tenant. One with the same slug as a
          platform entry replaces it for your tenant.
        </DialogDescription>
      </DialogHeader>

      <div className="grid gap-4 sm:grid-cols-2">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="ce-name">Name</Label>
          <Input
            id="ce-name"
            value={draft.name}
            onChange={(e) =>
              patch({
                name: e.target.value,
                ...(!isEdit && !slugTouched ? { slug: slugify(e.target.value) } : {}),
              })
            }
          />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="ce-slug">Slug</Label>
          <Input
            id="ce-slug"
            value={draft.slug ?? ''}
            disabled={isEdit}
            className="font-mono text-xs"
            onChange={(e) => {
              setSlugTouched(true)
              patch({ slug: e.target.value })
            }}
          />
        </div>
      </div>

      <div className="flex flex-col gap-1.5">
        <Label htmlFor="ce-desc">Description</Label>
        <Input
          id="ce-desc"
          value={draft.description}
          onChange={(e) => patch({ description: e.target.value })}
        />
      </div>

      <div className="grid gap-4 sm:grid-cols-2">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="ce-url">Server URL</Label>
          <Input
            id="ce-url"
            value={draft.url}
            placeholder="https://mcp.example.com/mcp"
            className="font-mono text-xs"
            onChange={(e) => patch({ url: e.target.value })}
          />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="ce-category">Category</Label>
          <Input
            id="ce-category"
            value={draft.category}
            onChange={(e) => patch({ category: e.target.value })}
          />
        </div>
      </div>

      <label className="flex items-center gap-2 text-[13px]">
        <input
          type="checkbox"
          checked={draft.url_overridable}
          onChange={(e) => patch({ url_overridable: e.target.checked })}
        />
        Let people change the URL when adding (another region or a self-hosted server)
      </label>

      <div className="flex flex-col gap-1.5">
        <Label htmlFor="ce-kind">Authentication</Label>
        <NativeSelect
          id="ce-kind"
          value={draft.auth.kind}
          onChange={(e) => setKind(e.target.value as CatalogAuthKind)}
        >
          {KINDS.map((k) => (
            <NativeSelectOption key={k.value} value={k.value}>
              {k.label}
            </NativeSelectOption>
          ))}
        </NativeSelect>
      </div>

      {draft.auth.kind === 'header' ? (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="ce-header-name">Header name</Label>
          <Input
            id="ce-header-name"
            value={draft.auth.header_template?.name ?? ''}
            placeholder="X-Api-Key"
            onChange={(e) =>
              setDraft((d) => ({
                ...d,
                auth: {
                  ...d.auth,
                  header_template: { ...d.auth.header_template, name: e.target.value },
                },
              }))
            }
          />
        </div>
      ) : null}

      {draft.auth.kind !== 'oauth' && draft.auth.kind !== 'none' ? (
        <p className="text-[12px] text-text-subtle">
          {draft.auth.kind === 'basic'
            ? 'Basic auth takes exactly two credential fields (for example a public and a secret key).'
            : 'This kind takes exactly one credential field.'}{' '}
          A field with a query parameter goes into the URL instead.
        </p>
      ) : null}

      <div className="flex flex-col gap-2">
        {draft.auth.fields.map((f, i) => (
          <div
            key={i}
            className="grid grid-cols-[1fr_1fr_auto] items-end gap-2 rounded-lg border border-border p-2"
          >
            <div className="flex flex-col gap-1">
              <Label htmlFor={`ce-f-name-${i}`} className="text-[11px]">
                Field name
              </Label>
              <Input
                id={`ce-f-name-${i}`}
                value={f.name}
                className="font-mono text-xs"
                onChange={(e) =>
                  setFields(
                    replaceAt(draft.auth.fields, i, { ...f, name: e.target.value }),
                  )
                }
              />
            </div>
            <div className="flex flex-col gap-1">
              <Label htmlFor={`ce-f-label-${i}`} className="text-[11px]">
                Label
              </Label>
              <Input
                id={`ce-f-label-${i}`}
                value={f.label}
                onChange={(e) =>
                  setFields(
                    replaceAt(draft.auth.fields, i, { ...f, label: e.target.value }),
                  )
                }
              />
            </div>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              aria-label={`Remove field ${f.name || i + 1}`}
              onClick={() => setFields(draft.auth.fields.filter((_, j) => j !== i))}
            >
              <Trash2 className="size-4" aria-hidden="true" />
            </Button>
            <div className="col-span-3 flex flex-wrap items-center gap-4 text-[12px]">
              <label className="flex items-center gap-1.5">
                <input
                  type="checkbox"
                  checked={!!f.secret}
                  disabled={!!f.query}
                  onChange={(e) =>
                    setFields(
                      replaceAt(draft.auth.fields, i, { ...f, secret: e.target.checked }),
                    )
                  }
                />
                Secret
              </label>
              <label className="flex items-center gap-1.5">
                <input
                  type="checkbox"
                  checked={!!f.required}
                  onChange={(e) =>
                    setFields(
                      replaceAt(draft.auth.fields, i, {
                        ...f,
                        required: e.target.checked,
                      }),
                    )
                  }
                />
                Required
              </label>
              <label className="flex items-center gap-1.5">
                URL query parameter
                <Input
                  aria-label={`Query parameter for ${f.name || 'field'}`}
                  value={f.query ?? ''}
                  className="h-7 w-32 font-mono text-xs"
                  onChange={(e) =>
                    setFields(
                      replaceAt(draft.auth.fields, i, {
                        ...f,
                        query: e.target.value || undefined,
                        secret: e.target.value ? false : f.secret,
                      }),
                    )
                  }
                />
              </label>
            </div>
          </div>
        ))}
        {draft.auth.kind !== 'none' && draft.auth.kind !== 'oauth' ? (
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="self-start"
            onClick={() => setFields([...draft.auth.fields, blankField()])}
          >
            <Plus className="size-4" aria-hidden="true" />
            Add field
          </Button>
        ) : null}
        {draft.auth.kind !== 'none' && draft.auth.kind !== 'oauth' ? (
          <p className="text-[11.5px] text-text-subtle">
            {credCount} credential field{credCount === 1 ? '' : 's'} needed.
          </p>
        ) : null}
      </div>

      <label className="flex items-center gap-2 text-[13px]">
        <input
          type="checkbox"
          checked={draft.enabled}
          onChange={(e) => patch({ enabled: e.target.checked })}
        />
        Enabled (disabled entries are hidden from everyone but admins)
      </label>

      {error ? (
        <p role="alert" className="text-[13px] text-destructive">
          {error}
        </p>
      ) : null}

      <DialogFooter>
        <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
          Cancel
        </Button>
        <Button type="submit" disabled={save.isPending}>
          {save.isPending ? 'Saving...' : isEdit ? 'Save changes' : 'Create entry'}
        </Button>
      </DialogFooter>
    </form>
  )
}

function replaceAt<T>(list: T[], i: number, value: T): T[] {
  return list.map((x, j) => (j === i ? value : x))
}

/** Keeps every URL-routed (query) field and trims or pads the credential
 * fields to `want`. */
function resizeCredentialFields(fields: CatalogField[], want: number): CatalogField[] {
  const query = fields.filter((f) => f.query)
  const cred = fields.filter((f) => !f.query)
  if (want === 0) return query
  const kept = cred.slice(0, want)
  while (kept.length < want) kept.push(blankField())
  return [...kept, ...query]
}

interface Props {
  open: boolean
  /** The entry being edited, or `null` to create a new one. */
  entry: CatalogEntry | null
  onOpenChange: (open: boolean) => void
}

/** Create or edit a tenant catalog entry. */
export function CatalogEntryFormDialog({ open, entry, onOpenChange }: Props) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100vh-3rem)] w-full overflow-y-auto sm:max-w-2xl">
        {open ? (
          <EntryForm key={entry?.id ?? 'new'} entry={entry} onOpenChange={onOpenChange} />
        ) : null}
      </DialogContent>
    </Dialog>
  )
}
