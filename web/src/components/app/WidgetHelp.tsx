import type { LucideIcon } from 'lucide-react'
import { InfoHint } from '@/components/app/InfoHint'
import { cn } from '@/lib/utils'

/** Accent for a card's title and icon. */
export type HelpTone = 'info' | 'success' | 'warning' | 'danger' | 'brand' | 'muted'

const TONE_CLASS: Record<HelpTone, string> = {
  info: 'text-primary',
  success: 'text-status-resolved',
  warning: 'text-sev-medium',
  danger: 'text-sev-high',
  brand: 'text-brand-cyan-to',
  muted: 'text-text-muted',
}

/** One block of the long-form explanation, in reading order. */
export type HelpBlock =
  /** Highlighted lead-in. */
  | { kind: 'callout'; body: string }
  /** Small uppercase section heading. */
  | { kind: 'heading'; title: string }
  /** Bordered card with an optional icon + accent title, and a body. */
  | { kind: 'card'; title?: string; body: string; tone?: HelpTone; icon?: LucideIcon }

/** Help content for one widget: what the popover and the panel say. */
export interface HelpContent {
  /** The short version: one or two sentences. */
  short: string
  /** The full version. Omit when the short text is the whole story. */
  blocks?: HelpBlock[]
}

function Block({ block }: { block: HelpBlock }) {
  switch (block.kind) {
    case 'callout':
      return (
        <div className="rounded-r-4 border border-primary/40 bg-primary/10 p-3.5 text-sm text-foreground">
          {block.body}
        </div>
      )
    case 'heading':
      return (
        <p className="pt-1 text-xs font-bold tracking-wide text-primary uppercase">
          {block.title}
        </p>
      )
    case 'card': {
      const Icon = block.icon
      return (
        <div className="rounded-r-4 border border-border bg-muted/40 p-3.5">
          {block.title ? (
            <div
              className={cn(
                'mb-1.5 flex items-center gap-2 text-[11px] font-bold tracking-wide uppercase',
                TONE_CLASS[block.tone ?? 'info'],
              )}
            >
              {Icon ? <Icon className="size-4 shrink-0" aria-hidden="true" /> : null}
              {block.title}
            </div>
          ) : null}
          <p className="text-sm text-foreground/85">{block.body}</p>
        </div>
      )
    }
  }
}

interface WidgetHelpProps {
  /** The widget's title — heads the popover and the panel. */
  title: string
  content: HelpContent
  triggerClassName?: string
}

/**
 * Contextual help for a dashboard widget: the short summary in the hover
 * popover, and the block-by-block explanation in the side panel.
 */
export function WidgetHelp({ title, content, triggerClassName }: WidgetHelpProps) {
  const { short, blocks } = content
  return (
    <InfoHint
      label={`About ${title}`}
      title={title}
      triggerClassName={triggerClassName}
      details={
        blocks && blocks.length > 0 ? (
          <div className="flex flex-col gap-3">
            {blocks.map((block, i) => (
              <Block key={i} block={block} />
            ))}
          </div>
        ) : undefined
      }
    >
      <p>{short}</p>
    </InfoHint>
  )
}
