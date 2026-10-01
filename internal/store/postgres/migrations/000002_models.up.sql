-- Model registry: client-facing model names mapped onto ordered upstream
-- targets. tenant_id NULL is a platform default visible to every tenant; a
-- tenant row with the same name overrides it for that tenant.
--
-- Uniqueness is enforced by a partial unique index rather than a table
-- constraint: (a) it must treat NULL tenant_ids as equal (two platform rows
-- may not share a name), which UNIQUE (tenant_id, name) does not before
-- Postgres 15's NULLS NOT DISTINCT, so the NULL is coalesced onto a fixed
-- sentinel; (b) it covers live rows only, so a soft-deleted name can be
-- registered again.

CREATE TABLE models (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID REFERENCES tenants(id) ON DELETE CASCADE,
    name          VARCHAR(64) NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    targets       JSONB NOT NULL,
    price         JSONB,
    metadata      JSONB NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ
);
CREATE UNIQUE INDEX models_tenant_name_unique
    ON models (COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid), name)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_models_tenant_id ON models(tenant_id);
