import { Input } from '@/components/ui/input'

interface ApiKeyInputProps {
  /** Accessible name, e.g. "Target 1 API key" or "Nebius API key". */
  'aria-label': string
  value: string
  onChange: (value: string) => void
  /** Shown under the field -- what happens to it on save. */
  helperText: string
  autoFocus?: boolean
}

/**
 * The raw-key entry field shared by the model form's "Accept API key"
 * option (`TargetRowEditor`) and the Model Catalog Connect dialog's "Enter
 * API key" mode (`ConnectProviderDialog`) -- see ADDENDUM 1 item 5 in
 * MODEL-CATALOG-CONTRACT.md ("the dialog shares the same key-entry
 * component as the model form"). The key itself is never sent as a model
 * or connect field: callers store it as (or replace) a credential and
 * reference it by name.
 */
export function ApiKeyInput({
  value,
  onChange,
  helperText,
  autoFocus,
  ...rest
}: ApiKeyInputProps) {
  return (
    <div className="flex flex-col gap-1.5">
      <Input
        type="password"
        autoComplete="off"
        autoFocus={autoFocus}
        placeholder="Paste the vendor's API key"
        className="font-mono"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        {...rest}
      />
      <p className="text-xs text-text-subtle">{helperText}</p>
    </div>
  )
}
