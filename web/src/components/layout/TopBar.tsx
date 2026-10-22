import { useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import {
  ChevronRight,
  Copy,
  KeyRound,
  LogOut,
  Menu,
  PanelLeftClose,
  PanelLeftOpen,
} from 'lucide-react'
import { useAuth } from '@/auth/AuthContext'
import { findNavItem, OVERVIEW_PATH } from '@/lib/nav'
import { TenantSwitcher } from '@/components/layout/TenantSwitcher'
import { ThemeToggle } from '@/components/layout/ThemeToggle'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'

interface TopBarProps {
  collapsed: boolean
  onToggleCollapsed: () => void
  onOpenMobileNav: () => void
}

export function TopBar({ collapsed, onToggleCollapsed, onOpenMobileNav }: TopBarProps) {
  const { principal, signOut, isPlatformAdmin, actingTenantId } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const [copied, setCopied] = useState(false)

  const current = findNavItem(location.pathname)
  const pageLabel = current?.label ?? findNavItem(OVERVIEW_PATH)?.label ?? 'Overview'
  const sessionUser = principal?.kind === 'user' ? principal.user : undefined
  const accountName = sessionUser?.username ?? principal?.subject ?? ''
  const accountDetail = sessionUser
    ? `${sessionUser.role} · ${sessionUser.tenant}`
    : 'API key'
  const initials = (accountName || '?').slice(0, 2).toUpperCase()
  // The pill is for non-platform sessions, which see their one tenant;
  // platform sessions get the acting tenant's id from the switcher.
  const shownTenantId = principal ? (actingTenantId ?? principal.tenant_id) : ''

  async function copyTenantId() {
    if (!shownTenantId) return
    try {
      await navigator.clipboard.writeText(shownTenantId)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      // clipboard API unavailable — silently ignore, nothing to fall back to.
    }
  }

  return (
    <header className="flex h-[60px] shrink-0 items-center gap-2.5 border-b border-border bg-card px-3 sm:gap-3.5 sm:px-5">
      {/* < 1024px: opens the off-canvas Sheet drawer. */}
      <Button
        variant="ghost"
        size="icon"
        className="lg:hidden"
        onClick={onOpenMobileNav}
        aria-label="Open navigation"
      >
        <Menu className="size-[18px]" aria-hidden="true" />
      </Button>

      {/* >= 1024px only: the fixed sidebar's collapse-to-icons toggle. */}
      <Button
        variant="ghost"
        size="icon"
        className="hidden text-text-subtle lg:inline-flex"
        onClick={onToggleCollapsed}
        aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
      >
        {collapsed ? (
          <PanelLeftOpen className="size-[18px]" aria-hidden="true" />
        ) : (
          <PanelLeftClose className="size-[18px]" aria-hidden="true" />
        )}
      </Button>

      {/* Below md: collapses to just the current page. */}
      <nav
        aria-label="Breadcrumb"
        className="flex min-w-0 items-center gap-1.5 truncate text-[13px] whitespace-nowrap"
      >
        <span className="hidden text-text-subtle md:inline">Tenant</span>
        <ChevronRight
          className="hidden size-3.5 shrink-0 text-text-subtle md:inline"
          aria-hidden="true"
        />
        <span className="hidden font-semibold text-foreground md:inline">
          AI Agent Gateway
        </span>
        <ChevronRight
          className="hidden size-3.5 shrink-0 text-text-subtle md:inline"
          aria-hidden="true"
        />
        <span className="truncate font-medium text-foreground md:font-normal md:text-text-muted">
          {pageLabel}
        </span>
      </nav>

      <div className="ml-auto flex items-center gap-1.5 sm:gap-2.5">
        {/* Right cluster, left to right: tenant (switcher or static pill), theme, avatar. */}
        {isPlatformAdmin ? (
          <TenantSwitcher />
        ) : principal ? (
          <div className="hidden items-center gap-2.5 rounded-r-5 border border-border bg-card py-1 pr-1.5 pl-3 sm:flex">
            <span
              className="truncate font-mono text-[13px] text-foreground"
              data-testid="tenant-id-pill"
            >
              {shownTenantId}
            </span>
            <button
              type="button"
              onClick={copyTenantId}
              title={copied ? 'Copied' : 'Copy tenant ID'}
              aria-label="Copy tenant ID"
              className="flex size-[26px] shrink-0 items-center justify-center rounded-r-2 text-text-subtle transition-colors hover:bg-muted hover:text-foreground"
            >
              <Copy className="size-3.5" aria-hidden="true" />
            </button>
          </div>
        ) : null}

        <ThemeToggle />

        <DropdownMenu>
          <DropdownMenuTrigger
            render={
              <button
                type="button"
                className="rounded-full focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                aria-label="Account menu"
              />
            }
          >
            <Avatar>
              <AvatarFallback className="bg-primary text-xs font-semibold text-primary-foreground">
                {initials}
              </AvatarFallback>
            </Avatar>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-56">
            <DropdownMenuGroup>
              <DropdownMenuLabel className="flex flex-col gap-0.5">
                <span className="truncate text-sm font-medium text-foreground">
                  {accountName}
                </span>
                <span className="truncate text-xs font-normal text-muted-foreground">
                  {accountDetail}
                </span>
              </DropdownMenuLabel>
            </DropdownMenuGroup>
            <DropdownMenuSeparator />
            {sessionUser ? (
              <DropdownMenuItem onClick={() => navigate('/change-password')}>
                <KeyRound className="size-4" aria-hidden="true" />
                Change password
              </DropdownMenuItem>
            ) : null}
            <DropdownMenuItem
              onClick={() => {
                signOut()
                navigate('/login', { replace: true })
              }}
            >
              <LogOut className="size-4" aria-hidden="true" />
              Log out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </header>
  )
}
