import { useState } from 'react'
import { cn } from '@/lib/utils'
import { CopyButton } from '@/components/app/CopyButton'
import { buildGetStartedSnippets } from '@/components/app/connectors/snippets'

interface GetStartedSnippetsProps {
  connectorSlug: string
}

/** "Get started" tab switcher (cURL / Python / SDK / Claude Code / Codex /
 * Cursor) shown in the connector detail modal's Details tab. */
export function GetStartedSnippets({ connectorSlug }: GetStartedSnippetsProps) {
  const tabs = buildGetStartedSnippets(connectorSlug)
  const firstTab = tabs[0]!
  const [activeId, setActiveId] = useState(firstTab.id)
  const active = tabs.find((t) => t.id === activeId) ?? firstTab

  return (
    <div>
      <div className="mb-2.5 text-[14.5px] font-semibold text-foreground">
        Get started
      </div>
      <div className="overflow-hidden rounded-r-4 border border-border">
        <div className="flex flex-wrap gap-1 border-b border-border bg-bg-subtle p-1.5">
          {tabs.map((t) => (
            <button
              key={t.id}
              type="button"
              onClick={() => setActiveId(t.id)}
              className={cn(
                'h-7 rounded-r-2 px-2.5 text-[12.5px] font-medium',
                t.id === active.id
                  ? 'bg-bg-elevated text-foreground shadow-1'
                  : 'text-text-subtle hover:text-foreground',
              )}
            >
              {t.label}
            </button>
          ))}
        </div>
        <div className="relative">
          <pre className="overflow-x-auto bg-indigo-d-1 px-4 py-3.5 font-mono text-xs leading-relaxed text-slate-d-12">
            {active.code}
          </pre>
          <CopyButton
            value={active.code}
            label={`Copy ${active.label} snippet`}
            className="absolute top-2 right-2 border-white/15 bg-white/5 text-slate-d-11 hover:bg-white/10 hover:text-slate-d-12"
          />
        </div>
      </div>
    </div>
  )
}
