import { TriangleAlert } from 'lucide-react'
import { CopyButton } from '@/components/app/CopyButton'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

export interface RevealedPassword {
  username: string
  password: string
}

interface RevealPasswordDialogProps {
  revealed: RevealedPassword | null
  onOpenChange: (open: boolean) => void
}

/** Shows a temporary password exactly once. The server never returns it again
 * and the UI keeps it only in this dialog's state. */
export function RevealPasswordDialog({
  revealed,
  onOpenChange,
}: RevealPasswordDialogProps) {
  return (
    <Dialog open={revealed !== null} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Temporary password</DialogTitle>
          <DialogDescription>
            Give this to <span className="font-medium">{revealed?.username}</span>. They
            must choose a new password at their next sign-in.
          </DialogDescription>
        </DialogHeader>
        <div className="flex flex-col gap-3 py-2">
          <div className="flex items-center gap-2 rounded-r-4 border border-border bg-bg-subtle px-3 py-2.5">
            <code
              data-testid="temporary-password"
              className="min-w-0 flex-1 truncate font-mono text-[13px] text-foreground select-all"
            >
              {revealed?.password}
            </code>
            <CopyButton value={revealed?.password ?? ''} label="Copy password" />
          </div>
          <div className="flex items-start gap-2 rounded-r-4 border border-sev-medium/40 bg-sev-medium-bg px-3 py-2.5 text-xs text-sev-medium-fg">
            <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
            <span>This password won&apos;t be shown again. Copy it now.</span>
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
