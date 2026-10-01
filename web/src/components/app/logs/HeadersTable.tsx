interface HeadersTableProps {
  headers: Record<string, string> | undefined
}

/** The exact marker the gateway stores for a masked header: "[masked]"
 * (LLM plane) or "****" (MCP plane). Credentials only — nothing to show. */
const isMasked = (v: string) => v === '[masked]' || v === '****'

/** Request/response headers as a compact key/value table — shared by the
 * Access Logs and LLM Logs detail drawers. Masked (credential) headers are
 * left out. */
export function HeadersTable({ headers }: HeadersTableProps) {
  const all = Object.entries(headers ?? {})
  const entries = all.filter(([, v]) => !isMasked(v))

  if (entries.length === 0) {
    return (
      <p className="text-xs text-text-subtle">
        {all.length > 0
          ? 'Only credential headers were sent (values hidden).'
          : 'No headers captured.'}
      </p>
    )
  }

  return (
    <div className="overflow-hidden rounded-r-4 border border-border">
      {entries.map(([key, value], i) => (
        <div
          key={key}
          className={
            'grid grid-cols-[minmax(0,140px)_1fr] gap-2 px-3 py-1.5 text-xs' +
            (i < entries.length - 1 ? ' border-b border-border' : '')
          }
        >
          <span className="truncate font-mono text-text-muted" title={key}>
            {key}
          </span>
          <span className="truncate font-mono text-foreground" title={value}>
            {value}
          </span>
        </div>
      ))}
    </div>
  )
}
