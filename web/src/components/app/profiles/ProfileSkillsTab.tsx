import { Search } from 'lucide-react'
import type { useProfileSkillsManager } from '@/hooks/use-profile-skills-manager'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'

interface ProfileSkillsTabProps {
  manager: ReturnType<typeof useProfileSkillsManager>
}

/**
 * Profile Studio's "Skills & commands" tab: a searchable checkbox list of
 * every skill/command visible to the tenant (platform rows included), with
 * a version pin ("Latest" or a specific `v1..vN`) shown once a row is
 * checked. A pin behind the registry's current `latest_version` gets an
 * "Outdated" badge and a one-click "Update to latest" button that sets
 * the local pin back to "track latest" (`version: null`) -- like every
 * other edit here, that's a draft change; it still needs Save to PUT.
 * Saving PUTs the full attached set -- see `useProfileSkillsManager`.
 */
export function ProfileSkillsTab({ manager }: ProfileSkillsTabProps) {
  const { skillsQuery, filteredSkills, filter, setFilter, selected, toggleSkill, setSkillVersion } =
    manager

  return (
    <div className="flex flex-col gap-3 pt-4">
      <div className="flex justify-end">
        <label className="flex h-[34px] w-[260px] items-center gap-2 rounded-r-4 border border-border bg-bg-elevated px-2.5 text-text-subtle">
          <Search className="size-3.5 shrink-0" aria-hidden="true" />
          <Input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Filter skills & commands…"
            className="h-auto border-0 bg-transparent p-0 text-[13px] shadow-none focus-visible:ring-0"
          />
        </label>
      </div>

      {skillsQuery.isLoading ? (
        <>
          <Skeleton className="h-10 w-full" />
          <Skeleton className="h-10 w-full" />
        </>
      ) : filteredSkills.length === 0 ? (
        <div className="rounded-r-4 border border-dashed border-border-strong px-7 py-7 text-center text-[13px] text-text-subtle">
          No skills or commands yet.
        </div>
      ) : (
        <div className="flex flex-col divide-y divide-border rounded-r-4 border border-border">
          {filteredSkills.map((skill) => {
            const sel = selected.get(skill.id)
            const checked = sel !== undefined
            // A pin is "outdated" once a newer version than the one this
            // profile attached exists in the registry -- `skill` (from
            // the live tenant+platform list, not the seeded draft) always
            // carries the current `latest_version`, so this needs no
            // extra fetch. "Latest" pins (`sel.version === null`) track
            // the newest version automatically and are never outdated.
            const outdated = checked && sel.version !== null && sel.version < skill.latest_version
            const versionItems = [
              { value: 'latest', label: 'Latest' },
              ...Array.from({ length: skill.latest_version }, (_, i) => ({
                value: String(i + 1),
                label: `v${i + 1}`,
              })),
            ]
            return (
              <div key={skill.id} className="flex items-center gap-3 px-3.5 py-2.5">
                <Checkbox
                  checked={checked}
                  onCheckedChange={() => toggleSkill(skill.id)}
                  aria-label={`Grant ${skill.name}`}
                />
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-1.5">
                    <span className="font-mono text-[12.5px] font-semibold text-foreground">
                      {skill.name}
                    </span>
                    <Badge variant="outline" className="text-[10px] uppercase">
                      {skill.kind}
                    </Badge>
                    {skill.scope === 'platform' ? (
                      <Badge variant="outline" className="text-[10px] uppercase">
                        platform
                      </Badge>
                    ) : null}
                    {outdated ? (
                      <Badge
                        variant="outline"
                        className="border-sev-medium/40 text-[10px] text-sev-medium-fg uppercase"
                      >
                        Outdated
                      </Badge>
                    ) : null}
                  </div>
                  {skill.description ? (
                    <p className="truncate text-xs text-text-subtle">{skill.description}</p>
                  ) : null}
                </div>
                {outdated ? (
                  <Button
                    type="button"
                    variant="outline"
                    size="xs"
                    className="shrink-0"
                    onClick={() => setSkillVersion(skill.id, null)}
                  >
                    Update to latest
                  </Button>
                ) : null}
                {checked ? (
                  <Select
                    items={versionItems}
                    value={sel.version === null ? 'latest' : String(sel.version)}
                    onValueChange={(v) =>
                      setSkillVersion(skill.id, !v || v === 'latest' ? null : Number(v))
                    }
                  >
                    <SelectTrigger
                      size="sm"
                      aria-label={`${skill.name} version`}
                      className="w-28 shrink-0"
                    >
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {versionItems.map((item) => (
                        <SelectItem key={item.value} value={item.value}>
                          {item.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                ) : null}
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}
