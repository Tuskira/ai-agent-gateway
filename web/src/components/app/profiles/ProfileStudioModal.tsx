import { useState } from 'react'
import { ChevronDown, Pencil, RefreshCw, Search } from 'lucide-react'
import { useAuth } from '@/auth/AuthContext'
import { useProfileToolsManager } from '@/hooks/use-profile-tools-manager'
import { useProfileSkillsManager } from '@/hooks/use-profile-skills-manager'
import { useProfileInstructionsManager } from '@/hooks/use-profile-instructions'
import { useAllProfileTools, useConnectorsList, useProfilesList } from '@/lib/queries'
import { profileToolKey } from '@/lib/profiles'
import type { Profile } from '@/lib/profiles'
import { cn, shortId } from '@/lib/utils'
import { ProfileFormDialog } from '@/components/app/profiles/ProfileFormDialog'
import { ProfileSkillsTab } from '@/components/app/profiles/ProfileSkillsTab'
import { ProfileInstructionsTab } from '@/components/app/profiles/ProfileInstructionsTab'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'

interface ProfileStudioModalProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

function statusDotClass(status: string): string {
  if (status === 'healthy') return 'bg-status-resolved'
  if (status === 'unhealthy') return 'bg-sev-high'
  return 'bg-border-strong'
}

function statusLabel(status: string): string {
  return status.length > 0 ? status[0]!.toUpperCase() + status.slice(1) : status
}

function statusTextClass(status: string): string {
  if (status === 'healthy') return 'text-status-resolved'
  if (status === 'unhealthy') return 'text-sev-high'
  return 'text-text-subtle'
}

/** Tabs with no real content yet -- rendered as a "Coming in a later
 * release" placeholder. "Skills & commands" and "Instructions" used to be
 * in this list too; they're now real tabs backed by
 * `useProfileSkillsManager` / `useProfileInstructionsManager`. */
const PLACEHOLDER_TABS = [
  { id: 'models', label: 'Models' },
  { id: 'agents', label: 'Agents' },
]

/**
 * v8 "Profile Studio" full-screen modal: tenant profiles on the left,
 * the selected profile's tool grants in the center (built on the same
 * `useProfileToolsManager` hook as `/profiles/{id}/tools`, so the two never
 * drift), and the tenant's connectors on the right. OSS has no global
 * tenant, so the mock's "Standard profiles" / "Global connectors" sections
 * are replaced with a short note instead of being built out.
 *
 * Only mounts its body while open, same reasoning as `ConnectorFormDialog`:
 * no point firing profile/connector/tool queries (or requiring an
 * `AuthProvider` for `useAuth`) while the modal is closed.
 */
export function ProfileStudioModal({ open, onOpenChange }: ProfileStudioModalProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        showCloseButton
        className="flex h-[calc(100vh-2rem)] w-[calc(100vw-2rem)] max-w-none flex-col overflow-hidden p-0 sm:max-w-none"
      >
        {open ? <ProfileStudioContent /> : null}
      </DialogContent>
    </Dialog>
  )
}

