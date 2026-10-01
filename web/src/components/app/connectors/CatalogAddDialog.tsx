import { useState, type FormEvent } from 'react'
import { toast } from 'sonner'
import { useAddCatalogEntry } from '@/lib/queries'
import { ApiError } from '@/lib/api'
import { buildAddBody, type AddCatalogResult, type CatalogEntry } from '@/lib/mcpCatalog'
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

interface Props {
  entry: CatalogEntry | null
  onOpenChange: (open: boolean) => void
  onAdded?: (result: AddCatalogResult) => void
}

/** The form itself, keyed by entry so its state resets per entry. */
function AddForm({
  entry,
  onOpenChange,
  onAdded,
}: {
  entry: CatalogEntry
  onOpenChange: (open: boolean) => void
  onAdded?: (result: AddCatalogResult) => void
}) {
  const add = useAddCatalogEntry()
  const [values, setValues] = useState<Record<string, string>>({})
  const [name, setName] = useState(entry.name)
  const [url, setUrl] = useState(entry.url)
  const [error, setError] = useState<string | null>(null)

  const missing = entry.auth.fields.filter(
    (f) => f.required && (values[f.name] ?? '').trim() === '',
  )

  function submit(e: FormEvent) {
    e.preventDefault()
    setError(null)
    if (missing.length > 0) {
      setError(`${missing[0]?.label} is required`)
      return
    }
    add.mutate(
      { slug: entry.slug, input: buildAddBody(entry, values, name, url) },
      {
        onSuccess: (result) => {
          const { status, tools_discovered } = result.discovery
          if (status === 'unhealthy') {
            toast.warning(
              `${entry.name} added, but it reports unhealthy. Check the credentials.`,
            )
          } else {
            toast.success(`${entry.name} added (${tools_discovered} tools)`)
          }
          onOpenChange(false)
          onAdded?.(result)
        },
        onError: (err) => {
          setError(
            err instanceof ApiError || err instanceof Error
              ? err.message
              : 'Failed to add MCP',
          )
        },
      },
    )
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <DialogHeader>
        <DialogTitle>Add {entry.name}</DialogTitle>
        <DialogDescription>
          {entry.auth.kind === 'none'
            ? 'This server needs no credentials. Adding it creates an MCP in your tenant.'
            : 'Credentials are stored encrypted in your tenant and are never shown again.'}
        </DialogDescription>
      </DialogHeader>

      <div className="flex flex-col gap-1.5">
        <Label htmlFor="catalog-name">Name</Label>
        <Input id="catalog-name" value={name} onChange={(e) => setName(e.target.value)} />
      </div>

      {entry.url_overridable ? (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="catalog-url">Server URL</Label>
          <Input
            id="catalog-url"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            className="font-mono text-xs"
          />
          <p className="text-[11.5px] text-text-subtle">
            Change this for another region or a self-hosted server.
          </p>
        </div>
      ) : null}

      {entry.auth.fields.map((f) => {
        const id = `catalog-field-${f.name}`
        return (
          <div key={f.name} className="flex flex-col gap-1.5">
            <Label htmlFor={id}>
              {f.label}
              {f.required ? <span aria-hidden="true"> *</span> : null}
            </Label>
            <Input
              id={id}
              type={f.secret ? 'password' : 'text'}
              autoComplete="off"
              placeholder={f.placeholder}
              value={values[f.name] ?? ''}
              onChange={(e) => setValues((v) => ({ ...v, [f.name]: e.target.value }))}
            />
            {f.help ? <p className="text-[11.5px] text-text-subtle">{f.help}</p> : null}
          </div>
        )
      })}

      {error ? (
        <p role="alert" className="text-[13px] text-destructive">
          {error}
        </p>
      ) : null}

      <DialogFooter>
        <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
          Cancel
        </Button>
        <Button type="submit" disabled={add.isPending}>
          {add.isPending ? 'Adding...' : 'Add to tenant'}
        </Button>
      </DialogFooter>
    </form>
  )
}

/** "Add to tenant" dialog, generated from the catalog entry's `auth.fields`. */
export function CatalogAddDialog({ entry, onOpenChange, onAdded }: Props) {
  return (
    <Dialog open={entry !== null} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100vh-3rem)] w-full overflow-y-auto sm:max-w-lg">
        {entry ? (
          <AddForm
            key={entry.id}
            entry={entry}
            onOpenChange={onOpenChange}
            onAdded={onAdded}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  )
}
