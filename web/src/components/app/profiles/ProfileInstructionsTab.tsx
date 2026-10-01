import type { useProfileInstructionsManager } from '@/hooks/use-profile-instructions'
import { cn } from '@/lib/utils'
import { Textarea } from '@/components/ui/textarea'

interface ProfileInstructionsTabProps {
  manager: ReturnType<typeof useProfileInstructionsManager>
}

/**
 * Profile Studio's "Instructions" tab: a free-text prompt appended to
 * every session using this profile (on top of the skill/command index --
 * see `SKILLS-CONTRACT.md`, Phase 2's `initialize` section), capped at
 * 8 KiB. Saving PUTs the whole profile -- see `useProfileInstructionsManager`.
 */
export function ProfileInstructionsTab({ manager }: ProfileInstructionsTabProps) {
  const { value, setValue, byteLength, maxBytes, overLimit } = manager

  return (
    <div className="flex flex-col gap-2 pt-4">
      <Textarea
        value={value}
        onChange={(e) => setValue(e.target.value)}
        rows={16}
        placeholder="Extra instructions appended to every session using this profile…"
        className="font-mono text-xs"
        aria-label="Profile instructions"
      />
      <p
        className={cn(
          'text-right text-xs tabular-nums',
          overLimit ? 'text-destructive' : 'text-text-subtle',
        )}
      >
        {byteLength} / {maxBytes} bytes
      </p>
    </div>
  )
}
