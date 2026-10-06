import type { ReactNode } from 'react'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { cn } from '@/lib/utils'

/** A segmented control on the shared ToggleGroup. Clicking the selected
 * option keeps it; `value` null shows none selected (a choice made
 * elsewhere). An option with an icon shows only the icon, named by `label`. */
export function SegmentedControl<T extends string>({
  label,
  options,
  value,
  onChange,
  className,
}: {
  label: string
  options: { value: T; label: string; icon?: ReactNode }[]
  value: T | null
  onChange: (v: T) => void
  className?: string
}) {
  return (
    <ToggleGroup
      aria-label={label}
      value={value ? [value] : []}
      onValueChange={(v) => v[0] && onChange(v[0] as T)}
      className={cn('gap-0 rounded-r-4 border border-border bg-card p-0.5', className)}
    >
      {options.map((o) => (
        <ToggleGroupItem
          key={o.value}
          value={o.value}
          aria-label={o.icon ? o.label : undefined}
          title={o.icon ? o.label : undefined}
          className="h-8 rounded-r-3 px-3 text-[13px] text-text-muted transition-colors hover:bg-transparent hover:text-foreground aria-pressed:bg-primary aria-pressed:text-primary-foreground"
        >
          {o.icon ?? o.label}
        </ToggleGroupItem>
      ))}
    </ToggleGroup>
  )
}
