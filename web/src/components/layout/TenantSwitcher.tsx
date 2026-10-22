import { useState } from 'react'
import { Building2, ChevronsUpDown } from 'lucide-react'
import { useAuth } from '@/auth/AuthContext'
import { useTenants } from '@/lib/queries'
import { cn } from '@/lib/utils'
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from '@/components/ui/command'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'

/** First 8 characters of a tenant id: enough to tell tenants apart in a
 * list, short enough to sit next to the name. */
function shortTenantId(id: string): string {
  return id.slice(0, 8)
}

/**
 * The top-bar tenant picker for platform sessions. Lists every tenant the
 * gateway knows (`GET /tenants`, platform.admin only), searchable by name,
 * slug or id; choosing one calls `switchTenant`, which sets the
 * `X-Gateway-Acting-Tenant` header for every later call and drops the
 * cached data of the tenant being left. Rendered only when
 * `isPlatformAdmin`; for every other session TopBar keeps its static label.
 */
export function TenantSwitcher({ className }: { className?: string }) {
  const { isPlatformAdmin, actingTenantId, switchTenant } = useAuth()
  const [open, setOpen] = useState(false)
  const tenantsQuery = useTenants(isPlatformAdmin)

  const tenants = tenantsQuery.data?.items ?? []
  const total = tenantsQuery.data?.total ?? tenants.length
  const current = tenants.find((t) => t.id === actingTenantId)
  const label = current
    ? current.name
    : actingTenantId
      ? shortTenantId(actingTenantId)
      : 'Select tenant'

  function pick(id: string) {
    if (id !== actingTenantId) switchTenant(id)
    setOpen(false)
  }

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        render={
          <button
            type="button"
            aria-label={`Acting tenant: ${label}. Switch tenant`}
            aria-haspopup="listbox"
            aria-expanded={open}
            data-testid="tenant-switcher"
            className={cn(
              'inline-flex h-8 max-w-[220px] items-center gap-1.5 rounded-r-3 border border-border bg-background px-2 text-[13px] font-medium text-foreground transition-colors hover:bg-muted',
              className,
            )}
          />
        }
      >
        <Building2 className="size-3.5 shrink-0 text-text-subtle" aria-hidden="true" />
        <span className="truncate">{label}</span>
        <ChevronsUpDown
          className="size-3.5 shrink-0 text-text-subtle"
          aria-hidden="true"
        />
      </PopoverTrigger>
      <PopoverContent align="start" className="w-80 gap-0 p-0">
        {/* cmdk names its input from this label (aria-labelledby). */}
        <Command label="Search tenants">
          <CommandInput placeholder="Search by name or id…" />
          <CommandList>
            {tenantsQuery.isError ? (
              <p role="alert" className="px-3 py-4 text-center text-sm text-destructive">
                Couldn&apos;t load tenants.
              </p>
            ) : (
              <CommandEmpty>
                {tenantsQuery.isPending ? 'Loading…' : 'No tenant matches.'}
              </CommandEmpty>
            )}
            <CommandGroup>
              {tenants.map((t) => (
                <CommandItem
                  key={t.id}
                  value={t.id}
                  keywords={[t.name, t.slug]}
                  data-checked={t.id === actingTenantId ? 'true' : undefined}
                  onSelect={() => pick(t.id)}
                >
                  <div className="flex min-w-0 flex-1 items-baseline gap-2">
                    <span className="truncate font-medium text-foreground">{t.name}</span>
                    <span className="shrink-0 font-mono text-[11px] text-text-muted">
                      {shortTenantId(t.id)}
                    </span>
                  </div>
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
          <div
            data-testid="tenant-switcher-footer"
            className="border-t border-border px-3 py-1.5 text-[11.5px] text-text-muted"
          >
            {tenantsQuery.isSuccess
              ? `${total} ${total === 1 ? 'tenant' : 'tenants'}`
              : ''}
          </div>
        </Command>
      </PopoverContent>
    </Popover>
  )
}
