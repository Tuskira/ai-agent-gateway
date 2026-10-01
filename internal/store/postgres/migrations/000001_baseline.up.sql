-- Baseline schema: the seven tables of the v0.1 data model. gen_random_uuid()
-- is a Postgres built-in since 13, so no extension is required.

CREATE TABLE tenants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        VARCHAR(100) NOT NULL,
    name        VARCHAR(255) NOT NULL,
    settings    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT tenants_slug_unique UNIQUE (slug)
);

CREATE TABLE api_keys (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name          VARCHAR(255) NOT NULL,
    role          VARCHAR(50) NOT NULL,
    key_hash      VARCHAR(64) NOT NULL,
    key_prefix    VARCHAR(16) NOT NULL,
    expires_at    TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ,
    last_used_at  TIMESTAMPTZ,
    created_by    VARCHAR(255) NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT api_keys_key_hash_unique UNIQUE (key_hash)
);
CREATE INDEX idx_api_keys_tenant_id ON api_keys(tenant_id);

CREATE TABLE credentials (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name          VARCHAR(255) NOT NULL,
    type          VARCHAR(50) NOT NULL,
    ciphertext    BYTEA NOT NULL,
    nonce         BYTEA NOT NULL,
    key_id        VARCHAR(100) NOT NULL,
    field_names   TEXT[] NOT NULL DEFAULT '{}',
    created_by    VARCHAR(255) NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at    TIMESTAMPTZ,
    CONSTRAINT credentials_tenant_name_unique UNIQUE (tenant_id, name)
);
CREATE INDEX idx_credentials_tenant_id ON credentials(tenant_id);

CREATE TABLE connectors (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name          VARCHAR(255) NOT NULL,
    slug          VARCHAR(100) NOT NULL,
    endpoint      VARCHAR(500) NOT NULL,
    timeout_ms    INTEGER NOT NULL DEFAULT 30000,
    status        VARCHAR(50) NOT NULL DEFAULT 'unknown',
    capabilities  JSONB NOT NULL DEFAULT '{}',
    metadata      JSONB NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ,
    CONSTRAINT connectors_tenant_slug_unique UNIQUE (tenant_id, slug)
);
CREATE INDEX idx_connectors_tenant_id ON connectors(tenant_id);

CREATE TABLE agent_profiles (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name          VARCHAR(255) NOT NULL,
    slug          VARCHAR(100) NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    metadata      JSONB NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ,
    CONSTRAINT agent_profiles_tenant_slug_unique UNIQUE (tenant_id, slug)
);
CREATE INDEX idx_agent_profiles_tenant_id ON agent_profiles(tenant_id);

CREATE TABLE agent_profile_tools (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_profile_id    UUID NOT NULL REFERENCES agent_profiles(id) ON DELETE CASCADE,
    connector_id        UUID NOT NULL REFERENCES connectors(id) ON DELETE CASCADE,
    tool_namespace      VARCHAR(255) NOT NULL DEFAULT '',
    tool_name           VARCHAR(255) NOT NULL,
    CONSTRAINT agent_profile_tools_unique UNIQUE (agent_profile_id, connector_id, tool_namespace, tool_name)
);
CREATE INDEX idx_agent_profile_tools_profile_id ON agent_profile_tools(agent_profile_id);
CREATE INDEX idx_agent_profile_tools_connector_id ON agent_profile_tools(connector_id);

CREATE TABLE tool_cache (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    connector_id    UUID NOT NULL REFERENCES connectors(id) ON DELETE CASCADE,
    tool_namespace  VARCHAR(255) NOT NULL DEFAULT '',
    tool_name       VARCHAR(255) NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    input_schema    JSONB NOT NULL DEFAULT '{}',
    search_vector   tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', coalesce(tool_name, '')), 'A') ||
        setweight(to_tsvector('english', coalesce(tool_namespace, '')), 'B') ||
        setweight(to_tsvector('english', coalesce(description, '')), 'C')
    ) STORED,
    cached_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    is_stale    BOOLEAN NOT NULL DEFAULT FALSE,
    CONSTRAINT tool_cache_unique UNIQUE (tenant_id, connector_id, tool_namespace, tool_name)
);
CREATE INDEX idx_tool_cache_tenant_connector ON tool_cache(tenant_id, connector_id);
CREATE INDEX idx_tool_cache_search ON tool_cache USING gin(search_vector);
CREATE INDEX idx_tool_cache_expires ON tool_cache(expires_at) WHERE is_stale = FALSE;
