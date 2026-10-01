export { cn } from 'cn'

/** "Jan 5, 2026" — shared date formatting for admin-console tables. Returns
 * an em dash for anything missing or unparsable, never a fabricated date. */
export function formatDate(iso?: string | null): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleDateString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
  })
}

/** "Jan 5, 2026, 3:04 PM" — like `formatDate` with the time, for audit rows and
 * last-login columns. Returns an em dash for anything missing or unparsable. */
export function formatDateTime(iso?: string | null): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  })
}

/** Lowercase, dash-separated identifier derived from a display name — used
 * to auto-fill a connector's slug from its name until the person edits the
 * slug field themselves. */
export function slugify(value: string): string {
  return value
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
}

/** Shortens a long opaque id (tenant id, connector id, …) to `abcd1234…ef01`
 * for compact display in tables and headers. Returns the id unchanged when
 * it's already short. */
export function shortId(id: string, headLen = 8, tailLen = 6): string {
  if (id.length <= headLen + tailLen + 1) return id
  return `${id.slice(0, headLen)}…${id.slice(-tailLen)}`
}

/** The gateway's MCP endpoint, derived from the current origin. The admin
 * API serves on :8081 in dev; the MCP endpoint itself is served on :8080.
 * In production (single gateway binary) both are same-origin and this is a
 * no-op replace. */
export function getMcpUrl(): string {
  if (typeof window === 'undefined') return '/mcp'
  return `${window.location.origin.replace(':8081', ':8080')}/mcp`
}
