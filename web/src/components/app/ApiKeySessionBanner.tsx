import { useState } from 'react'
import { Link } from 'react-router-dom'
import { KeyRound, X } from 'lucide-react'
import { useAuth } from '@/auth/AuthContext'
import { Button } from '@/components/ui/button'

export const API_KEY_BANNER_DISMISSED_KEY = 'gateway.apiKeyBannerDismissed'

function isDismissed(): boolean {
  try {
    return window.sessionStorage.getItem(API_KEY_BANNER_DISMISSED_KEY) === '1'
  } catch {
    return false
  }
}

/** One-line nudge shown to an admin signed in with an API key: create a
 * personal login, because API-key sign-in is going away. Dismissal lasts
 * for the browser tab session. Never shown for user sessions. */
export function ApiKeySessionBanner() {
  const { principal, mode } = useAuth()
  const [dismissed, setDismissed] = useState(isDismissed)

  if (dismissed || mode !== 'api_key' || !principal?.roles.includes('admin')) return null

  function dismiss() {
    setDismissed(true)
    try {
      window.sessionStorage.setItem(API_KEY_BANNER_DISMISSED_KEY, '1')
    } catch {
      // best effort: the banner still hides for this render
    }
  }

  return (
    <div
      role="status"
      data-testid="api-key-banner"
      className="flex items-center gap-3 border-b border-primary/35 bg-primary/6 px-4 py-2 text-[13px] text-foreground"
    >
      <KeyRound className="size-4 shrink-0 text-text-link" aria-hidden="true" />
      <span className="flex-1">
        You're signed in with an API key. Create a personal login for yourself — API-key
        sign-in will be removed in a later release.{' '}
        <Link
          to="/users"
          className="font-medium text-text-link underline underline-offset-2 hover:opacity-80"
        >
          Go to Users
        </Link>
      </span>
      <Button
        type="button"
        variant="ghost"
        size="icon-sm"
        aria-label="Dismiss"
        title="Dismiss"
        className="text-text-subtle"
        onClick={dismiss}
      >
        <X className="size-4" aria-hidden="true" />
      </Button>
    </div>
  )
}
