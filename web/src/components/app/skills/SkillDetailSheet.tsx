import { useState } from 'react'
import { Plus, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import {
  useCreateSkillVersion,
  useDeleteSkill,
  useSkill,
  useSkillVersions,
  useUpdateSkill,
} from '@/lib/queries'
import type { Skill } from '@/lib/skills'
import { formatDate } from '@/lib/utils'
import { SKILL_MD_PATH, validateFrontmatter, validateSkillFiles } from '@/lib/skill-validation'
import { filesToRows, rowsToFiles, type SkillFileRow } from './skill-files'
import { SkillFileEditor } from './SkillFileEditor'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ConfirmDialog } from '@/components/app/ConfirmDialog'
import { Skeleton } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'

interface SkillDetailSheetProps {
  /** The row clicked in the table, or `null` when the sheet is closed. */
  skill: Skill | null
  onOpenChange: (open: boolean) => void
}

/**
 * Outer shell: only mounts the body while a skill is selected, keyed by
 * its id, so switching skills always starts the body's local state
 * (version editor, delete confirmation) fresh -- same pattern as
 * `ModelFormDialog`.
 */
export function SkillDetailSheet({ skill, onOpenChange }: SkillDetailSheetProps) {
  return (
    <Sheet open={skill !== null} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="w-full sm:max-w-2xl">
        {skill ? (
          <SkillDetailBody
            key={skill.id}
            skill={skill}
            onDeleted={() => onOpenChange(false)}
          />
        ) : null}
      </SheetContent>
    </Sheet>
  )
}

interface SkillDetailBodyProps {
  skill: Skill
  onDeleted: () => void
}

