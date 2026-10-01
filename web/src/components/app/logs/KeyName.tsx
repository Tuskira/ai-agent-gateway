import { useApiKeys } from '@/lib/queries'

/** "name (gk_xxxxxxxx)" for a call's key id, raw id in the tooltip. Falls
 * back to the raw id while the key list loads, for deleted keys, and for
 * non-admins (the list 403s → null). */
export function KeyName({ keyId }: { keyId: string }) {
  const { data } = useApiKeys()
  if (!keyId) return <span className="text-text-subtle">—</span>
  const key = data?.items.find((k) => k.id === keyId)
  return (
    <span className="font-mono text-xs break-all" title={keyId}>
      {key ? `${key.name} (${key.prefix})` : keyId}
    </span>
  )
}
