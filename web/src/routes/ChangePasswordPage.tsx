import { useMemo, useState } from 'react'
import { Link, Navigate, useNavigate } from 'react-router-dom'
import { zodResolver } from '@hookform/resolvers/zod'
import { useForm } from 'react-hook-form'
import { Loader2, LogOut, TriangleAlert } from 'lucide-react'
import { toast } from 'sonner'
import type { z } from 'zod'
import tuskiraLogo from '@/assets/tuskira-logo.svg'
import { useAuth } from '@/auth/AuthContext'
import { ApiError } from '@/lib/api'
import {
  buildPasswordSchema,
  PASSWORD_MAX_LENGTH,
  PASSWORD_MIN_LENGTH,
} from '@/lib/password-policy'
import { useChangePassword } from '@/lib/queries'
import { BrandWordmark } from '@/components/app/BrandWordmark'
import { ThemeToggle } from '@/components/layout/ThemeToggle'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'

type FormValues = z.infer<ReturnType<typeof buildPasswordSchema>>

/** Change your own password. Forced (no way out except logging out) while the
 * account still has a temporary password; otherwise reachable from the user
 * menu. */
export default function ChangePasswordPage() {
  const { principal, mode, mustChangePassword, passwordChanged, signOut } = useAuth()
  const navigate = useNavigate()
  const changePassword = useChangePassword()
  const [error, setError] = useState<string | null>(null)

  const username = principal?.user?.username ?? ''
  const schema = useMemo(() => buildPasswordSchema(username), [username])
  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    defaultValues: { current_password: '', new_password: '', confirm_password: '' },
  })

  // Passwords belong to cookie-session users; API keys have none.
  if (mode === 'api_key') return <Navigate to="/" replace />

  function onSubmit(values: FormValues) {
    setError(null)
    changePassword.mutate(
      {
        current_password: values.current_password,
        new_password: values.new_password,
      },
      {
        onSuccess: () => {
          toast.success('Password changed')
          form.reset()
          passwordChanged()
          navigate('/', { replace: true })
        },
        onError: (err) => {
          setError(
            err instanceof ApiError
              ? err.message
              : 'Could not reach the gateway. Check your connection and try again.',
          )
        },
      },
    )
  }

  return (
    <div className="relative flex min-h-svh flex-col bg-bg-subtle px-4 py-8 sm:px-6">
      <div className="flex justify-end">
        <ThemeToggle />
      </div>

      <div className="rise-in flex flex-1 items-center justify-center py-8">
        <div className="flex w-full max-w-md flex-col gap-6 rounded-2xl border border-border bg-card px-6 py-8 shadow-3 sm:px-10">
          <div className="flex items-center justify-center gap-2">
            <img src={tuskiraLogo} alt="" aria-hidden="true" className="h-9 w-auto" />
            <BrandWordmark
              label="Tuskira"
              className="h-8 w-auto text-brand-deep dark:text-foreground"
            />
          </div>

          <h1 className="text-center text-xl font-medium tracking-tight text-foreground">
            Change password
          </h1>

          {mustChangePassword ? (
            <div className="flex items-start gap-2 rounded-r-4 border border-sev-medium/40 bg-sev-medium-bg px-3 py-2.5 text-xs text-sev-medium-fg">
              <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
              <span>
                You&apos;re using a temporary password. Choose a new one to continue.
              </span>
            </div>
          ) : null}

          {error ? (
            <div
              role="alert"
              className="rounded-r-3 border border-destructive/20 bg-destructive/10 px-3 py-2 text-sm text-destructive"
            >
              {error}
            </div>
          ) : null}

          <Form {...form}>
            <form
              className="flex flex-col gap-4"
              onSubmit={form.handleSubmit(onSubmit)}
              noValidate
            >
              <FormField
                control={form.control}
                name="current_password"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Current password</FormLabel>
                    <FormControl>
                      <Input type="password" autoComplete="current-password" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="new_password"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>New password</FormLabel>
                    <FormControl>
                      <Input type="password" autoComplete="new-password" {...field} />
                    </FormControl>
                    <FormDescription>
                      {PASSWORD_MIN_LENGTH}–{PASSWORD_MAX_LENGTH} characters, not your
                      username.
                    </FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="confirm_password"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Confirm new password</FormLabel>
                    <FormControl>
                      <Input type="password" autoComplete="new-password" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />

              <Button
                type="submit"
                className="h-10 w-full"
                disabled={changePassword.isPending}
              >
                {changePassword.isPending ? (
                  <Loader2 className="size-4 animate-spin" aria-hidden="true" />
                ) : null}
                Change password
              </Button>
            </form>
          </Form>

          {mustChangePassword ? (
            <Button
              type="button"
              variant="ghost"
              onClick={() => {
                signOut()
                navigate('/login', { replace: true })
              }}
            >
              <LogOut className="size-4" aria-hidden="true" />
              Log out
            </Button>
          ) : (
            <Link
              to="/"
              className="self-center text-xs font-medium text-text-link underline underline-offset-2 hover:opacity-80"
            >
              Back to the console
            </Link>
          )}
        </div>
      </div>
    </div>
  )
}
