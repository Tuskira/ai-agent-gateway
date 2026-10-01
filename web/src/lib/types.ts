export type Role = 'admin' | 'agent' | 'interceptor' | 'viewer'

export interface Principal {
  subject: string
  tenant_id: string
  email: string
  roles: Role[]
  auth_method: string
  key_id: string
  /** `user` for a console session, `api_key` for a bearer key. Absent on
   * gateways that predate password login. */
  kind?: 'user' | 'api_key'
  /** Present for session principals. */
  user?: SessionUser
  must_change_password?: boolean
  /** Double-submit token for session principals; kept in memory only. */
  csrf_token?: string
}

export type UserRole = 'admin' | 'viewer'

/** The signed-in person, as returned by `/auth/login` and `/auth/me`. */
export interface SessionUser {
  id: string
  username: string
  display_name: string
  role: UserRole
  /** Tenant slug. */
  tenant: string
}

export interface HealthResponse {
  status: string
  plane: string
  version: string
}

export interface ApiErrorBody {
  error: {
    type: string
    message: string
  }
}
