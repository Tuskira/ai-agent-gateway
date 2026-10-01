-- Skills & commands registry: one registry table for both "skill" (a
-- Model-Skills-style bundle of text files the model can load on demand)
-- and "command" (a reusable prompt template rendered with named
-- arguments) resources, distinguished by kind. tenant_id NULL is a
-- PLATFORM row visible to every tenant and read-only for tenants (see
-- internal/api/handlers.Skills); a tenant row with the same name
-- overrides it for that tenant -- the same convention as models
-- (migration 000002), including the COALESCE-onto-a-sentinel partial
-- unique index (see models_tenant_name_unique's comment for why, over
-- NULLS NOT DISTINCT, which needs Postgres 15+).
CREATE TABLE skills (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID REFERENCES tenants(id) ON DELETE CASCADE,
    name           VARCHAR(64) NOT NULL,
    kind           VARCHAR(20) NOT NULL CHECK (kind IN ('skill', 'command')),
    description    TEXT NOT NULL DEFAULT '',
    frontmatter    JSONB NOT NULL DEFAULT '{}',
    arguments      JSONB NOT NULL DEFAULT '[]',
    latest_version INTEGER NOT NULL DEFAULT 0,
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    metadata       JSONB NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at     TIMESTAMPTZ
);
CREATE UNIQUE INDEX skills_tenant_name_unique
    ON skills (COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid), name)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_skills_tenant_id ON skills(tenant_id);

-- Every version of a skill's files, immutable once written. files is
-- [{"path","content","sha256","size"}, ...].
CREATE TABLE skill_versions (
    skill_id   UUID NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    version    INTEGER NOT NULL,
    files      JSONB NOT NULL,
    created_by VARCHAR(255) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (skill_id, version)
);

-- One row per (profile, skill) attachment. version NULL means "always
-- resolve to the skill's current latest_version"; a non-NULL version
-- pins a specific one.
CREATE TABLE profile_skills (
    agent_profile_id UUID NOT NULL REFERENCES agent_profiles(id) ON DELETE CASCADE,
    skill_id         UUID NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    version          INTEGER,
    PRIMARY KEY (agent_profile_id, skill_id)
);
CREATE INDEX idx_profile_skills_skill_id ON profile_skills(skill_id);

-- Free-text, profile-level instructions prepended to the resolved
-- profile's MCP initialize response (see pkg/store.AgentProfile.Instructions).
ALTER TABLE agent_profiles ADD COLUMN instructions TEXT NOT NULL DEFAULT '';
