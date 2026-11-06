-- Single sign-on (OIDC). A user_identities row links one identity at an
-- OIDC issuer (issuer + subject, as configured under provider) to one
-- console user of the provider's tenant. The first SSO login links the
-- pre-made user found by email; later logins match by subject only.
-- A user has at most one identity per (provider, issuer), so a second
-- account at the same issuer that claims the same email cannot take
-- the user over.
CREATE TABLE user_identities (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    provider      TEXT NOT NULL,
    issuer        TEXT NOT NULL,
    subject       TEXT NOT NULL,
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email         TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX user_identities_subject ON user_identities (tenant_id, provider, issuer, subject);
CREATE UNIQUE INDEX user_identities_user ON user_identities (tenant_id, provider, issuer, user_id);

-- How a session was started: 'password' (POST /auth/login) or 'oidc' (an
-- SSO callback). Reported as auth_method by GET /auth/me.
ALTER TABLE user_sessions
    ADD COLUMN auth_method TEXT NOT NULL DEFAULT 'password'
    CONSTRAINT user_sessions_auth_method_check CHECK (auth_method IN ('password', 'oidc'));
