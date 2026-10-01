import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  ApiError,
  apiFetch,
  clearStoredApiKey,
  getStoredApiKey,
  onPasswordChangeRequired,
  setCsrfToken,
  setStoredApiKey,
} from '@/lib/api'
import { login, logout, me } from '@/lib/auth-api'
import type { Principal } from '@/lib/types'

export const AUTH_ME_QUERY_KEY = ['auth', 'me'] as const

export type AuthMode = 'session' | 'api_key'

interface AuthContextValue {
  /** The signed-in identity: `kind` is `user` (cookie session) or `api_key`
   * (emergency fallback); `user` carries username, role and tenant slug. */
  principal: Principal | null
  mode: AuthMode
  isLoading: boolean
  /** True while the server (or `/auth/me`) requires a password change before
   * anything else may be used. */
  mustChangePassword: boolean
  /** Double-submit CSRF token of the current session; in memory only. */
  csrfToken: string | null
  /** Password login. Sets the `gw_session` cookie server-side. */
  signIn: (username: string, password: string, tenant?: string) => Promise<Principal>
  /** Emergency API-key login: validates with `/auth/me` and persists the key
   * in sessionStorage. Only offered when `/auth/config.api_key_login`. */
  signInWithKey: (apiKey: string) => Promise<Principal>
  /** Ends the session (calls `/auth/logout` for cookie sessions). */
  signOut: () => void
  /** Call after a successful password change to lift the forced-change gate. */
  passwordChanged: () => void
}

const AuthContext = createContext<AuthContextValue | undefined>(undefined)

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient()

  // Mode is React state — NOT re-read from sessionStorage on every render.
  // `apiKeyMode` is on only for the emergency API-key fallback; the default
  // is a cookie session, restored on page load through `/auth/me`. Nothing
  // secret is stored for the session mode.
  const [apiKeyMode, setApiKeyMode] = useState(() => getStoredApiKey() !== null)
  // `signedOut` gates `principal` in the same commit as the navigation that
  // follows a sign-out: `queryClient.clear()` notifies observers via a
  // microtask, so without this flag LoginPage's "already signed in" guard
  // could bounce straight back for one render.
  const [signedOut, setSignedOut] = useState(false)
  const [forcedChange, setForcedChange] = useState(false)

  const { data, isLoading } = useQuery({
    queryKey: AUTH_ME_QUERY_KEY,
    // Session probe: a 401 just means "not signed in", so no redirect. The
    // API-key path keeps its old behaviour (a bad stored key → back to /login).
    queryFn: () => (apiKeyMode ? apiFetch<Principal>('/auth/me') : me()),
    enabled: !signedOut,
    retry: false,
    staleTime: Infinity,
  })

  const principal = useMemo<Principal | null>(() => {
    if (signedOut || !data) return null
    // In session mode only a real user principal counts; anything else is a
    // stray response, not a login.
    if (!apiKeyMode && data.kind !== 'user') return null
    return data
  }, [data, signedOut, apiKeyMode])

  const csrfToken = !apiKeyMode ? (principal?.csrf_token ?? null) : null
  useEffect(() => {
    setCsrfToken(csrfToken)
  }, [csrfToken])

  // The API answered 403 password_change_required: lock the UI onto the
  // change-password page (RequireAuth does the redirect).
  useEffect(() => onPasswordChangeRequired(() => setForcedChange(true)), [])

  const signIn = useCallback(
    async (username: string, password: string, tenant?: string) => {
      const res = await login({ username, password, tenant })
      setCsrfToken(res.csrf_token)
      let fetched: Principal
      try {
        fetched = await me()
      } catch (err) {
        setCsrfToken(null)
        throw err
      }
      const next: Principal = {
        ...fetched,
        kind: 'user',
        user: fetched.user ?? res.user,
        roles: fetched.roles ?? [res.user.role],
        csrf_token: fetched.csrf_token ?? res.csrf_token,
        must_change_password: fetched.must_change_password ?? res.must_change_password,
      }
      setCsrfToken(next.csrf_token ?? null)
      queryClient.setQueryData(AUTH_ME_QUERY_KEY, next)
      setApiKeyMode(false)
      setSignedOut(false)
      setForcedChange(Boolean(next.must_change_password))
      return next
    },
    [queryClient],
  )

  const signInWithKey = useCallback(
    async (apiKey: string) => {
      const trimmed = apiKey.trim()
      // skipAuthRedirect: a bad key here should surface as an inline form
      // error, not trigger the global clear-and-redirect-to-login behavior.
      const fetched = await apiFetch<Principal>('/auth/me', {
        apiKey: trimmed,
        skipAuthRedirect: true,
      })
      // Note: a UI gate only; the API still honors each role's permissions.
      if (!fetched.roles.includes('admin')) {
        throw new ApiError(
          403,
          'forbidden',
          'This key can’t open the console. Sign in with an admin key.',
        )
      }
      setStoredApiKey(trimmed)
      setCsrfToken(null)
      queryClient.setQueryData(AUTH_ME_QUERY_KEY, fetched)
      setApiKeyMode(true)
      setSignedOut(false)
      setForcedChange(false)
      return fetched
    },
    [queryClient],
  )

  const signOut = useCallback(() => {
    if (!apiKeyMode) {
      // Fire before the CSRF token is dropped; failure is fine, the cookie
      // expires server-side anyway.
      logout().catch(() => {})
    }
    clearStoredApiKey()
    setCsrfToken(null)
    setApiKeyMode(false)
    setSignedOut(true)
    setForcedChange(false)
    // Drop every cached answer so the next person to sign in never sees this
    // one's data.
    queryClient.clear()
  }, [apiKeyMode, queryClient])

  const passwordChanged = useCallback(() => {
    setForcedChange(false)
    queryClient.setQueryData<Principal | undefined>(AUTH_ME_QUERY_KEY, (prev) =>
      prev ? { ...prev, must_change_password: false } : prev,
    )
  }, [queryClient])

  const value = useMemo<AuthContextValue>(
    () => ({
      principal,
      mode: apiKeyMode ? 'api_key' : 'session',
      isLoading: !signedOut && isLoading,
      mustChangePassword: forcedChange || Boolean(principal?.must_change_password),
      csrfToken,
      signIn,
      signInWithKey,
      signOut,
      passwordChanged,
    }),
    [
      principal,
      apiKeyMode,
      signedOut,
      isLoading,
      forcedChange,
      csrfToken,
      signIn,
      signInWithKey,
      signOut,
      passwordChanged,
    ],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) {
    throw new Error('useAuth must be used within an AuthProvider')
  }
  return ctx
}

/** Like `useAuth`, but `null` outside an AuthProvider instead of throwing:
 * for leaf components that only decide whether to show an admin control. */
export function useAuthOptional(): AuthContextValue | null {
  return useContext(AuthContext) ?? null
}
