import { TriangleAlert } from 'lucide-react'
import type { ApiKeyCreated } from '@/lib/api-keys'
import { CopyButton } from '@/components/app/CopyButton'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'

interface RevealApiKeyDialogProps {
  apiKey: ApiKeyCreated | null
  onOpenChange: (open: boolean) => void
}

/** Shows a freshly created/rotated key exactly once. There is no way to
 * retrieve it again after this dialog closes. */
export function RevealApiKeyDialog({ apiKey, onOpenChange }: RevealApiKeyDialogProps) {
  return (
    <Dialog open={apiKey !== null} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{apiKey?.name}</DialogTitle>
          <DialogDescription>
            Copy this key now — it won&apos;t be shown again.
          </DialogDescription>
        </DialogHeader>
        <div className="flex flex-col gap-3 py-2">
          <div className="flex items-center gap-2 rounded-r-4 border border-border bg-bg-subtle px-3 py-2.5">
            <code className="min-w-0 flex-1 truncate font-mono text-[13px] text-foreground">
              {apiKey?.key}
            </code>
            <CopyButton value={apiKey?.key ?? ''} label="Copy key" />
          </div>
          <div className="flex items-start gap-2 rounded-r-4 border border-sev-medium/40 bg-sev-medium-bg px-3 py-2.5 text-xs text-sev-medium-fg">
            <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
            <span>This key won&apos;t be shown again. Store it somewhere safe.</span>
          </div>
        </div>
        <DialogFooter>
          <Button type="button" onClick={() => onOpenChange(false)}>
            Done
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
