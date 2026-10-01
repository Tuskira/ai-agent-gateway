import { Navigate, Outlet, useLocation } from 'react-router-dom'
import { useAuth } from '@/auth/AuthContext'

export const CHANGE_PASSWORD_PATH = '/change-password'

/** Route guard for the authenticated app shell. Redirects to /login when there
 * is no session, and to the change-password page while a password change is
 * forced. */
export function RequireAuth() {
  const { principal, isLoading, mustChangePassword } = useAuth()
  const location = useLocation()

  if (isLoading) {
    return (
      <div className="flex h-svh items-center justify-center text-sm text-muted-foreground">
        Loading…
      </div>
    )
  }

  if (!principal) {
    return <Navigate to="/login" replace state={{ from: location }} />
  }

  if (mustChangePassword && location.pathname !== CHANGE_PASSWORD_PATH) {
    return <Navigate to={CHANGE_PASSWORD_PATH} replace />
  }

  return <Outlet />
}
