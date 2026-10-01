import { NavLink } from 'react-router-dom'
import tuskiraLogo from '@/assets/tuskira-logo.svg'
import { BrandWordmark } from '@/components/app/BrandWordmark'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { useAuth } from '@/auth/AuthContext'
import { navGroups } from '@/lib/nav'
import { cn } from '@/lib/utils'

interface SidebarProps {
  collapsed?: boolean
  onNavigate?: () => void
}

/** Coloured by the `--sidebar-*` tokens in src/index.css, so it follows the theme. */
export function Sidebar({ collapsed = false, onNavigate }: SidebarProps) {
  const { principal } = useAuth()
  const isAdmin = Boolean(principal?.roles.includes('admin'))
  const groups = navGroups
    .map((group) => ({
      ...group,
      items: group.items.filter((item) => !item.adminOnly || isAdmin),
    }))
    .filter((group) => group.items.length > 0)

  return (
    <div className="flex h-full flex-col bg-sidebar text-sidebar-foreground">
      <div
        className={cn(
          'flex h-[60px] shrink-0 items-center gap-3 border-b border-sidebar-border px-4',
          collapsed && 'justify-center px-2',
        )}
      >
        <span className="flex size-10 shrink-0 items-center justify-center rounded-r-5 bg-white shadow-1">
          <img src={tuskiraLogo} alt="Tuskira" className="size-[34px]" />
        </span>
        {!collapsed && (
          <div className="flex min-w-0 flex-col gap-0.5">
            <BrandWordmark
              label="Tuskira"
              className="h-[22px] w-auto self-start text-sidebar-foreground"
            />
            <span className="truncate text-[11px] leading-[14px] font-semibold tracking-[0.14em] text-sidebar-foreground/70 uppercase">
              AI Agent Gateway
            </span>
          </div>
        )}
      </div>

      <nav className="flex-1 overflow-y-auto px-2.5 py-3.5" aria-label="Primary">
        {groups.map((group) => (
          <div key={group.label} className="mb-3.5 flex flex-col gap-0.5 last:mb-0">
            {!collapsed && (
              <p className="px-2.5 pt-1 pb-1.5 text-[10.5px] font-semibold tracking-[0.1em] text-sidebar-foreground/60 uppercase">
                {group.label}
              </p>
            )}
            <ul className="flex flex-col gap-0.5">
              {group.items.map((item) => {
                const link = (
                  <NavLink
                    to={item.path}
                    end={item.path === '/'}
                    onClick={onNavigate}
                    aria-label={collapsed ? item.label : undefined}
                    className={({ isActive }) =>
                      cn(
                        'flex h-9 items-center gap-2.5 rounded-r-3 px-2.5 text-[13.5px] font-medium whitespace-nowrap transition-colors',
                        isActive
                          ? 'bg-sidebar-primary/10 text-sidebar-primary shadow-[inset_3px_0_0_var(--sidebar-primary)]'
                          : 'text-sidebar-foreground/70 hover:bg-sidebar-accent hover:text-sidebar-accent-foreground',
                        collapsed && 'justify-center px-0',
                      )
                    }
                  >
                    <item.icon className="size-[18px] shrink-0" aria-hidden="true" />
                    {!collapsed && <span className="truncate">{item.label}</span>}
                  </NavLink>
                )

                return (
                  <li key={item.path}>
                    {collapsed ? (
                      <Tooltip>
                        <TooltipTrigger render={link} />
                        <TooltipContent side="right" sideOffset={8}>
                          {item.label}
                        </TooltipContent>
                      </Tooltip>
                    ) : (
                      link
                    )}
                  </li>
                )
              })}
            </ul>
          </div>
        ))}
      </nav>

      <div
        className={cn(
          'flex shrink-0 items-center gap-2 border-t border-sidebar-border px-4 py-3 text-[11.5px] whitespace-nowrap text-sidebar-foreground/60',
          collapsed && 'justify-center px-2',
        )}
      >
        <span
          className="size-1.5 shrink-0 rounded-full bg-status-resolved"
          aria-hidden="true"
        />
        {!collapsed && (
          <span>
            v0.1 &middot; open source &middot;{' '}
            <a
              href="/licenses.txt"
              target="_blank"
              rel="noreferrer"
              className="underline-offset-2 hover:underline"
            >
              Licenses
            </a>
          </span>
        )}
      </div>
    </div>
  )
}
