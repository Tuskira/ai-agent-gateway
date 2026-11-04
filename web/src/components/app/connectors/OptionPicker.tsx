import { useRef, useState } from 'react'
import { ChevronDown } from 'lucide-react'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

export interface PickerOption {
  value: string
  label: string
}

interface OptionPickerProps {
  value: string
  onChange: (value: string) => void
  /** What the picker can offer. Empty while loading or when it failed. */
  options: PickerOption[]
  loading?: boolean
  /** Why the options are missing; the text box stays usable. */
  error?: string
  /** Accessible name of the text box and the list. */
  label: string
}

/** A text box with a searchable list of known values under it. The text box
 * IS the value, so typing an id the list lacks (or no list at all) works:
 * the list only filters on what is typed and fills the box when one is
 * chosen. */
export function OptionPicker({
  value,
  onChange,
  options,
  loading = false,
  error,
  label,
}: OptionPickerProps) {
  const [open, setOpen] = useState(false)
  const rootRef = useRef<HTMLDivElement>(null)

  const needle = value.trim().toLowerCase()
  const shown = options.filter(
    (o) =>
      o.value.toLowerCase().includes(needle) || o.label.toLowerCase().includes(needle),
  )
  const hasOptions = options.length > 0

  return (
    <div
      ref={rootRef}
      className="flex flex-col gap-1.5"
      onBlur={(e) => {
        if (!rootRef.current?.contains(e.relatedTarget)) setOpen(false)
      }}
      onKeyDown={(e) => {
        if (e.key === 'Escape' && open) {
          e.stopPropagation()
          setOpen(false)
        }
      }}
    >
      <div className="flex items-center gap-1.5">
        <Input
          aria-label={label}
          className="font-mono"
          value={value}
          onFocus={() => setOpen(true)}
          onChange={(e) => {
            onChange(e.target.value)
            setOpen(true)
          }}
        />
        <Button
          type="button"
          variant="outline"
          size="icon"
          aria-label={open ? `Hide ${label} options` : `Show ${label} options`}
          aria-expanded={open}
          disabled={!hasOptions}
          onClick={() => setOpen((o) => !o)}
        >
          <ChevronDown className="size-4" aria-hidden="true" />
        </Button>
      </div>

      {open && hasOptions ? (
        // Keep focus in the text box while an option is pressed, so the
        // blur above does not close the list before the click lands.
        <div
          role="listbox"
          aria-label={`${label} options`}
          className="max-h-44 overflow-auto rounded-r-3 border border-border bg-popover p-1 shadow-sm"
          onMouseDown={(e) => e.preventDefault()}
        >
          {shown.length === 0 ? (
            <p className="px-2 py-1.5 text-xs text-text-subtle">
              No match — what you typed is used as is.
            </p>
          ) : (
            shown.map((o) => (
              <button
                key={o.value}
                type="button"
                role="option"
                aria-selected={o.value === value}
                className={cn(
                  'flex w-full cursor-pointer flex-col rounded-r-2 px-2 py-1.5 text-left text-sm hover:bg-accent focus-visible:bg-accent',
                  o.value === value && 'bg-accent',
                )}
                onClick={() => {
                  onChange(o.value)
                  setOpen(false)
                }}
              >
                <span className="font-mono text-[12.5px] text-foreground">{o.value}</span>
                {o.label !== o.value ? (
                  <span className="text-xs text-text-subtle">{o.label}</span>
                ) : null}
              </button>
            ))
          )}
        </div>
      ) : null}

      {loading ? <p className="text-xs text-text-subtle">Loading options…</p> : null}
      {error ? (
        <p className="text-xs text-text-subtle">
          Couldn&apos;t list options ({error}). Type the value instead.
        </p>
      ) : null}
    </div>
  )
}
