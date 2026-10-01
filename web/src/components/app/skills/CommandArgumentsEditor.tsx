import { Plus, Trash2 } from 'lucide-react'
import { newArgumentRow, type ArgumentRow } from './command-arguments'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'

interface CommandArgumentsEditorProps {
  rows: ArgumentRow[]
  /** Per-row error, parallel to `rows` (see `commandArgumentError`). */
  rowErrors?: (string | null)[]
  onChange: (rows: ArgumentRow[]) => void
}

/** Add/remove rows for a command's `{{placeholder}}` arguments -- name,
 * optional description, and whether it's required. Placeholders in the
 * SKILL.md body that aren't declared here fail validation before submit
 * (see `findUndeclaredPlaceholders`). */
export function CommandArgumentsEditor({
  rows,
  rowErrors,
  onChange,
}: CommandArgumentsEditorProps) {
  function updateRow(id: string, patch: Partial<ArgumentRow>) {
    onChange(rows.map((r) => (r.id === id ? { ...r, ...patch } : r)))
  }

  function removeRow(id: string) {
    onChange(rows.filter((r) => r.id !== id))
  }

  return (
    <div className="flex flex-col gap-3">
      {rows.map((row, index) => (
        <div key={row.id} className="flex flex-col gap-1.5 rounded-r-3 border border-border p-3">
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-[1fr_1.5fr_auto_auto] sm:items-end">
            <div className="flex flex-col gap-1">
              <Label className="text-xs">Name</Label>
              <Input
                className="h-8 font-mono text-xs"
                value={row.name}
                placeholder="e.g. severity"
                onChange={(e) => updateRow(row.id, { name: e.target.value })}
                aria-label={`Argument ${index + 1} name`}
              />
            </div>
            <div className="flex flex-col gap-1">
              <Label className="text-xs">Description</Label>
              <Input
                className="h-8 text-xs"
                value={row.description}
                placeholder="Optional"
                onChange={(e) => updateRow(row.id, { description: e.target.value })}
                aria-label={`Argument ${index + 1} description`}
              />
            </div>
            <div className="flex flex-col items-start gap-1">
              <Label className="text-xs">Required</Label>
              <div className="flex h-8 items-center">
                <Switch
                  checked={row.required}
                  onCheckedChange={(v) => updateRow(row.id, { required: v })}
                  aria-label={`Argument ${index + 1} required`}
                />
              </div>
            </div>
            <Button
              type="button"
              variant="ghost"
              size="icon-sm"
              aria-label={`Remove argument ${index + 1}`}
              onClick={() => removeRow(row.id)}
            >
              <Trash2 className="size-3.5" aria-hidden="true" />
            </Button>
          </div>
          {rowErrors?.[index] ? (
            <p className="text-xs text-destructive">{rowErrors[index]}</p>
          ) : null}
        </div>
      ))}
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="w-fit"
        onClick={() => onChange([...rows, newArgumentRow()])}
      >
        <Plus className="size-3.5" aria-hidden="true" />
        Add argument
      </Button>
    </div>
  )
}
