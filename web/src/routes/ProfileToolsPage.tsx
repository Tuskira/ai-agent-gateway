import { Link, useNavigate, useParams } from 'react-router-dom'
import { ArrowLeft, Search, X } from 'lucide-react'
import { useProfileToolsManager } from '@/hooks/use-profile-tools-manager'
import { profileToolKey } from '@/lib/profiles'
import { CopyButton } from '@/components/app/CopyButton'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'

/**
 * Reads `:id` and keys the real page body by it, so navigating between two
 * different profiles' tool managers gives `ProfileToolsPageBody` a fresh
 * mount — its local selection state (see `useProfileToolsManager`) never
 * needs an effect-based reset for the "profile changed" case.
 */
export default function ProfileToolsPage() {
  const { id } = useParams<{ id: string }>()
  if (!id) return null
  return <ProfileToolsPageBody key={id} id={id} />
}

function ProfileToolsPageBody({ id }: { id: string }) {
  const navigate = useNavigate()
  const {
    profileQuery,
    connectorsQuery,
    connectorGroups,
    filter,
    setFilter,
    selected,
    selectedList,
    toggleTool,
    removeTool,
    handleSave,
    isSaving,
  } = useProfileToolsManager(id)

  return (
    <div className="flex h-full flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2.5">
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label="Back to profiles"
            nativeButton={false}
            render={<Link to="/profiles" />}
          >
            <ArrowLeft className="size-4" aria-hidden="true" />
          </Button>
          <div>
            <h1 className="text-[20px] leading-[1.25] font-bold tracking-[-0.02em] text-foreground">
              {profileQuery.data?.name ?? 'Profile tools'}
            </h1>
            <p className="font-mono text-[12px] text-text-subtle">
              {profileQuery.data?.slug}
            </p>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <div className="flex h-9 items-center gap-2 rounded-r-4 border border-border bg-card px-3 text-xs text-text-muted">
            <span>
              <code className="font-mono text-foreground">X-Agent-Profile-Name</code>:{' '}
              <span className="font-mono text-foreground">{profileQuery.data?.name}</span>
            </span>
            <CopyButton value={profileQuery.data?.name ?? ''} label="Copy profile name" />
          </div>
          <Button onClick={handleSave} disabled={isSaving}>
            {isSaving ? 'Saving…' : 'Save'}
          </Button>
        </div>
      </div>

      <div className="grid flex-1 grid-cols-1 gap-4 lg:grid-cols-2">
        <div className="flex flex-col gap-3 rounded-r-5 border border-border bg-card p-4">
          <label className="flex h-9 items-center gap-2 rounded-r-4 border border-border bg-background px-2.5 text-text-subtle">
            <Search className="size-4 shrink-0" aria-hidden="true" />
            <Input
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="Filter tools…"
              className="h-auto border-0 bg-transparent p-0 shadow-none focus-visible:ring-0"
            />
          </label>
          <div className="flex flex-col gap-3 overflow-y-auto">
            {connectorsQuery.isLoading ? (
              <>
                <Skeleton className="h-24 w-full" />
                <Skeleton className="h-24 w-full" />
              </>
            ) : connectorGroups.length === 0 ? (
              <p className="py-8 text-center text-xs text-text-subtle">
                No MCPs yet — add one first.
              </p>
            ) : (
              connectorGroups.map(({ connector, tools, isLoading }) => (
                <div
                  key={connector.id}
                  className="overflow-hidden rounded-r-4 border border-border"
                >
                  <div className="flex items-center gap-2 border-b border-border bg-bg-subtle px-3.5 py-2">
                    <span className="font-mono text-[13px] font-semibold text-foreground">
                      {connector.name}
                    </span>
                    <span className="text-[11px] text-text-subtle">
                      {tools.length} tools
                    </span>
                  </div>
                  <div className="flex flex-col divide-y divide-border">
                    {isLoading ? (
                      <div className="p-3">
                        <Skeleton className="h-4 w-2/3" />
                      </div>
                    ) : tools.length === 0 ? (
                      <p className="px-3.5 py-3 text-xs text-text-subtle">
                        No tools match.
                      </p>
                    ) : (
                      tools.map((tool) => {
                        const key = profileToolKey(connector.id, tool.tool_name)
                        const checked = selected.has(key)
                        return (
                          <label
                            key={key}
                            className="flex cursor-pointer items-center gap-2.5 px-3.5 py-2 text-sm hover:bg-bg-subtle"
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
                      })
                    )}
                  </div>
                </div>
              ))
            )}
          </div>
        </div>

        <div className="flex flex-col gap-3 rounded-r-5 border border-border bg-card p-4">
          <div className="text-sm font-semibold text-foreground">
            Selected tools ({selectedList.length})
          </div>
          <div className="flex flex-col gap-1.5 overflow-y-auto">
            {selectedList.length === 0 ? (
              <p className="py-8 text-center text-xs text-text-subtle">
                No tools selected yet. Check tools on the left to grant them.
              </p>
            ) : (
              selectedList.map(([key, t]) => (
                <div
                  key={key}
                  className="flex items-center justify-between gap-2 rounded-r-3 border border-border bg-bg-subtle px-3 py-2"
                >
                  <div className="min-w-0">
                    <div className="truncate font-mono text-[12.5px] text-foreground">
                      {t.toolName}
                    </div>
                    <div className="truncate text-[11px] text-text-subtle">
                      {t.connectorName}
                    </div>
                  </div>
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label={`Remove ${t.toolName}`}
                    onClick={() => removeTool(key)}
                  >
                    <X className="size-3.5" aria-hidden="true" />
                  </Button>
                </div>
              ))
            )}
          </div>
        </div>
      </div>
      {profileQuery.isError && !profileQuery.isLoading ? (
        <p className="text-xs text-sev-high">
          Couldn&apos;t load this profile.{' '}
          <button className="underline" onClick={() => navigate('/profiles')}>
            Back to profiles
          </button>
        </p>
      ) : null}
    </div>
  )
}
