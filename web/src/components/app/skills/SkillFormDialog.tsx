import { useState } from 'react'
import { zodResolver } from '@hookform/resolvers/zod'
import { useForm } from 'react-hook-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { useCreateSkill } from '@/lib/queries'
import type { CreateSkillInput, SkillKind } from '@/lib/skills'
import {
  SKILL_MD_PATH,
  commandArgumentError,
  findUndeclaredPlaceholders,
  parseFrontmatter,
  validateCommandArgumentCount,
  validateFrontmatter,
  validateSkillFiles,
  validateSkillName,
} from '@/lib/skill-validation'
import { defaultSkillMdRow, rowsToFiles, type SkillFileRow } from './skill-files'
import { newArgumentRow, rowsToArguments, type ArgumentRow } from './command-arguments'
import { SkillFileEditor } from './SkillFileEditor'
import { CommandArgumentsEditor } from './CommandArgumentsEditor'
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
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group'

const schema = z
  .object({
    name: z.string().trim().min(1, 'Name is required'),
  })
  .superRefine((values, ctx) => {
    const nameError = validateSkillName(values.name)
    if (nameError) {
      ctx.addIssue({ code: 'custom', path: ['name'], message: nameError })
    }
  })

type FormValues = z.infer<typeof schema>

interface SkillFormDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** Prefill the name, e.g. when registering a Discovered skill. */
  initialName?: string
}

/**
 * "Add skill / command" dialog. Name, kind, and files are only ever set
 * here -- once created, the name and kind are fixed, files change through
 * the detail sheet's "New version" editor, and enabled/arguments change
 * through it too. There is no separate description field: the registry
 * derives `description` from the submitted SKILL.md's frontmatter
 * `description` key and it stays read-only afterwards (see
 * `SkillDetailSheet`).
 */
