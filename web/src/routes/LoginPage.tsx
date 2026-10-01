import { useState, type FormEvent } from 'react'
import { Link, Navigate, useLocation, useNavigate } from 'react-router-dom'
import { Building2, Eye, EyeOff, KeyRound, Loader2, Lock, User, X } from 'lucide-react'
import tuskiraLogo from '@/assets/tuskira-logo.svg'
import { useAuth } from '@/auth/AuthContext'
import { ApiError } from '@/lib/api'
import { useAuthConfig, useHealth } from '@/lib/queries'
import { BrandWordmark } from '@/components/app/BrandWordmark'
import { ShinyButton } from '@/components/app/ShinyButton'
import { ThemeToggle } from '@/components/layout/ThemeToggle'
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from '@/components/ui/input-group'
import { Label } from '@/components/ui/label'

interface LocationState {
  from?: { pathname: string }
}

const GENERIC_LOGIN_ERROR = 'Invalid username or password'
const NETWORK_ERROR = 'Could not reach the gateway. Check your connection and try again.'

export default function LoginPage() {
  const { principal, signIn, signInWithKey } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const { data: health } = useHealth()
  const configQuery = useAuthConfig()

  // Until the config call settles we don't know whether to show the tenant
  // field; if it fails (older gateway) fall back to the most general form.
  const config = configQuery.data
  const singleTenant = config?.single_tenant ?? false
  const apiKeyLogin = config?.api_key_login ?? false
  const defaultTenant = config?.default_tenant ?? ''
  // First run: a single-tenant install with no console user yet.
  const firstRun = config?.has_users === false

  // null = "not toggled yet": first run opens the API-key form first.
  const [useKey, setUseKey] = useState<boolean | null>(null)
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  // null = "not edited yet", so the field follows the config's default.
  const [tenantInput, setTenantInput] = useState<string | null>(null)
  const [apiKey, setApiKey] = useState('')
  const [showKey, setShowKey] = useState(false)
  const [showPassword, setShowPassword] = useState(false)
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const tenant = singleTenant ? defaultTenant : (tenantInput ?? defaultTenant)
  const keyMode = (useKey ?? firstRun) && apiKeyLogin

  // Already signed in — don't show the login form again.
  if (principal) {
    const from = (location.state as LocationState | null)?.from?.pathname ?? '/'
    return <Navigate to={from} replace />
  }

  function finish() {
    const from = (location.state as LocationState | null)?.from?.pathname ?? '/'
    navigate(from, { replace: true })
  }

  async function handlePasswordSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!username.trim() || !password) {
      setError('Enter your username and password.')
      return
    }
    setIsSubmitting(true)
    setError(null)
    try {
      await signIn(username.trim(), password, tenant.trim() || undefined)
      setPassword('')
      finish()
    } catch (err) {
      if (err instanceof ApiError && err.status === 429) {
        setError('Too many attempts. Wait a few minutes and try again.')
      } else if (err instanceof ApiError) {
        // Never say which part was wrong.
        setError(GENERIC_LOGIN_ERROR)
      } else {
        setError(NETWORK_ERROR)
      }
    } finally {
      setIsSubmitting(false)
    }
  }

  async function handleKeySubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!apiKey.trim()) {
      setError('Enter an API key.')
      return
    }
    setIsSubmitting(true)
    setError(null)
    try {
      await signInWithKey(apiKey)
      finish()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : NETWORK_ERROR)
    } finally {
      setIsSubmitting(false)
    }
  }

  function toggleKeyMode() {
    setUseKey(!keyMode)
    setError(null)
  }

  return (
    <div className="relative flex min-h-svh flex-col bg-bg-subtle px-4 py-8 sm:px-6">
      <div className="flex justify-end">
        <ThemeToggle />
      </div>

      <div className="rise-in flex flex-1 items-center justify-center py-8">
        <div className="flex w-full max-w-md flex-col gap-6 rounded-2xl border border-border bg-card px-6 py-8 shadow-3 sm:px-10">
          <div className="flex flex-col items-center gap-1.5">
            <div className="flex items-center gap-2">
              <img src={tuskiraLogo} alt="" aria-hidden="true" className="h-9 w-auto" />
              <BrandWordmark
                label="Tuskira"
                className="h-8 w-auto text-brand-deep dark:text-foreground"
              />
            </div>
            <span className="text-[11px] font-semibold tracking-[0.18em] text-text-subtle uppercase">
              AI Agent Gateway
            </span>
          </div>

          <h1 className="text-center text-xl font-medium tracking-tight text-foreground">
            Sign in to your Gateway
          </h1>

          {firstRun ? (
            <div
              role="status"
              data-testid="first-run-notice"
              className="rounded-r-3 border border-primary/35 bg-primary/6 px-3 py-2 text-sm text-foreground"
            >
              {apiKeyLogin ? (
                'No console users yet. Sign in with an admin API key to create one.'
              ) : (
                <>
                  No console users yet. Ask your operator to run{' '}
                  <code className="font-mono text-[13px]">gateway create-user</code>.
                </>
              )}
            </div>
          ) : null}

          {error ? (
            <div
              role="alert"
              className="flex items-start gap-2 rounded-r-3 border border-destructive/20 bg-destructive/10 px-3 py-2 text-sm text-destructive"
            >
              <span className="flex-1">{error}</span>
              <button
                type="button"
                onClick={() => setError(null)}
                aria-label="Dismiss"
                className="-my-0.5 -mr-1 shrink-0 rounded-r-2 p-0.5 text-destructive/70 outline-none transition-colors hover:text-destructive focus-visible:ring-2 focus-visible:ring-ring"
              >
                <X className="size-4" aria-hidden="true" />
              </button>
            </div>
          ) : null}

          {configQuery.isLoading ? (
            <div
              role="status"
              className="flex items-center justify-center gap-2 py-6 text-sm text-text-subtle"
            >
              <Loader2 className="size-4 animate-spin" aria-hidden="true" />
              Loading…
            </div>
          ) : keyMode ? (
            <>
              <form className="flex flex-col gap-4" onSubmit={handleKeySubmit} noValidate>
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor="api-key">API key</Label>
                  <InputGroup className="h-10">
                    <InputGroupAddon>
                      <KeyRound aria-hidden="true" />
                    </InputGroupAddon>
                    <InputGroupInput
                      id="api-key"
                      name="api-key"
                      type={showKey ? 'text' : 'password'}
                      autoComplete="off"
                      autoCapitalize="off"
                      autoCorrect="off"
                      spellCheck={false}
                      placeholder="gk_&bull;&bull;&bull;&bull;&bull;&bull;&bull;&bull;&bull;&bull;&bull;&bull;&bull;&bull;&bull;&bull;"
                      value={apiKey}
                      onChange={(event) => setApiKey(event.target.value)}
                      aria-invalid={error ? true : undefined}
                      className="h-full font-mono text-[13.5px]"
                    />
                    <InputGroupAddon align="inline-end">
                      <InputGroupButton
                        size="icon-xs"
                        aria-label={showKey ? 'Hide API key' : 'Show API key'}
                        aria-pressed={showKey}
                        onClick={() => setShowKey((v) => !v)}
                      >
                        {showKey ? (
                          <EyeOff aria-hidden="true" />
                        ) : (
                          <Eye aria-hidden="true" />
                        )}
                      </InputGroupButton>
                    </InputGroupAddon>
                  </InputGroup>
                </div>

                <ShinyButton
                  type="submit"
                  className="h-10 w-full"
                  disabled={isSubmitting}
                >
                  <span className="inline-flex items-center justify-center gap-2">
                    {isSubmitting ? (
                      <Loader2 className="size-4 animate-spin" aria-hidden="true" />
                    ) : null}
                    Sign in
                  </span>
                </ShinyButton>
              </form>
            </>
          ) : (
            <form
              className="flex flex-col gap-4"
              onSubmit={handlePasswordSubmit}
              noValidate
            >
              {singleTenant ? null : (
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor="tenant">Tenant</Label>
                  <InputGroup className="h-10">
                    <InputGroupAddon>
                      <Building2 aria-hidden="true" />
                    </InputGroupAddon>
                    <InputGroupInput
                      id="tenant"
                      name="tenant"
                      autoComplete="organization"
                      autoCapitalize="off"
                      autoCorrect="off"
                      spellCheck={false}
                      placeholder="your-org"
                      value={tenant}
                      onChange={(event) => setTenantInput(event.target.value)}
                      className="h-full"
                    />
                  </InputGroup>
                </div>
              )}
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="username">Username</Label>
                <InputGroup className="h-10">
                  <InputGroupAddon>
                    <User aria-hidden="true" />
                  </InputGroupAddon>
                  <InputGroupInput
                    id="username"
                    name="username"
                    autoComplete="username"
                    autoCapitalize="off"
                    autoCorrect="off"
                    spellCheck={false}
                    placeholder="jane.doe"
                    value={username}
                    onChange={(event) => setUsername(event.target.value)}
                    aria-invalid={error ? true : undefined}
                    className="h-full"
                  />
                </InputGroup>
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="password">Password</Label>
                <InputGroup className="h-10">
                  <InputGroupAddon>
                    <Lock aria-hidden="true" />
                  </InputGroupAddon>
                  <InputGroupInput
                    id="password"
                    name="password"
                    type={showPassword ? 'text' : 'password'}
                    autoComplete="current-password"
                    placeholder="••••••••••••"
                    value={password}
                    onChange={(event) => setPassword(event.target.value)}
                    aria-invalid={error ? true : undefined}
                    className="h-full"
                  />
                  <InputGroupAddon align="inline-end">
                    <InputGroupButton
                      size="icon-xs"
                      aria-label={showPassword ? 'Hide password' : 'Show password'}
                      aria-pressed={showPassword}
                      onClick={() => setShowPassword((v) => !v)}
                    >
                      {showPassword ? (
                        <EyeOff aria-hidden="true" />
                      ) : (
                        <Eye aria-hidden="true" />
                      )}
                    </InputGroupButton>
                  </InputGroupAddon>
                </InputGroup>
              </div>

              <ShinyButton type="submit" className="h-10 w-full" disabled={isSubmitting}>
                <span className="inline-flex items-center justify-center gap-2">
                  {isSubmitting ? (
                    <Loader2 className="size-4 animate-spin" aria-hidden="true" />
                  ) : null}
                  Sign in
                </span>
              </ShinyButton>
            </form>
          )}

          {apiKeyLogin ? (
            <button
              type="button"
              onClick={toggleKeyMode}
              className="self-center text-xs font-medium text-text-link underline underline-offset-2 hover:opacity-80"
            >
              {keyMode ? 'Back to username sign-in' : 'Use an API key (emergency access)'}
            </button>
          ) : null}

          <p className="text-xs leading-relaxed text-text-subtle">
            {keyMode
              ? 'Admin keys can manage the gateway. Lost your key? Rotate it from the CLI.'
              : 'Forgot your password? Ask a gateway admin to reset it.'}{' '}
            <Link
              to="/docs"
              className="font-medium text-text-link underline underline-offset-2 hover:opacity-80"
            >
              View docs
            </Link>
          </p>
        </div>
      </div>

      <div className="flex flex-col items-center gap-1 text-xs text-text-subtle">
        <p>
          {new Date().getFullYear()} Tuskira&reg; AI Agent Gateway &middot; open source &middot;
          v{health?.version ?? '0.1'}
        </p>
      </div>
    </div>
  )
}
