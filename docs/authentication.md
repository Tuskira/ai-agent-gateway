# Authentication

The gateway has two kinds of caller and two ways in.

| | Who | How they prove it | Used for |
|---|---|---|---|
| **API key** | A program: an agent, CI, the interceptor | `Authorization: Bearer gk_...` or `X-Gateway-Key: gk_...` | Every plane: MCP, LLM, and the control-plane REST API |
| **Console user** | A person | Username and password, exchanged for a session cookie | The admin console, and the control-plane REST API from a browser |

The two are separate on purpose. A user never calls the MCP or LLM planes
(those accept API keys only), and an API key never needs a password.
Both resolve to the same `Principal`, so everything downstream (roles,
permissions, tenant scoping) works the same way. See
[security-model.md](security-model.md) for the permission model itself.

## Users and tenants

A console user belongs to **exactly one tenant**. A username is lowercase
(`^[a-z0-9][a-z0-9._@+-]{2,63}$`, so an email address works), unique within
its tenant among live users, and immutable. Deleting a user is a soft
delete: the name becomes free again.

| Role | Permissions |
|---|---|
| `admin` | Everything the `admin` API-key role has: connectors (MCP servers registered with the gateway; the console's **MCPs** page), profiles, models, skills, credentials, API keys, users, the audit trail. Never `platform.admin`: it cannot list or create tenants, assign the `platform-admin` role, or act in another tenant. Holds `platform.catalog.manage` (model catalog writes) only while the deployment has exactly one tenant. |
| `platform-admin` | Everything `admin` has, plus `platform.*`: list and create tenants, manage the platform catalogs, create other `platform-admin` users. May act in any tenant from the console (see [Acting in another tenant](#acting-in-another-tenant)). The console counterpart of a `platform-admin` API key; only a caller who already holds `platform.admin` can assign it. |
| `viewer` | `*.read` only. Not `mcp.*` or `llm.*`, and not `users.manage`, so a viewer cannot list users or read the audit trail. |

Tenant creation (`platform.admin`) needs a `platform-admin` API key or
user, or the CLI: an `admin` user never holds it, whatever its role's
patterns say. `viewer` is also a valid role for an API key.

The user-management routes (`/users*` and `/auth/audit`) are gated on one
permission, `users.manage`. Like `admin.manage`, only the `admin` role's
`*` grant matches it; `agent` and `viewer` (`*.read`) do not. Admin-role
**API keys** pass, so automation can create users and reset passwords;
**agent keys get `403`**.

## The first user

`bootstrap-key` creates a tenant and an admin API key (the very first key in
a database also gets `platform-admin`; pass `--platform` for a later one). For a person, use
`create-user` instead:

```sh
gateway create-user -tenant default -username alice@example.com -role admin -display-name "Alice"
```

It prints a one-time **temporary password** to stdout (everything else goes
to stderr, so `PW=$(gateway create-user ... 2>/dev/null)` works). The user
must change it at first login. The tenant must already exist
(`gateway bootstrap-key -tenant <slug>` creates one).

For a person who must manage **every** tenant, pass `-role platform-admin`.
`bootstrap-key` has no flag for this on purpose (its stdout is the API key
alone, so scripts can capture it), so a fresh install is two commands:

```sh
KEY=$(gateway bootstrap-key -tenant platform -name provisioning)
gateway create-user -tenant platform -username root@example.com -role platform-admin
```

The `-tenant` is only where the account lives and signs in; it does not
limit the role. Over the API, `POST /users` with `role: "platform-admin"`
needs a caller that already holds `platform.admin`, so this command is both
how the first platform-admin is made and the break-glass path when the last
one is gone. Keep such accounts few: one is full control of every tenant.

With Docker Compose:

```sh
docker compose -f deploy/docker-compose.yml exec gateway /gateway bootstrap-key
docker compose -f deploy/docker-compose.yml exec gateway /gateway create-user \
  -tenant default -username admin -role admin
```

Further users are created from the console's Users page or `POST /api/v1/users`.

### First run

On a single-tenant install with no active console user, the login page says
so. If `auth.console_api_key_login` is on, it opens on the API-key form with
"No console users yet. Sign in with an admin API key to create one."; sign in
with an admin key and create your user on the Users page. If API-key login is
off, it says to ask your operator to run `gateway create-user`. A user who is
disabled or deleted does not count. With more than one tenant the page shows
no notice, so the public endpoint never reveals which tenants have users.

After an API-key sign-in, an admin sees a dismissible banner (per browser tab
session) asking them to create a personal login, since API-key sign-in is
planned for removal. The Users page also warns an API-key session when the
tenant has no active admin user.

When `auth.console_api_key_login` is `false`, the gateway logs a WARN at
startup for each tenant with no active admin user, because nobody can then
sign in to that tenant's console:

```
console API-key login is disabled and tenant <slug> has no active admin user; create one with 'gateway create-user'
```

## Logging in

`POST /api/v1/auth/login` with `{"username": "...", "password": "...", "tenant": "<slug>"}`.
`tenant` is optional. Without it the username is looked up across every
tenant: exactly one live account signs in as if its tenant had been named
(usernames are usually email addresses, so this is the normal case); none is
the ordinary `401` below, after the same hashing work; more than one is
`409` `tenant_required` with a body that names no tenant, and the caller
retries with `tenant` set. That `409` does reveal that the username exists
in several tenants, the accepted trade-off for not typing a tenant on every
login; it costs the caller the same as a failed login (a strike against the
per-IP lockout, the same hashing work, and a `login_fail` audit row with
detail `tenant_required`, filed under no tenant), so probing for it is no
cheaper than guessing passwords. `GET /api/v1/auth/config` reports
`tenant_optional: true`, so the console hides the tenant field and shows it
only after a `409` (an older gateway lacks the flag and the console then
shows the field, prefilled from `default_tenant` and hidden when
`single_tenant` is true). Clients that always send `tenant` keep working.

A successful login returns the user, whether a password change is pending,
and the session's CSRF token, and sets the session cookie:

```json
{"user": {"id": "...", "username": "alice@example.com", "display_name": "Alice", "role": "admin", "tenant": "default"},
 "must_change_password": false,
 "csrf_token": "..."}
```

Every failed login returns the same `401`, `invalid username or password`,
whether the tenant, the username or the password was wrong, and whether the
account is unknown, disabled or locked. The real reason is recorded in the
audit trail. When the account does not exist the server still does the same
hashing work, so the response time does not reveal which usernames exist.

## Acting in another tenant

Every tenant-scoped route acts in the caller's own tenant. A caller holding
`platform.admin` (a `platform-admin` user or API key) may act in another
tenant for one request by sending

```
X-Gateway-Acting-Tenant: <tenant UUID>
```

One middleware handles it (`internal/auth.TenantOverride`), mounted on the
control-plane API after authentication, the CSRF check and the
forced-password gate, so a forged or must-change request is refused on the
real account first. The MCP and LLM planes do not read the header.

| Caller | Header | Result |
|---|---|---|
| anyone | absent | the request acts in the caller's own tenant |
| without `platform.admin` (a tenant `admin` or `viewer`, any key without the role) | present | `403` `permission_error`. Never silently ignored: a request acting on the wrong tenant unnoticed is the dangerous failure, and the console never sends the header for such sessions |
| with `platform.admin` | not a UUID | `400` `validation_error` |
| with `platform.admin` | unknown tenant | `404` `not_found` |
| with `platform.admin` | an existing tenant | the request is scoped to that tenant: `Principal.TenantID` is the acting tenant and `Principal.HomeTenantID` the caller's own |

Self-service never follows the header: `GET /auth/me`, `POST /auth/password`
and `POST /auth/logout` always act on the caller's own account, and
`/auth/me` reports both `tenant_id` (acting) and `home_tenant_id`. The
`/tenants*` routes are platform-wide and are not scoped by it.

In the console a `platform-admin` session shows a tenant switcher in the top
bar and a **Tenants** page (list and create). Switching sends the header on
every call except the self-service routes above and `/auth/login` and
`/auth/config` (so `GET /auth/audit` shows the acting tenant's trail), resets
every cached answer of the previous tenant, and remembers the choice per
account in the browser. Other sessions never send it.

## Sessions, cookies and CSRF

A session is an opaque random token (32 bytes, base64url) in the cookie
**`gw_session`**: `HttpOnly`, `SameSite=Strict`, `Path=/`, and `Secure` when
the request arrived over TLS or carries `X-Forwarded-Proto: https` (or
always, with `auth.cookie_secure: true`). Only the SHA-256 of the token is
stored, in `user_sessions`, so a database read never yields a usable
session.

| Limit | Default | Key |
|---|---|---|
| Idle timeout (no request) | 8 h | `auth.session_idle` |
| Absolute lifetime from login | 24 h | `auth.session_max` |

The idle clock is refreshed at most once a minute. A disabled or deleted
user's live sessions stop working on their next request.

**CSRF.** Requests authenticated by the cookie use double-submit: every
`POST`, `PUT`, `PATCH` and `DELETE` must send the header
**`X-CSRF-Token`** equal to the session's `csrf_token` (returned by login
and `GET /auth/me`), or get `403` `csrf_error`. `GET`, `HEAD` and `OPTIONS` are exempt. API-key
requests are exempt, since a browser cannot be made to attach an
`Authorization` header to a forged request. A request with an
`Authorization: Bearer gk_...` header is always treated as an API-key
request, even if a session cookie is also present.

The cookie is accepted by the control-plane API only. The MCP and LLM planes
never read it: a cookie is not scoped by port, and those planes have no CSRF
check.

**Forced password change.** While `must_change_password` is true (a fresh
temporary password), a session may call only `GET /auth/me`,
`POST /auth/password` and `POST /auth/logout` (`GET /auth/config` is public).
Everything else is `403` with error type `password_change_required`.

## Passwords

- argon2id, `m=64 MiB, t=3, p=2`, 16-byte random salt, stored as a PHC
  string (`$argon2id$v=19$m=65536,t=3,p=2$...`); verified in constant time.
  Parameters are read back from each stored hash, so they can be raised later.
- Policy: at least 12 and at most 128 characters, and not equal to the
  username (case-insensitive).
- Temporary passwords are 20 characters from `crypto/rand`, base62 minus
  the look-alike characters `0 O o 1 I l`.
- Changing your own password (`POST /auth/password`) needs the current
  one, revokes all your **other** sessions, and clears the forced-change
  flag. A wrong current password counts toward the lockout below.

## Lockout and throttling

- **Per IP.** Login sits behind the same control-plane lockout as API keys
  ([security-model.md](security-model.md#rate-limiting-and-lockout)): 10
  failures from one IP in a minute lock it out for 5 minutes (`429` with
  `Retry-After`). A request that carries **no credential at all** does not
  count as a failure, so the console can probe `GET /auth/me` on every page
  load without locking anyone out.
- **Per user.** 5 consecutive wrong passwords lock that account for 15
  minutes. The response is the ordinary `invalid username or password`: a
  locked account is indistinguishable from a wrong password. Guessing while
  locked does not extend the lock, and a successful login resets the count.
  The count is per account, so a success on one account never resets another's.

## Resetting a password

There is no email reset. Three paths, all producing a one-time temporary
password, forcing a change at next login, and revoking every session of that
user:

1. **An admin in the console** (Users page) or
   `POST /api/v1/users/{id}/reset-password`.
2. **An admin API key**, the same endpoint: `Authorization: Bearer gk_...`.
   Tenant-scoped and audited with `actor_kind = api_key`. Agent keys get `403`.
3. **Break-glass, from the host**, when no admin can log in:

   ```sh
   gateway reset-password -tenant default -username alice@example.com
   ```

   The temporary password goes to stdout, the rest to stderr. Audited with
   `actor_kind = cli` and the operating-system user who ran it.

## Guards

- You cannot disable, delete or demote **yourself** (`409`, error type
  `self_action`).
- Nobody can disable, delete or demote a tenant's **last active admin**
  (`409`, `last_admin`). An admin API key is held to this too. Promoting
  that admin to `platform-admin` is not a demotion and is allowed: the user
  keeps every admin grant.
- Nobody can disable, delete or demote the deployment's **last active
  `platform-admin`**, in any tenant (`409`, `last_platform_admin`).
  `gateway create-user -role platform-admin` is the recovery path.
- Giving a user `platform-admin` (on create or a role change), and editing,
  disabling, deleting, resetting or logging out a user who holds it, needs
  the caller to hold `platform.admin` (`403`, `permission_error`). A tenant
  admin can neither mint a platform-admin nor touch one that lives in its
  tenant.
- The username cannot be changed.

The last-admin checks read a count and then write, without a lock: two
admins removing each other in the same instant could both pass them. The
break-glass CLI recovers that case.

## Audit trail

Every authentication action appends a row to `auth_audit`: `login_ok`,
`login_fail` (with the internal reason: `bad_password`, `unknown_user`,
`unknown_tenant`, `user_disabled`, `user_locked`, `password_too_long`,
`bad_stored_hash`), `logout`,
`password_change`, `password_reset`, `user_create`, `user_update`,
`user_disable`, `user_delete`, `session_revoke`. Each row records the actor
(`user`, `api_key`, `cli` or `anonymous`), the target user, the client IP and
a short detail. When the actor was acting from another tenant (a
`platform-admin` using `X-Gateway-Acting-Tenant`) the row also carries
`actor_tenant_id`, the actor's home tenant, so a tenant admin reading its own
trail sees that the change came from outside; the actor's own logins,
logout and password changes are recorded in its home tenant. Read it with
`GET /api/v1/auth/audit` (`users.manage`, tenant-scoped, newest first,
filterable by `action` and `user_id`). Audit
writes are best effort: a failed write is logged and does not fail the
request it describes. Failed logins against an unknown tenant have no tenant
and are therefore not readable through the API; they are in the table.

## API-key login on the console (emergency fallback)

For this release the console still offers "sign in with an API key" next to
the password form, so an existing deployment is not locked out the day it
upgrades. It is controlled by `auth.console_api_key_login` (default `true`),
reported to the console as `api_key_login` by `GET /auth/config`.

This is a convenience switch for the console only. **The server keeps
accepting API keys on every API route regardless of it**; turning it off
hides the option, it does not restrict keys. **This fallback is planned for
removal**: new deployments should set the flag to `false` once a user exists.

## SSO later

Sessions record who is logged in, not how they proved it, so an SSO login
(OIDC) can create the same `user_sessions` rows and reuse the cookie, CSRF,
role and audit machinery unchanged. Only a new login route and an
identity-to-user mapping would be added. Password login stays as the
break-glass path.

## Reference

Routes (full table in [api.md](api.md#route-table)):

| Route | Access |
|---|---|
| `GET /auth/config` | public |
| `POST /auth/login` | public, rate limited |
| `POST /auth/logout` | any authenticated caller (revokes the session, if any) |
| `GET /auth/me` | any caller |
| `POST /auth/password` | console user session only |
| `GET/POST /users`, `GET/PATCH/DELETE /users/{id}`, `POST /users/{id}/reset-password`, `POST /users/{id}/revoke-sessions`, `GET /auth/audit` | `users.manage` (`platform.admin` as well to assign or touch `platform-admin`) |
| `GET/POST /tenants` | `platform.admin` |

`X-Gateway-Acting-Tenant: <tenant UUID>` on any tenant-scoped route scopes
that request to the named tenant for a caller holding `platform.admin`; for
anyone else it is `403` (see [Acting in another tenant](#acting-in-another-tenant)).

Configuration: [`auth.console_api_key_login`, `auth.cookie_secure`,
`auth.session_idle`, `auth.session_max`, `auth.default_tenant`](configuration.md#auth).
