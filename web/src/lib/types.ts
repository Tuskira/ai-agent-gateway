/** Principal roles as `/auth/me` reports them. A platform administrator
 * holds both `admin` and `platform-admin`. */
export type Role = 'admin' | 'agent' | 'interceptor' | 'viewer' | 'platform-admin'

export interface Principal {
  subject: string
  /** The tenant this request acts in. For a platform session that selected
   * another tenant this is the selected one, not the account's own. */
  tenant_id: string
  /** The tenant the account belongs to. Equals `tenant_id` unless a platform
   * session is acting in another tenant. Absent on older gateways. */
  home_tenant_id?: string
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

/** Console user roles. `platform-admin` may be assigned only by a caller
 * holding `platform.admin`. */
export type UserRole = 'admin' | 'viewer' | 'platform-admin'

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