function ProfileStudioContent() {
  const { principal } = useAuth()
  const profilesQuery = useProfilesList()
  const connectorsQuery = useConnectorsList()
  const profiles = profilesQuery.data?.items ?? []
  const toolsQueries = useAllProfileTools(profiles)
  const toolCountByProfile = new Map(
    profiles.map((p, i) => [p.id, toolsQueries[i]?.data?.total]),
  )

  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [editTarget, setEditTarget] = useState<Profile | null>(null)
  const activeId = selectedId ?? profiles[0]?.id ?? null

  return (
    <>
      <div className="flex items-center justify-between gap-3 border-b border-border px-6 py-4 pr-14">
        <div>
          <DialogTitle>Profile Studio</DialogTitle>
          <DialogDescription>
            Agent profiles for{' '}
            <span className="font-mono">
              {principal ? shortId(principal.tenant_id) : '—'}
            </span>
          </DialogDescription>
        </div>
        <Button
          variant="ghost"
          size="icon-sm"
          title="Reload"
          aria-label="Reload"
          onClick={() => {
            void profilesQuery.refetch()
            void connectorsQuery.refetch()
          }}
        >
          <RefreshCw className="size-4" aria-hidden="true" />
        </Button>
      </div>

      <div className="grid min-h-0 flex-1 grid-cols-1 lg:grid-cols-[280px_minmax(0,1fr)_280px]">
        <div className="flex flex-col gap-2.5 overflow-y-auto border-b border-border bg-bg-subtle p-4 lg:border-r lg:border-b-0">
          <div className="text-[11px] font-semibold tracking-[.08em] text-text-subtle uppercase">
            Tenant Profiles ({profiles.length})
          </div>
          {profilesQuery.isLoading ? (
            <>
              <Skeleton className="h-14 w-full" />
              <Skeleton className="h-14 w-full" />
            </>
          ) : profiles.length === 0 ? (
            <p className="py-6 text-center text-xs text-text-subtle">
              No profiles yet. Create one from the Profiles page.
            </p>
          ) : (
            profiles.map((p) => (
              <button
                key={p.id}
                type="button"
                onClick={() => setSelectedId(p.id)}
                className={cn(
                  'flex flex-col gap-1 rounded-r-4 border px-3.5 py-3 text-left',
                  p.id === activeId
                    ? 'border-ring bg-bg-elevated'
                    : 'border-border bg-bg-elevated hover:border-ring',
                )}
              >
                <div className="flex w-full items-center justify-between gap-2">
                  <span className="text-[13.5px] font-semibold text-foreground">
                    {p.name}
                  </span>
                  <span className="inline-flex h-[19px] shrink-0 items-center rounded-r-pill border border-border px-1.5 text-[10.5px] font-semibold text-text-subtle">
                    tenant
                  </span>
                </div>
                <span className="text-xs text-text-subtle">
                  {toolCountByProfile.get(p.id) ?? '—'} tools
                </span>
              </button>
            ))
          )}
          <p className="mt-1 text-xs text-text-subtle">
            Global profiles are not available in this edition.
          </p>
        </div>

        {activeId ? (
          <ProfileStudioBody
            key={activeId}
            profileId={activeId}
            onEditProfile={setEditTarget}
          />
        ) : (
          <div className="flex min-w-0 items-center justify-center px-6 py-5 text-sm text-text-subtle">
            Select a profile to manage its tools.
          </div>
        )}

        <div className="flex flex-col gap-2 overflow-y-auto border-t border-border bg-bg-subtle p-4 lg:border-t-0 lg:border-l">
          <div className="mb-0.5 text-[11px] font-semibold tracking-[.08em] text-text-subtle uppercase">
            Tenant MCPs ({connectorsQuery.data?.items.length ?? 0})
          </div>
          {connectorsQuery.isLoading ? (
            <>
              <Skeleton className="h-[42px] w-full" />
              <Skeleton className="h-[42px] w-full" />
            </>
          ) : (connectorsQuery.data?.items.length ?? 0) === 0 ? (
            <p className="py-6 text-center text-xs text-text-subtle">No MCPs yet.</p>
          ) : (
            connectorsQuery.data?.items.map((c) => (
              <div
                key={c.id}
                className="flex h-[42px] items-center justify-between gap-2 rounded-r-3 border border-border bg-bg-elevated px-3"
              >
                <span className="truncate font-mono text-[12.5px] text-foreground">
                  {c.name}
                </span>
                <span
                  className={cn(
                    'flex shrink-0 items-center gap-1.5 text-xs font-medium',
                    statusTextClass(c.status),
                  )}
                >
                  <span
                    className={cn('size-1.5 rounded-full', statusDotClass(c.status))}
                    aria-hidden="true"
                  />
                  {statusLabel(c.status)}
                </span>
              </div>
            ))
          )}
        </div>
      </div>
      <ProfileFormDialog
        open={editTarget !== null}
        onOpenChange={(open) => !open && setEditTarget(null)}
        profile={editTarget}
      />
    </>
  )
}

