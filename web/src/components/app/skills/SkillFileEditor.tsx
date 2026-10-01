import { Plus, Trash2 } from 'lucide-react'
import { SKILL_MD_PATH } from '@/lib/skill-validation'
import { newSkillFileRow, type SkillFileRow } from './skill-files'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'

interface SkillFileEditorProps {
  rows: SkillFileRow[]
  /** Per-row error, parallel to `rows` (see `validateSkillFiles`). */
  fileErrors?: (string | null)[]
  onChange: (rows: SkillFileRow[]) => void
  /** Called when the SKILL.md row's content is edited directly -- the
   * caller uses this to stop auto-syncing the template from the name. */
  onSkillMdEdit?: () => void
}

/**
 * Add/remove file rows shared by the create dialog and the "New version"
 * editor. `SKILL.md` is always present and its path can't be changed or
 * removed (the registry requires a root SKILL.md); every other row is a
 * path + content pair the person can add or remove freely.
 */
export function SkillFileEditor({
  rows,
  fileErrors,
  onChange,
  onSkillMdEdit,
}: SkillFileEditorProps) {
  function updateRow(id: string, patch: Partial<SkillFileRow>) {
    onChange(rows.map((r) => (r.id === id ? { ...r, ...patch } : r)))
  }

  function removeRow(id: string) {
    onChange(rows.filter((r) => r.id !== id))
  }

  return (
    <div className="flex flex-col gap-3">
      {rows.map((row, index) => {
        const isSkillMd = row.path === SKILL_MD_PATH
        const error = fileErrors?.[index]
        return (
          <div
            key={row.id}
            className="flex flex-col gap-1.5 rounded-r-3 border border-border p-3"
          >
            <div className="flex items-center gap-2">
              <Label className="text-xs whitespace-nowrap">Path</Label>
              <Input
                className="h-7 flex-1 font-mono text-xs"
                value={row.path}
                readOnly={isSkillMd}
                placeholder="e.g. reference.md"
                onChange={(e) => updateRow(row.id, { path: e.target.value })}
                aria-label={isSkillMd ? 'SKILL.md path' : `File ${index + 1} path`}
              />
              {!isSkillMd ? (
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  aria-label={`Remove file ${index + 1}`}
                  onClick={() => removeRow(row.id)}
                >
                  <Trash2 className="size-3.5" aria-hidden="true" />
                </Button>
              ) : null}
            </div>
            <Textarea
              rows={isSkillMd ? 10 : 5}
              className="font-mono text-xs"
              value={row.content}
              placeholder={isSkillMd ? undefined : 'File contents'}
              onChange={(e) => {
                updateRow(row.id, { content: e.target.value })
                if (isSkillMd) onSkillMdEdit?.()
              }}
              aria-label={isSkillMd ? 'SKILL.md content' : `File ${index + 1} content`}
            />
            {error ? <p className="text-xs text-destructive">{error}</p> : null}
          </div>
        )
      })}
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="w-fit"
        onClick={() => onChange([...rows, newSkillFileRow()])}
      >
        <Plus className="size-3.5" aria-hidden="true" />
        Add file
      </Button>
    </div>
  )
}
