-- Console users, their browser sessions, and the authentication audit
-- trail. A user belongs to exactly one tenant; usernames are lowercase and
-- unique per tenant among live (non-deleted) users, so a soft-deleted
-- user's name can be reused.
CREATE TABLE users (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    username             TEXT NOT NULL,
    display_name         TEXT NOT NULL DEFAULT '',
    password_hash        TEXT NOT NULL,
    role                 TEXT NOT NULL CHECK (role IN ('admin', 'viewer')),
    must_change_password BOOLEAN NOT NULL DEFAULT FALSE,
    disabled             BOOLEAN NOT NULL DEFAULT FALSE,
    failed_logins        INTEGER NOT NULL DEFAULT 0,
    locked_until         TIMESTAMPTZ,
    last_login_at        TIMESTAMPTZ,
    password_changed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at           TIMESTAMPTZ
);
CREATE UNIQUE INDEX users_tenant_username_live ON users (tenant_id, username) WHERE deleted_at IS NULL;

-- id is the hex SHA-256 of the opaque session token: the raw token only
-- ever lives in the browser cookie, so reading this table never yields a
-- usable session.
CREATE TABLE user_sessions (
    id           TEXT PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tenant_id    UUID NOT NULL,
    csrf_token   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ,
    user_agent   TEXT NOT NULL DEFAULT '',
    ip           TEXT NOT NULL DEFAULT ''
);
CREATE INDEX user_sessions_user ON user_sessions (user_id) WHERE revoked_at IS NULL;

-- Append-only. tenant_id is nullable (a login attempt against an unknown
-- tenant has none) and deliberately not a foreign key, so the trail
-- outlives what it describes.
CREATE TABLE auth_audit (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      UUID,
    actor_kind     TEXT NOT NULL,
    actor_id       TEXT NOT NULL DEFAULT '',
    action         TEXT NOT NULL,
    target_user_id UUID,
    ip             TEXT NOT NULL DEFAULT '',
    detail         TEXT NOT NULL DEFAULT '',
    at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX auth_audit_tenant_at ON auth_audit (tenant_id, at DESC);