export function SkillFormDialog({ open, onOpenChange, initialName = '' }: SkillFormDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[calc(100vh-3rem)] w-full flex-col overflow-hidden sm:max-w-3xl">
        {open ? (
          <SkillFormBody
            key={initialName}
            initialName={initialName}
            onDone={() => onOpenChange(false)}
            onCancel={() => onOpenChange(false)}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  )
}

interface SkillFormBodyProps {
  initialName: string
  onDone: () => void
  onCancel: () => void
}

function SkillFormBody({ initialName, onDone, onCancel }: SkillFormBodyProps) {
  const createSkill = useCreateSkill()

  const [kind, setKind] = useState<SkillKind>('skill')
  const [fileRows, setFileRows] = useState<SkillFileRow[]>(() => [
    defaultSkillMdRow(initialName, 'skill'),
  ])
  const [skillMdTouched, setSkillMdTouched] = useState(false)
  const [argumentRows, setArgumentRows] = useState<ArgumentRow[]>([])
  const [fileErrors, setFileErrors] = useState<(string | null)[]>([])
  const [argumentErrors, setArgumentErrors] = useState<(string | null)[]>([])

  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: { name: initialName },
  })

  /** While the person hasn't touched the SKILL.md content directly, keep
   * its frontmatter template in sync with the name/kind fields -- same
   * "auto-fill until edited" pattern as the connector form's slug. */
  function syncSkillMdTemplate(name: string, nextKind: SkillKind) {
    setFileRows((rows) => {
      const index = rows.findIndex((r) => r.path === SKILL_MD_PATH)
      if (index < 0) return rows
      const template = defaultSkillMdRow(name, nextKind)
      const next = [...rows]
      next[index] = { ...next[index]!, content: template.content }
      return next
    })
  }

  function handleKindChange(value: string) {
    const nextKind = value as SkillKind
    setKind(nextKind)
    if (!skillMdTouched) {
      syncSkillMdTemplate(form.getValues('name'), nextKind)
    }
    if (nextKind === 'skill') {
      setArgumentRows([])
      setArgumentErrors([])
    }
  }

  function onSubmit(values: FormValues) {
    const files = rowsToFiles(fileRows)
    const filesValidation = validateSkillFiles(files)
    const skillMd = files.find((f) => f.path === SKILL_MD_PATH)
    const frontmatterErrors = skillMd ? validateFrontmatter(skillMd.content, values.name) : []

    const args = kind === 'command' ? rowsToArguments(argumentRows) : []
    const argCountError = kind === 'command' ? validateCommandArgumentCount(args) : null
    const perArgErrors =
      kind === 'command' ? args.map((a, i) => commandArgumentError(a, i, args)) : []
    const undeclared =
      kind === 'command' && skillMd
        ? findUndeclaredPlaceholders(parseFrontmatter(skillMd.content)?.body ?? skillMd.content, args)
        : []

    setFileErrors(filesValidation.fileErrors)
    setArgumentErrors(perArgErrors)

    const blockingErrors = [
      ...filesValidation.errors,
      ...frontmatterErrors,
      ...(argCountError ? [argCountError] : []),
      ...undeclared.map((p) => `Placeholder "{{${p}}}" is not declared as an argument`),
    ]
    const hasRowErrors =
      filesValidation.fileErrors.some((e) => e !== null) || perArgErrors.some((e) => e !== null)

    if (blockingErrors.length > 0 || hasRowErrors) {
      toast.error(blockingErrors[0] ?? 'Fix the highlighted fields before saving.')
      return
    }

    const input: CreateSkillInput = {
      name: values.name,
      kind,
      files,
      arguments: kind === 'command' ? args : undefined,
    }

    createSkill.mutate(input, {
      onSuccess: () => {
        toast.success(kind === 'command' ? 'Command created' : 'Skill created')
        onDone()
      },
      onError: (err) => {
        toast.error(err instanceof Error ? err.message : 'Failed to save')
      },
    })
  }

  return (
    <Form {...form}>
      <form onSubmit={form.handleSubmit(onSubmit)} className="flex min-h-0 flex-1 flex-col">
        <DialogHeader>
          <DialogTitle>Add skill or command</DialogTitle>
          <DialogDescription>
            Text-only instructions an agent can load on demand (a skill) or run as a rendered
            prompt (a command). Visible only through a profile it's attached to.
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
                    <Input
                      placeholder="e.g. incident-runbook"
                      className="font-mono"
                      {...field}
                      onChange={(e) => {
                        field.onChange(e)
                        if (!skillMdTouched) {
                          syncSkillMdTemplate(e.target.value.trim(), kind)
                        }
                      }}
                    />
                  </FormControl>
                  <p className="text-xs text-text-subtle">
                    Lowercase letters, numbers, dot, underscore, dash. No &quot;__&quot;.
                  </p>
                  <FormMessage />
                </FormItem>
              )}
            />
            <div className="flex flex-col gap-1.5">
              <Label>Kind</Label>
              <RadioGroup
                value={kind}
                onValueChange={handleKindChange}
                className="flex h-9 items-center gap-4"
              >
                <label className="flex items-center gap-1.5 text-sm">
                  <RadioGroupItem value="skill" />
                  Skill
                </label>
                <label className="flex items-center gap-1.5 text-sm">
                  <RadioGroupItem value="command" />
                  Command
                </label>
              </RadioGroup>
            </div>
          </div>

          <div className="flex flex-col gap-3">
            <div>
              <div className="text-[14.5px] font-semibold text-foreground">Files</div>
              <p className="text-xs text-text-subtle">
                SKILL.md is required at the root; its body is{' '}
                {kind === 'command' ? 'the prompt template.' : 'the skill an agent loads.'} The
                frontmatter <code>description</code> becomes this skill&apos;s description.
              </p>
            </div>
            <SkillFileEditor
              rows={fileRows}
              fileErrors={fileErrors}
              onChange={setFileRows}
              onSkillMdEdit={() => setSkillMdTouched(true)}
            />
          </div>

          {kind === 'command' ? (
            <div className="flex flex-col gap-3">
              <div>
                <div className="text-[14.5px] font-semibold text-foreground">Arguments</div>
                <p className="text-xs text-text-subtle">
                  Every <code>{'{{placeholder}}'}</code> used in the SKILL.md body must be
                  declared here.
                </p>
              </div>
              {argumentRows.length === 0 ? (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="w-fit"
                  onClick={() => setArgumentRows([newArgumentRow()])}
                >
                  Add argument
                </Button>
              ) : (
                <CommandArgumentsEditor
                  rows={argumentRows}
                  rowErrors={argumentErrors}
                  onChange={setArgumentRows}
                />
              )}
            </div>
          ) : null}
        </div>

        <DialogFooter>
          <Button type="button" variant="outline" onClick={onCancel}>
            Cancel
          </Button>
          <Button type="submit" disabled={createSkill.isPending}>
            {createSkill.isPending ? 'Saving…' : 'Create'}
          </Button>
        </DialogFooter>
      </form>
    </Form>
  )
}
