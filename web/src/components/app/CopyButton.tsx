import { useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { toast } from 'sonner'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'

interface CopyButtonProps {
  value: string
  label?: string
  className?: string
  size?: 'icon' | 'icon-sm' | 'icon-xs'
  variant?: 'outline' | 'ghost'
}

/** Small icon button that copies `value` to the clipboard, flashes a check
 * mark, and toasts — shared by every "copy the id / key / profile name"
 * affordance across the admin console. */
export function CopyButton({
  value,
  label = 'Copy',
  className,
  size = 'icon-sm',
  variant = 'outline',
}: CopyButtonProps) {
  const [copied, setCopied] = useState(false)

  async function handleCopy() {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      toast.success('Copied to clipboard')
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      toast.error('Could not copy to clipboard')
    }
  }

  return (
    <Button
      type="button"
      variant={variant}
      size={size}
      onClick={handleCopy}
      aria-label={label}
      title={label}
      className={cn('text-text-muted', className)}
    >
      {copied ? (
        <Check className="size-3.5 text-status-resolved" aria-hidden="true" />
      ) : (
        <Copy className="size-3.5" aria-hidden="true" />
      )}
    </Button>
  )
}
