import { useState, type ReactNode } from 'react'
import { Info, X } from 'lucide-react'
import { Button } from '@/components/ui/button'

interface DismissibleBannerProps {
  /** localStorage key the dismissal is remembered under — stays dismissed
   * across reloads, same page only. */
  storageKey: string
  children: ReactNode
}

function isDismissed(storageKey: string): boolean {
  try {
    return window.localStorage.getItem(storageKey) === '1'
  } catch {
    return false
  }
}

/** Small informational banner ("keys are shown once…", "values are
 * encrypted at rest…") that can be dismissed and stays dismissed for that
 * browser. Shared by the API Keys and Credentials list pages. */
export function DismissibleBanner({ storageKey, children }: DismissibleBannerProps) {
  const [dismissed, setDismissed] = useState(() => isDismissed(storageKey))

  if (dismissed) return null

  function handleDismiss() {
    setDismissed(true)
    try {
      window.localStorage.setItem(storageKey, '1')
    } catch {
      // best effort — banner still hides for this render
    }
  }

  return (
    <div className="flex items-center gap-3 rounded-r-4 border border-primary/35 bg-primary/6 px-3.5 py-3 text-[13px] text-foreground">
      <Info className="size-[18px] shrink-0 text-text-link" aria-hidden="true" />
      <span className="flex-1">{children}</span>
      <Button
        type="button"
        variant="ghost"
        size="icon-sm"
        aria-label="Dismiss"
        title="Dismiss"
        className="text-text-subtle"
        onClick={handleDismiss}
      >
        <X className="size-4" aria-hidden="true" />
      </Button>
    </div>
  )
}
