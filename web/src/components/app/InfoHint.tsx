import { useState, type ReactNode } from 'react'
import { ArrowRight, Info } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Separator } from '@/components/ui/separator'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { cn } from '@/lib/utils'

interface InfoHintProps {
  /** Accessible name for the info button, e.g. "About LLM Usage". */
  label: string
  /** Heading of the popover and of the panel. */
  title: string
  /** The short version: a sentence or two, shown in the popover. */
  children: ReactNode
  /**
   * The full version, shown in a side panel beneath the short text. When
   * given, the popover offers "View full details" to open it.
   */
  details?: ReactNode
  /** Extra classes for the info button. */
  triggerClassName?: string
  side?: 'top' | 'right' | 'bottom' | 'left'
  align?: 'start' | 'center' | 'end'
}

const proseClassName =
  'text-sm leading-relaxed text-muted-foreground [&_a]:text-primary [&_a]:underline-offset-4 hover:[&_a]:underline'

/**
 * Contextual help beside a heading: an info button that explains what the
 * thing next to it shows. It has two modes. Short — hovering, focusing or
 * clicking the button opens a small popover with a sentence or two. Full —
 * "View full details" in that popover opens a side panel with the long-form
 * explanation. Without `details` there is only the short mode.
 *
 * Place it in a `flex items-center gap-1` row with the heading.
 */
export function InfoHint({
  label,
  title,
  children,
  details,
  triggerClassName,
  side = 'bottom',
  align = 'start',
}: InfoHintProps) {
  const [popoverOpen, setPopoverOpen] = useState(false)
  const [panelOpen, setPanelOpen] = useState(false)
  const hasDetails = details !== undefined

  return (
    <>
      <Popover open={popoverOpen} onOpenChange={setPopoverOpen}>
        <PopoverTrigger
          openOnHover
          delay={150}
          closeDelay={150}
          render={
            <Button
              type="button"
              variant="ghost"
              size="icon-xs"
              aria-label={label}
              className={cn(
                'shrink-0 text-text-subtle hover:text-foreground',
                triggerClassName,
              )}
            />
          }
        >
          <Info className="size-3.5" aria-hidden="true" />
        </PopoverTrigger>
        <PopoverContent side={side} align={align} className="w-80 gap-3 p-3.5">
          <div className="flex items-center gap-2.5">
            <span className="flex size-8 shrink-0 items-center justify-center rounded-full bg-primary/10 text-primary">
              <Info className="size-4" aria-hidden="true" />
            </span>
            <p className="text-sm font-semibold text-foreground">{title}</p>
          </div>
          <div className={cn(proseClassName, '[&_p+p]:mt-2')}>{children}</div>
          {hasDetails ? (
            <>
              <Separator />
              <Button
                variant="ghost"
                size="sm"
                className="self-start px-1 text-primary hover:text-primary"
                onClick={() => {
                  setPopoverOpen(false)
                  setPanelOpen(true)
                }}
              >
                View full details
                <ArrowRight className="size-4" aria-hidden="true" />
              </Button>
            </>
          ) : null}
        </PopoverContent>
      </Popover>

      {hasDetails ? (
        <Sheet open={panelOpen} onOpenChange={setPanelOpen}>
          <SheetContent className="flex w-full flex-col gap-0 p-0 data-[side=right]:sm:max-w-md">
            <SheetHeader className="gap-2 border-b border-border">
              <SheetTitle>{title}</SheetTitle>
              <SheetDescription className="sr-only">{label}</SheetDescription>
            </SheetHeader>
            <div
              className={cn(
                proseClassName,
                'min-h-0 flex-1 overflow-y-auto p-4 [&_p+p]:mt-3',
              )}
            >
              {children}
              <div className="mt-4">{details}</div>
            </div>
          </SheetContent>
        </Sheet>
      ) : null}
    </>
  )
}
