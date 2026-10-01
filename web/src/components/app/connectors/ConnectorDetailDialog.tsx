import { Check, RefreshCw, Search } from 'lucide-react'
import { toast } from 'sonner'
import {
  useConnectorHealth,
  useConnectorTools,
  useDiscoverConnectorTools,
} from '@/lib/queries'
import type { Connector, HeaderConfig } from '@/lib/connectors'
import { formatDate } from '@/lib/utils'
import { StatusPill } from '@/components/app/StatusPill'
import { GetStartedSnippets } from '@/components/app/connectors/GetStartedSnippets'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from '@/components/ui/dialog'
import { Skeleton } from '@/components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'

interface ConnectorDetailDialogProps {
  connector: Connector | null
  onOpenChange: (open: boolean) => void
  onEdit: (connector: Connector) => void
}

function statusTone(status: string): 'positive' | 'negative' | 'neutral' {
  if (status === 'healthy') return 'positive'
  if (status === 'unhealthy') return 'negative'
  return 'neutral'
}

function capabilityLabel(value: boolean | Record<string, unknown> | undefined): string {
  if (value === undefined || value === false) return 'Not supported'
  return 'Supported'
}

/** Read-only enabled/disabled badge for one server-initiated-request
 * policy field (sampling/elicitation/roots) — off by default, so an
 * absent value renders exactly like an explicit `false`. */
function ServerRequestBadge({ enabled }: { enabled: boolean }) {
  return (
    <Badge
      variant="outline"
      className={
        enabled
          ? 'border-status-resolved bg-status-resolved-bg text-status-resolved'
          : 'border-border-strong bg-bg-muted text-text-muted'
      }
    >
      {enabled ? 'Enabled' : 'Disabled'}
    </Badge>
  )
}

function headerSource(cfg: HeaderConfig): string {
  switch (cfg.type) {
    case 'static':
      return 'Static value'
    case 'token_field':
      return 'Token field'
    case 'incoming_field':
      return 'Incoming field'
    case 'external':
      return cfg.provider === 'secret_store'
        ? 'Secret store'
        : `External · ${cfg.provider || '—'}`
  }
}

function headerField(cfg: HeaderConfig): string {
  switch (cfg.type) {
    case 'static':
      return cfg.value
    case 'token_field':
      return cfg.field
    case 'incoming_field':
      return cfg.header
    case 'external':
      return Object.values(cfg.config)[0] ?? '—'
  }
}

/**
 * v8 connector detail modal: Details / Tools tabs plus a "Governance
 * setup" right rail. Replaces the earlier `ConnectorDetailDrawer` sheet.
 */
