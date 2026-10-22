import { useState, type MouseEvent } from 'react'
import { ChevronDown } from 'lucide-react'
import { useAuth } from '@/auth/AuthContext'
import { useTenants } from '@/lib/queries'
import { cn } from '@/lib/utils'
import { CopyButton } from '@/components/app/CopyButton'
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from '@/components/ui/command'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'

/**
 * The top-bar tenant picker for platform sessions. A bordered box shows the
 * acting tenant's name, its full id with a copy button, and a chevron;
 * clicking it opens a searchable list of every tenant the gateway knows
 * (`GET /tenants`, platform.admin only), each with its full id, searchable
 * by name, slug or id. Choosing one calls `switchTenant`, which sets the
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
  const name = current ? current.name : actingTenantId ? 'Unknown tenant' : 'Select tenant'

  function pick(id: string) {
    if (id !== actingTenantId) switchTenant(id)
    setOpen(false)
  }

  // The trigger button covers the name and id; a click on the rest of the
  // box (the chevron, the padding) opens the list too. Clicks on a button
  // are left to that button (the trigger toggles itself, copy only copies).
  function onBoxClick(event: MouseEvent<HTMLDivElement>) {
    if ((event.target as HTMLElement).closest('button')) return
    setOpen((o) => !o)
  }

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <div
        onClick={onBoxClick}
        className={cn(
          'flex w-[340px] max-w-[60vw] cursor-pointer items-center gap-1 rounded-r-3 border border-border bg-background py-1 pr-2 pl-2.5 transition-colors hover:bg-muted',
          className,
        )}
      >
        <PopoverTrigger
          render={
            <button
              type="button"
              aria-label={`Acting tenant: ${name}. Switch tenant`}
              aria-haspopup="listbox"
              aria-expanded={open}
              data-testid="tenant-switcher"
              className="flex min-w-0 flex-col items-start text-left leading-tight outline-none focus-visible:ring-2 focus-visible:ring-ring"
            />
          }
        >
          <span className="max-w-full truncate text-[13px] font-bold text-foreground">
            {name}
          </span>
          {actingTenantId ? (
            <span
              data-testid="tenant-switcher-id"
              className="max-w-full truncate font-mono text-[11.5px] text-text-muted"
            >
              {actingTenantId}
            </span>
          ) : null}
        </PopoverTrigger>
        {actingTenantId ? (
          <CopyButton
            value={actingTenantId}
            label="Copy tenant ID"
            size="icon-xs"
            variant="ghost"
            className="mt-auto shrink-0"
          />
        ) : null}
        <ChevronDown
          className="ml-auto size-4 shrink-0 text-text-subtle"
          aria-hidden="true"
        />
      </div>
      <PopoverContent
        align="start"
        className="w-[min(340px,calc(100vw-1.5rem))] gap-0 p-0"
      >
        {/* cmdk names its input from this label (aria-labelledby). */}
        <Command label="Search tenants">
          <CommandInput placeholder="Switch tenant..." />
          <CommandList className="max-h-80">
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
                  className="py-2"
                >
                  <div className="flex min-w-0 flex-1 flex-col leading-tight">
                    <span className="truncate font-bold text-foreground">{t.name}</span>
                    <span className="truncate font-mono text-[11.5px] text-text-muted">
                      {t.id}
                    </span>
                  </div>
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
          <div
            data-testid="tenant-switcher-footer"
            className="border-t border-border px-3 py-2 text-[12px] text-text-muted"
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