function SkillDetailBody({ skill, onDeleted }: SkillDetailBodyProps) {
  const detailQuery = useSkill(skill.id)
  const versionsQuery = useSkillVersions(skill.id)
  const updateSkill = useUpdateSkill(skill.id)
  const deleteSkill = useDeleteSkill()
  const createVersion = useCreateSkillVersion(skill.id)

  const [versionEditorOpen, setVersionEditorOpen] = useState(false)
  const [versionRows, setVersionRows] = useState<SkillFileRow[]>([])
  const [versionFileErrors, setVersionFileErrors] = useState<(string | null)[]>([])
  const [deleteConfirmOpen, setDeleteConfirmOpen] = useState(false)

  const isPlatform = skill.scope === 'platform'
  const detail = detailQuery.data
  const noun = skill.kind === 'command' ? 'command' : 'skill'

  function openVersionEditor() {
    if (!detail) return
    setVersionRows(filesToRows(detail.latest.files))
    setVersionFileErrors([])
    setVersionEditorOpen(true)
  }

  function handleToggleEnabled(next: boolean) {
    updateSkill.mutate(
      { enabled: next },
      {
        onSuccess: () => toast.success(next ? 'Enabled' : 'Disabled'),
        onError: (err) => toast.error(err instanceof Error ? err.message : 'Failed to update'),
      },
    )
  }

  function handleDelete() {
    deleteSkill.mutate(skill.id, {
      onSuccess: () => {
        toast.success(noun === 'command' ? 'Command deleted' : 'Skill deleted')
        setDeleteConfirmOpen(false)
        onDeleted()
      },
      onError: (err) => toast.error(err instanceof Error ? err.message : 'Failed to delete'),
    })
  }

  function handleCreateVersion() {
    const files = rowsToFiles(versionRows)
    const filesValidation = validateSkillFiles(files)
    const skillMd = files.find((f) => f.path === SKILL_MD_PATH)
    const frontmatterErrors = skillMd ? validateFrontmatter(skillMd.content, skill.name) : []
    const errors = [...filesValidation.errors, ...frontmatterErrors]

    setVersionFileErrors(filesValidation.fileErrors)

    if (errors.length > 0 || filesValidation.fileErrors.some((e) => e !== null)) {
      toast.error(errors[0] ?? 'Fix the highlighted files before saving.')
      return
    }

    createVersion.mutate(files, {
      onSuccess: () => {
        toast.success('New version created')
        setVersionEditorOpen(false)
      },
      onError: (err) => toast.error(err instanceof Error ? err.message : 'Failed to create version'),
    })
  }

  return (
    <>
      <SheetHeader>
        <SheetTitle className="flex items-center gap-2 font-mono">
          {skill.name}
          {isPlatform ? <Badge variant="outline">Platform</Badge> : null}
        </SheetTitle>
        <SheetDescription className="capitalize">{skill.kind}</SheetDescription>
      </SheetHeader>

      {detailQuery.isLoading ? (
        <div className="flex flex-col gap-2 px-4">
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-2/3" />
        </div>
      ) : detailQuery.isError ? (
        <p className="px-4 text-sm text-destructive">Couldn&apos;t load this {noun}.</p>
      ) : detail ? (
        <div className="flex flex-1 flex-col gap-6 overflow-y-auto px-4 pb-4">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="flex flex-col gap-1">
              <span className="text-xs text-text-subtle">Enabled</span>
              <Switch
                checked={detail.enabled}
                disabled={isPlatform || updateSkill.isPending}
                onCheckedChange={handleToggleEnabled}
                aria-label="Enabled"
              />
            </div>
            <div className="text-right text-xs text-text-subtle">
              <div>Version v{detail.latest_version}</div>
              <div>Updated {formatDate(detail.updated_at)}</div>
            </div>
          </div>

          {detail.description ? (
            <div>
              <div className="mb-1 text-xs font-semibold text-text-subtle uppercase">
                Description
              </div>
              <p className="text-sm text-foreground">{detail.description}</p>
              <p className="mt-1 text-xs text-text-subtle">
                From the SKILL.md frontmatter -- edit it in a new version to change this.
              </p>
            </div>
          ) : null}

          {detail.kind === 'command' && detail.arguments.length > 0 ? (
            <div>
              <div className="mb-2 text-xs font-semibold text-text-subtle uppercase">
                Arguments
              </div>
              <ul className="flex flex-col gap-1.5">
                {detail.arguments.map((a) => (
                  <li key={a.name} className="flex flex-wrap items-center gap-2 text-xs">
                    <span className="font-mono font-semibold text-foreground">{a.name}</span>
                    {a.required ? (
                      <Badge variant="outline" className="text-[10px] uppercase">
                        required
                      </Badge>
                    ) : null}
                    {a.description ? (
                      <span className="text-text-muted">{a.description}</span>
                    ) : null}
                  </li>
                ))}
              </ul>
            </div>
          ) : null}

          <div>
            <div className="mb-2 flex items-center justify-between gap-2">
              <span className="text-xs font-semibold text-text-subtle uppercase">
                Latest files (v{detail.latest.version})
              </span>
              {!isPlatform && !versionEditorOpen ? (
                <Button type="button" variant="outline" size="sm" onClick={openVersionEditor}>
                  <Plus className="size-3.5" aria-hidden="true" />
                  New version
                </Button>
              ) : null}
            </div>
            <div className="flex flex-col gap-2">
              {detail.latest.files.map((f) => (
                <div key={f.path} className="rounded-r-3 border border-border p-2">
                  <div className="mb-1 font-mono text-xs font-semibold text-foreground">
                    {f.path}
                  </div>
                  <pre className="max-h-40 overflow-auto font-mono text-xs whitespace-pre-wrap text-text-muted">
                    {f.content}
                  </pre>
                </div>
              ))}
            </div>
          </div>

          {versionEditorOpen ? (
            <div className="flex flex-col gap-3 rounded-r-3 border border-border p-3">
              <div className="text-sm font-semibold text-foreground">New version</div>
              <SkillFileEditor
                rows={versionRows}
                fileErrors={versionFileErrors}
                onChange={setVersionRows}
              />
              <div className="flex justify-end gap-2">
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => setVersionEditorOpen(false)}
                >
                  Cancel
                </Button>
                <Button
                  type="button"
                  size="sm"
                  disabled={createVersion.isPending}
                  onClick={handleCreateVersion}
                >
                  {createVersion.isPending ? 'Saving…' : 'Save version'}
                </Button>
              </div>
            </div>
          ) : null}

          <div>
            <div className="mb-2 text-xs font-semibold text-text-subtle uppercase">Versions</div>
            {versionsQuery.isLoading ? (
              <Skeleton className="h-4 w-full" />
            ) : (versionsQuery.data?.items.length ?? 0) === 0 ? (
              <p className="text-xs text-text-subtle">No versions yet.</p>
            ) : (
              <ul className="flex flex-col gap-1 text-xs text-text-muted">
                {versionsQuery.data!.items.map((v) => (
                  <li key={v.version} className="flex items-center justify-between gap-2">
                    <span className="font-mono text-foreground">v{v.version}</span>
                    <span>
                      {v.file_count} file{v.file_count === 1 ? '' : 's'}
                    </span>
                    <span>{formatDate(v.created_at)}</span>
                  </li>
                ))}
              </ul>
            )}
          </div>

          {!isPlatform ? (
            <div className="border-t border-border pt-4">
              <Button
                type="button"
                variant="outline"
                className="text-sev-high hover:bg-sev-high-bg hover:text-sev-high"
                onClick={() => setDeleteConfirmOpen(true)}
              >
                <Trash2 className="size-3.5" aria-hidden="true" />
                Delete {noun}
              </Button>
            </div>
          ) : null}
        </div>
      ) : null}

      <ConfirmDialog
        open={deleteConfirmOpen}
        onOpenChange={setDeleteConfirmOpen}
        title={`Delete "${skill.name}"?`}
        description="Any profile this is attached to loses access to it immediately. This can't be undone."
        confirmLabel="Delete"
        destructive
        isLoading={deleteSkill.isPending}
        onConfirm={handleDelete}
      />
    </>
  )
}
