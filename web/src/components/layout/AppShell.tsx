import { useState } from 'react'
import { Outlet } from 'react-router-dom'
import { ApiKeySessionBanner } from '@/components/app/ApiKeySessionBanner'
import { Sidebar } from '@/components/layout/Sidebar'
import { TopBar } from '@/components/layout/TopBar'
import { Sheet, SheetContent, SheetTitle } from '@/components/ui/sheet'
import { cn } from '@/lib/utils'

/**
 * Authenticated app shell.
 *
 * >= 1024px (Tailwind `lg`): fixed 244px sidebar, collapsible to a 64px icon
 * rail. Below 1024px: the fixed sidebar is hidden entirely and replaced by
 * an off-canvas Sheet drawer, opened from the top bar's menu button.
 */
export function AppShell() {
  const [collapsed, setCollapsed] = useState(false)
  const [mobileNavOpen, setMobileNavOpen] = useState(false)

  return (
    <div className="flex h-svh w-full bg-background text-foreground">
      <aside
        className={cn(
          'hidden shrink-0 border-r border-sidebar-border transition-[width] duration-150 lg:flex lg:flex-col',
          collapsed ? 'w-16' : 'w-[244px]',
        )}
      >
        <Sidebar collapsed={collapsed} />
      </aside>

      <Sheet open={mobileNavOpen} onOpenChange={setMobileNavOpen}>
        <SheetContent
          side="left"
          className="w-72 border-sidebar-border bg-sidebar p-0 text-sidebar-foreground"
        >
          <SheetTitle className="sr-only">Navigation</SheetTitle>
          <Sidebar onNavigate={() => setMobileNavOpen(false)} />
        </SheetContent>
      </Sheet>

      <div className="flex min-w-0 flex-1 flex-col">
        <TopBar
          collapsed={collapsed}
          onToggleCollapsed={() => setCollapsed((value) => !value)}
          onOpenMobileNav={() => setMobileNavOpen(true)}
        />
        <ApiKeySessionBanner />
        <main className="flex-1 overflow-y-auto p-[var(--space-5)] md:p-[var(--space-7)]">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