interface ProfileStudioBodyProps {
  profileId: string
  onEditProfile: (profile: Profile) => void
}

/** Center pane, keyed by profile id so switching the selected profile in
 * the left pane gives every manager hook below a fresh mount — same
 * "no effect-based reset" pattern as `ProfileToolsPage`. */
function ProfileStudioBody({ profileId, onEditProfile }: ProfileStudioBodyProps) {
  const {
    profileQuery,
    connectorGroups,
    filter,
    setFilter,
    selected,
    selectedList,
    toggleTool,
    handleSave,
    isSaving,
  } = useProfileToolsManager(profileId)
  const skillsManager = useProfileSkillsManager(profileId)
  const instructionsManager = useProfileInstructionsManager(profileId)
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())
  // Controlled so the footer below the tabs can show the right
  // count/Save/Cancel affordance for whichever tab is active -- each real
  // tab keeps its own draft state (its own manager hook) and its own save
  // action, same as the Tools tab always has.
  const [activeTab, setActiveTab] = useState('tools')

  function toggleCollapsed(connectorId: string) {
    setCollapsed((prev) => {
      const next = new Set(prev)
      if (next.has(connectorId)) next.delete(connectorId)
      else next.add(connectorId)
      return next
    })
  }

  const profile = profileQuery.data

  return (
    <div className="flex min-h-0 min-w-0 flex-col overflow-y-auto px-6 py-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <div className="text-[19px] font-bold text-foreground">
            {profile?.name ?? '—'}
          </div>
          <div className="mt-0.5 text-[12.5px] text-text-subtle">
            {selectedList.length} tools · 0 agents
          </div>
        </div>
        <Button
          variant="outline"
          size="sm"
          disabled={!profile}
          onClick={() => profile && onEditProfile(profile)}
        >
          <Pencil className="size-3.5" aria-hidden="true" />
          Edit profile
        </Button>
      </div>

      <Tabs
        value={activeTab}
        onValueChange={setActiveTab}
        className="mt-4 flex min-h-0 flex-1 flex-col gap-0"
      >
        <TabsList variant="line" className="h-auto justify-start border-b border-border">
          <TabsTrigger value="tools" className="text-[13px]">
            Tools
            <span className="rounded-r-pill bg-bg-muted px-1.5 py-px text-[11px] text-text-muted">
              {selectedList.length}
            </span>
          </TabsTrigger>
          <TabsTrigger value="models" className="text-[13px]">
            Models
          </TabsTrigger>
          <TabsTrigger value="skills-commands" className="text-[13px]">
            Skills & commands
            <span className="rounded-r-pill bg-bg-muted px-1.5 py-px text-[11px] text-text-muted">
              {skillsManager.selectedList.length}
            </span>
          </TabsTrigger>
          <TabsTrigger value="agents" className="text-[13px]">
            Agents
          </TabsTrigger>
          <TabsTrigger value="instructions" className="text-[13px]">
            Instructions
          </TabsTrigger>
        </TabsList>

        <TabsContent value="tools" className="flex flex-col gap-3 pt-4">
          <div className="flex justify-end">
            <label className="flex h-[34px] w-[260px] items-center gap-2 rounded-r-4 border border-border bg-bg-elevated px-2.5 text-text-subtle">
              <Search className="size-3.5 shrink-0" aria-hidden="true" />
              <Input
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
                placeholder="Filter tools…"
                className="h-auto border-0 bg-transparent p-0 text-[13px] shadow-none focus-visible:ring-0"
              />
            </label>
          </div>

          {connectorGroups.length === 0 ? (
            <div className="rounded-r-4 border border-dashed border-border-strong px-7 py-7 text-center text-[13px] text-text-subtle">
              No MCPs yet.
            </div>
          ) : (
            connectorGroups.map(({ connector, tools, isLoading }) => {
              const isCollapsed = collapsed.has(connector.id)
              return (
                <div
                  key={connector.id}
                  className="overflow-hidden rounded-r-4 border border-border"
                >
                  <button
                    type="button"
                    onClick={() => toggleCollapsed(connector.id)}
                    className="flex w-full items-center gap-2.5 border-b border-border bg-bg-subtle px-3.5 py-2.5 text-left last:border-b-0"
                  >
                    <ChevronDown
                      className={cn(
                        'size-3.5 shrink-0 text-text-subtle transition-transform',
                        isCollapsed && '-rotate-90',
                      )}
                      aria-hidden="true"
                    />
                    <span className="font-mono text-[13px] font-semibold text-foreground">
                      {connector.name}
                    </span>
                    <span
                      className={cn(
                        'flex items-center gap-1.5 text-xs font-medium',
                        statusTextClass(connector.status),
                      )}
                    >
                      <span
                        className={cn(
                          'size-1.5 rounded-full',
                          statusDotClass(connector.status),
                        )}
                        aria-hidden="true"
                      />
                      {statusLabel(connector.status)}
                    </span>
                    <span className="text-xs text-text-subtle">{tools.length} tools</span>
                  </button>
                  {isCollapsed ? null : isLoading ? (
                    <div className="p-3">
                      <Skeleton className="h-4 w-2/3" />
                    </div>
                  ) : tools.length === 0 ? (
                    <p className="px-3.5 py-3 text-xs text-text-subtle">
                      No tools match.
                    </p>
                  ) : (
                    <div className="grid grid-cols-1 gap-x-4 gap-y-0 px-2 py-1.5 sm:grid-cols-2">
                      {tools.map((tool) => {
                        const key = profileToolKey(connector.id, tool.tool_name)
                        const checked = selected.has(key)
                        return (
                          <label
                            key={key}
                            className="flex cursor-pointer items-center gap-2.5 rounded-r-2 px-1.5 py-1.5 text-sm hover:bg-bg-subtle"
                          >
                            <Checkbox
                              checked={checked}
                              onCheckedChange={() =>
                                toggleTool(
                                  connector.id,
                                  connector.name,
                                  tool.tool_name,
                                  tool.tool_namespace,
                                )
                              }
                            />
                            <span className="font-mono text-[12.5px] text-foreground">
                              {tool.tool_name}
                            </span>
                          </label>
                        )
                      })}
                    </div>
                  )}
                </div>
              )
            })
          )}
        </TabsContent>

        <TabsContent value="skills-commands">
          <ProfileSkillsTab manager={skillsManager} />
        </TabsContent>

        <TabsContent value="instructions">
          <ProfileInstructionsTab manager={instructionsManager} />
        </TabsContent>

        {PLACEHOLDER_TABS.map((t) => (
          <TabsContent key={t.id} value={t.id} className="pt-4">
            <p className="rounded-r-4 border border-dashed border-border-strong px-4 py-6 text-center text-[13px] text-text-subtle">
              Coming in a later release
            </p>
          </TabsContent>
        ))}
      </Tabs>

      <div className="mt-4 flex items-center justify-end gap-3 border-t border-border pt-3">
        {activeTab === 'tools' ? (
          <>
            <span className="text-xs text-text-subtle">
              {selectedList.length} tool{selectedList.length === 1 ? '' : 's'} granted
            </span>
            <Button onClick={handleSave} disabled={isSaving}>
              {isSaving ? 'Saving…' : 'Save'}
            </Button>
          </>
        ) : activeTab === 'skills-commands' ? (
          <>
            <span className="text-xs text-text-subtle">
              {skillsManager.selectedList.length} attached
            </span>
            <Button onClick={skillsManager.handleSave} disabled={skillsManager.isSaving}>
              {skillsManager.isSaving ? 'Saving…' : 'Save'}
            </Button>
          </>
        ) : activeTab === 'instructions' ? (
          <Button
            onClick={instructionsManager.handleSave}
            disabled={instructionsManager.isSaving || instructionsManager.overLimit}
          >
            {instructionsManager.isSaving ? 'Saving…' : 'Save'}
          </Button>
        ) : null}
      </div>
    </div>
  )
}