export function ConnectorDetailDialog({
  connector,
  onOpenChange,
  onEdit,
}: ConnectorDetailDialogProps) {
  const open = connector !== null
  const toolsQuery = useConnectorTools(connector?.id)
  const healthQuery = useConnectorHealth(connector?.id, open)
  const discoverTools = useDiscoverConnectorTools(connector?.id ?? '')

  function handleDiscover() {
    discoverTools.mutate(undefined, {
      onSuccess: (page) => toast.success(`Discovered ${page.total} tools`),
      onError: (err) =>
        toast.error(err instanceof Error ? err.message : 'Failed to discover tools'),
    })
  }

  const headerEntries = Object.entries(connector?.metadata?.headers ?? {})

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        showCloseButton
        className="flex h-[calc(100vh-3rem)] w-full flex-col overflow-hidden p-0 sm:max-w-5xl"
      >
        <Tabs defaultValue="details" className="flex min-h-0 flex-1 flex-col gap-0">
          <div className="flex items-center justify-between gap-3 px-6 pt-5 pr-14">
            <div className="min-w-0">
              <DialogTitle className="font-mono text-lg">{connector?.name}</DialogTitle>
              <DialogDescription className="sr-only">
                MCP details for {connector?.name}
              </DialogDescription>
            </div>
            {connector ? (
              <Button size="sm" onClick={() => onEdit(connector)}>
                Edit
              </Button>
            ) : null}
          </div>

          <TabsList
            variant="line"
            className="h-auto justify-start rounded-none border-b border-border px-6 pt-2"
          >
            <TabsTrigger value="details" className="text-[13px]">
              Details
            </TabsTrigger>
            <TabsTrigger value="tools" className="text-[13px]">
              Tools
              {toolsQuery.data ? (
                <span className="rounded-r-pill bg-bg-muted px-1.5 py-px text-[11px] text-text-muted">
                  {toolsQuery.data.total}
                </span>
              ) : null}
            </TabsTrigger>
          </TabsList>

          <div className="grid min-h-0 flex-1 grid-cols-1 overflow-hidden lg:grid-cols-[minmax(0,1fr)_280px]">
            <div className="min-h-0 overflow-y-auto px-6 py-5">
              <TabsContent value="details" className="flex flex-col gap-6">
                <div>
                  <div className="mb-3 flex items-center justify-between gap-3">
                    <div className="text-[14.5px] font-semibold text-foreground">
                      MCP Information
                    </div>
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => healthQuery.refetch()}
                      disabled={healthQuery.isFetching}
                    >
                      <RefreshCw className="size-3.5" aria-hidden="true" />
                      {healthQuery.isFetching ? 'Checking…' : 'Check health'}
                    </Button>
                  </div>
                  <div className="grid grid-cols-2 gap-x-8 gap-y-3.5 sm:grid-cols-4">
                    <div>
                      <div className="text-[11.5px] text-text-subtle">Status</div>
                      <StatusPill
                        tone={statusTone(connector?.status ?? 'unknown')}
                        label={connector?.status ?? 'unknown'}
                        className="mt-1.5"
                      />
                    </div>
                    <div className="min-w-0">
                      <div className="text-[11.5px] text-text-subtle">Endpoint</div>
                      <div className="mt-1.5 font-mono text-xs break-all text-foreground">
                        {connector?.endpoint}
                      </div>
                    </div>
                    <div>
                      <div className="text-[11.5px] text-text-subtle">Timeout</div>
                      <div className="mt-1.5 text-[13px] text-foreground">
                        {connector?.timeout_ms}ms
                      </div>
                    </div>
                    <div>
                      <div className="text-[11.5px] text-text-subtle">Created</div>
                      <div className="mt-1.5 text-[13px] text-text-muted">
                        {formatDate(connector?.created_at)}
                      </div>
                    </div>
                    <div>
                      <div className="text-[11.5px] text-text-subtle">Updated</div>
                      <div className="mt-1.5 text-[13px] text-text-muted">
                        {formatDate(connector?.updated_at)}
                      </div>
                    </div>
                  </div>
                  {healthQuery.data ? (
                    <div className="mt-3 flex flex-col gap-1 rounded-r-4 border border-border bg-bg-subtle p-3 text-xs">
                      <span>
                        Status:{' '}
                        <span className="font-medium text-foreground">
                          {healthQuery.data.status}
                        </span>
                        {healthQuery.data.latency_ms !== undefined ? (
                          <>
                            {' '}
                            · Latency:{' '}
                            <span className="font-medium text-foreground">
                              {healthQuery.data.latency_ms}ms
                            </span>
                          </>
                        ) : null}
                      </span>
                      {healthQuery.data.error ? (
                        <span className="text-sev-high">{healthQuery.data.error}</span>
                      ) : null}
                    </div>
                  ) : healthQuery.data === null && !healthQuery.isFetching ? (
                    <p className="mt-3 text-xs text-text-subtle">
                      Health checks aren&apos;t available yet on this gateway.
                    </p>
                  ) : null}
                </div>

                <div>
                  <div className="mb-1 text-[14.5px] font-semibold text-foreground">
                    Server Capabilities
                  </div>
                  <div className="mb-2.5 text-[12.5px] text-text-subtle">
                    Protocol version: {connector?.capabilities?.protocol_version ?? '—'}
                  </div>
                  <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-4">
                    <div className="rounded-r-4 border border-border px-3 py-2.5">
                      <div className="text-[11.5px] text-text-subtle">Server</div>
                      <div className="text-[13px] font-medium text-foreground">
                        {connector?.capabilities?.server_info?.name
                          ? `${connector.capabilities.server_info.name}${
                              connector.capabilities.server_info.version
                                ? ` v${connector.capabilities.server_info.version}`
                                : ''
                            }`
                          : '—'}
                      </div>
                    </div>
                    <div className="rounded-r-4 border border-border px-3 py-2.5">
                      <div className="text-[11.5px] text-text-subtle">Tools</div>
                      <div className="text-[13px] text-text-muted">
                        {capabilityLabel(connector?.capabilities?.tools)}
                      </div>
                    </div>
                    <div className="rounded-r-4 border border-border px-3 py-2.5">
                      <div className="text-[11.5px] text-text-subtle">Resources</div>
                      <div className="text-[13px] text-text-muted">
                        {capabilityLabel(connector?.capabilities?.resources)}
                      </div>
                    </div>
                    <div className="rounded-r-4 border border-border px-3 py-2.5">
                      <div className="text-[11.5px] text-text-subtle">Prompts</div>
                      <div className="text-[13px] text-text-muted">
                        {capabilityLabel(connector?.capabilities?.prompts)}
                      </div>
                    </div>
                  </div>
                </div>

                <div>
                  <div className="mb-1 text-[14.5px] font-semibold text-foreground">
                    Server-initiated requests
                  </div>
                  <div className="mb-2.5 text-[12.5px] text-text-subtle">
                    Whether this MCP may ask the gateway to relay these requests
                    back to the agent. Off by default.
                  </div>
                  <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-4">
                    <div className="rounded-r-4 border border-border px-3 py-2.5">
                      <div className="text-[11.5px] text-text-subtle">Sampling</div>
                      <div className="mt-1.5">
                        <ServerRequestBadge
                          enabled={connector?.metadata?.server_requests?.sampling ?? false}
                        />
                      </div>
                    </div>
                    <div className="rounded-r-4 border border-border px-3 py-2.5">
                      <div className="text-[11.5px] text-text-subtle">Elicitation</div>
                      <div className="mt-1.5">
                        <ServerRequestBadge
                          enabled={
                            connector?.metadata?.server_requests?.elicitation ?? false
                          }
                        />
                      </div>
                    </div>
                    <div className="rounded-r-4 border border-border px-3 py-2.5">
                      <div className="text-[11.5px] text-text-subtle">Roots</div>
                      <div className="mt-1.5">
                        <ServerRequestBadge
                          enabled={connector?.metadata?.server_requests?.roots ?? false}
                        />
                      </div>
                    </div>
                  </div>
                </div>

                <div>
                  <div className="text-[14.5px] font-semibold text-foreground">
                    Authentication Headers
                  </div>
                  <div className="mb-2.5 text-[12.5px] text-text-subtle">
                    Headers sent with every request to the backend MCP server
                  </div>
                  {headerEntries.length === 0 ? (
                    <div className="rounded-r-4 border border-dashed border-border-strong px-4 py-4 text-center text-[12.5px] text-text-subtle">
                      No authentication headers configured.
                    </div>
                  ) : (
                    <div className="overflow-hidden rounded-r-4 border border-border">
                      {headerEntries.map(([name, cfg]) => (
                        <div
                          key={name}
                          className="grid grid-cols-3 gap-3 border-b border-border px-4 py-3 text-[13px] last:border-b-0"
                        >
                          <span className="font-mono text-[12.5px] font-semibold text-foreground">
                            {name}
                          </span>
                          <span className="text-text-muted">{headerSource(cfg)}</span>
                          <span className="font-mono text-[12.5px] text-text-link">
                            {headerField(cfg)}
                          </span>
                        </div>
                      ))}
                    </div>
                  )}
                </div>

                <GetStartedSnippets connectorSlug={connector?.slug ?? 'connector'} />
              </TabsContent>

              <TabsContent value="tools" className="flex flex-col gap-3">
                <div className="flex items-center justify-between">
                  <span className="text-[13px] text-text-muted">
                    {toolsQuery.data ? `${toolsQuery.data.total} tools discovered` : ''}
                  </span>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={handleDiscover}
                    disabled={discoverTools.isPending}
                  >
                    <Search className="size-3.5" aria-hidden="true" />
                    {discoverTools.isPending ? 'Discovering…' : 'Discover tools'}
                  </Button>
                </div>
                {toolsQuery.isLoading ? (
                  <div className="flex flex-col gap-1.5">
                    <Skeleton className="h-8 w-full" />
                    <Skeleton className="h-8 w-full" />
                    <Skeleton className="h-8 w-full" />
                  </div>
                ) : toolsQuery.data && toolsQuery.data.items.length > 0 ? (
                  <div className="overflow-hidden rounded-r-4 border border-border">
                    {toolsQuery.data.items.map((tool, i) => (
                      <div
                        key={`${tool.tool_name}-${i}`}
                        className="border-b border-border px-3.5 py-2 font-mono text-xs text-foreground last:border-b-0"
                      >
                        {tool.tool_name}
                      </div>
                    ))}
                  </div>
                ) : (
                  <p className="text-xs text-text-subtle">No tools cached yet.</p>
                )}
              </TabsContent>
            </div>

            <div className="flex min-h-0 flex-col gap-3.5 overflow-y-auto border-t border-border bg-bg-subtle px-5 py-5 lg:border-t-0 lg:border-l">
              <div>
                <div className="text-[14px] font-semibold text-foreground">
                  Governance setup
                </div>
                <div className="mt-0.5 text-xs text-text-subtle">1 of 3 set up</div>
                <div className="mt-2 h-1.5 overflow-hidden rounded-r-pill bg-bg-muted">
                  <div className="h-full w-1/3 rounded-r-pill bg-status-resolved" />
                </div>
              </div>
              <div className="flex flex-col gap-2">
                <div className="flex items-center gap-2.5 rounded-r-4 border border-border bg-bg-elevated p-3">
                  <span className="flex size-[22px] shrink-0 items-center justify-center rounded-full bg-status-resolved text-white">
                    <Check className="size-3" aria-hidden="true" />
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="text-[13px] font-semibold text-foreground">
                      Usage tracking
                    </div>
                    <div className="text-[11.5px] text-text-subtle">
                      Every call through this MCP is logged.
                    </div>
                  </div>
                </div>
                <div className="flex items-center gap-2.5 rounded-r-4 border border-border bg-bg-elevated p-3">
                  <span
                    className="size-[22px] shrink-0 rounded-full border-2 border-dashed border-border-strong"
                    aria-hidden="true"
                  />
                  <div className="min-w-0 flex-1">
                    <div className="text-[13px] font-semibold text-foreground">
                      Rate limits
                    </div>
                    <div className="text-[11.5px] text-text-subtle">
                      Cap requests per agent profile.
                    </div>
                  </div>
                  <Button
                    variant="outline"
                    size="sm"
                    className="shrink-0 text-text-link"
                    onClick={() =>
                      toast.message('Rate limits are coming in a later release.')
                    }
                  >
                    Set up
                  </Button>
                </div>
                <div className="flex items-center gap-2.5 rounded-r-4 border border-border bg-bg-elevated p-3">
                  <span
                    className="size-[22px] shrink-0 rounded-full border-2 border-dashed border-border-strong"
                    aria-hidden="true"
                  />
                  <div className="min-w-0 flex-1">
                    <div className="text-[13px] font-semibold text-foreground">
                      Policies
                    </div>
                    <div className="text-[11.5px] text-text-subtle">
                      Restrict which tools can run automatically.
                    </div>
                  </div>
                  <Button
                    variant="outline"
                    size="sm"
                    className="shrink-0 text-text-link"
                    onClick={() =>
                      toast.message('Policies are coming in a later release.')
                    }
                  >
                    Set up
                  </Button>
                </div>
              </div>
            </div>
          </div>
        </Tabs>
      </DialogContent>
    </Dialog>
  )
}
