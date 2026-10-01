import { Trash2 } from 'lucide-react'
import { useHeaderProviders } from '@/lib/queries'
import { headerRowError, type HeaderRowState, type HeaderRowType } from './header-rows'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

const HEADER_TYPE_ITEMS: { value: HeaderRowType; label: string }[] = [
  { value: 'static', label: 'Static value' },
  { value: 'token_field', label: 'Token field' },
  { value: 'incoming_field', label: 'Incoming field' },
  { value: 'external', label: 'External provider' },
]

interface HeaderRowEditorProps {
  row: HeaderRowState
  onChange: (next: HeaderRowState) => void
  onRemove: () => void
}

export function HeaderRowEditor({ row, onChange, onRemove }: HeaderRowEditorProps) {
  const providersQuery = useHeaderProviders()
  const providers = providersQuery.data?.providers ?? []
  const selectedProvider = providers.find((p) => p.id === row.provider)
  const providerItems = providers.map((p) => ({ value: p.id, label: p.name }))
  const error = headerRowError(row)

  return (
    <div className="flex flex-col gap-3 rounded-r-4 border border-border bg-bg-subtle p-3.5">
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-[1.2fr_1fr]">
        <div className="flex flex-col gap-1.5">
          <Label className="text-xs text-text-muted">Header name</Label>
          <Input
            placeholder="X-API-KEY"
            className="font-mono"
            value={row.headerName}
            onChange={(e) => onChange({ ...row, headerName: e.target.value })}
          />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label className="text-xs text-text-muted">Type</Label>
          <Select
            items={HEADER_TYPE_ITEMS}
            value={row.type}
            onValueChange={(value) => onChange({ ...row, type: value as HeaderRowType })}
          >
            <SelectTrigger className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {HEADER_TYPE_ITEMS.map((item) => (
                <SelectItem key={item.value} value={item.value}>
                  {item.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </div>

      {row.type === 'static' ? (
        <div className="flex flex-col gap-1.5">
          <Label className="text-xs text-text-muted">Value</Label>
          <Input
            placeholder="value"
            className="font-mono"
            value={row.value}
            onChange={(e) => onChange({ ...row, value: e.target.value })}
          />
        </div>
      ) : null}

      {row.type === 'token_field' ? (
        <div className="flex flex-col gap-1.5">
          <Label className="text-xs text-text-muted">Token field</Label>
          <Input
            placeholder="e.g. access_token"
            className="font-mono"
            value={row.field}
            onChange={(e) => onChange({ ...row, field: e.target.value })}
          />
        </div>
      ) : null}

      {row.type === 'incoming_field' ? (
        <div className="flex flex-col gap-1.5">
          <Label className="text-xs text-text-muted">Incoming header name</Label>
          <Input
            placeholder="e.g. X-Request-Id"
            className="font-mono"
            value={row.incomingHeader}
            onChange={(e) => onChange({ ...row, incomingHeader: e.target.value })}
          />
        </div>
      ) : null}

      {row.type === 'external' ? (
        <div className="flex flex-col gap-3">
          <div className="flex flex-col gap-1.5">
            <Label className="text-xs text-text-muted">Provider</Label>
            <Select
              items={providerItems}
              value={row.provider || null}
              onValueChange={(value) =>
                onChange({ ...row, provider: value ?? '', config: {} })
              }
            >
              <SelectTrigger className="w-full">
                <SelectValue placeholder="Select a provider…" />
              </SelectTrigger>
              <SelectContent>
                {providerItems.map((item) => (
                  <SelectItem key={item.value} value={item.value}>
                    {item.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          {selectedProvider
            ? Object.entries(selectedProvider.config_schema.properties).map(
                ([key, prop]) => {
                  const required = selectedProvider.config_schema.required?.includes(key)
                  return (
                    <div key={key} className="flex flex-col gap-1.5">
                      <Label className="text-xs text-text-muted">
                        {prop.description ?? key}
                        {required ? ' *' : ''}
                      </Label>
                      <Input
                        className="font-mono"
                        value={row.config[key] ?? ''}
                        onChange={(e) =>
                          onChange({
                            ...row,
                            config: { ...row.config, [key]: e.target.value },
                          })
                        }
                      />
                    </div>
                  )
                },
              )
            : null}
        </div>
      ) : null}

      {error ? <p className="text-xs font-medium text-destructive">{error}</p> : null}

      <Button
        type="button"
        variant="ghost"
        size="sm"
        className="self-start text-sev-high hover:bg-sev-high-bg hover:text-sev-high"
        onClick={onRemove}
      >
        <Trash2 className="size-3.5" aria-hidden="true" />
        Remove header
      </Button>
    </div>
  )
}
