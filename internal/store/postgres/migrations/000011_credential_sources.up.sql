-- Credential sources: a tenant's registrations of outside credential
-- services that connector headers can resolve secrets from (the
-- external_service header provider). config holds non-secret settings only
-- (the service URL and the NAME of a gateway credential that
-- authenticates to it); secret material stays in credentials. Names are
-- unique among live rows, so a soft-deleted source frees its name.
CREATE TABLE credential_sources (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name        VARCHAR(255) NOT NULL,
    provider    VARCHAR(100) NOT NULL,
    config      JSONB NOT NULL DEFAULT '{}',
    enabled     BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);
CREATE INDEX idx_credential_sources_tenant_id ON credential_sources(tenant_id);
CREATE UNIQUE INDEX credential_sources_tenant_name_unique
    ON credential_sources (tenant_id, name) WHERE deleted_at IS NULL;
