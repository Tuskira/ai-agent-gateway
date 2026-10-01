-- MCP catalog: curated MCP servers a tenant admin can add to their tenant.
-- tenant_id NULL is a PLATFORM row visible to every tenant (seeded from
-- config or written by a platform admin); a tenant row is that tenant's own
-- entry and, on a shared slug, overrides the platform one for that tenant --
-- the same convention as models (migration 000002), including the
-- COALESCE-onto-a-sentinel partial unique index. Entries are inert: adding
-- one creates an ordinary tenant connector that points back here through
-- connectors.catalog_id. auth describes what the tenant must supply (see
-- pkg/store.MCPCatalogAuth).
CREATE TABLE mcp_catalog (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID REFERENCES tenants(id) ON DELETE CASCADE,
    slug            TEXT NOT NULL,
    name            TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    icon            TEXT NOT NULL DEFAULT '',
    category        TEXT NOT NULL DEFAULT '',
    url             TEXT NOT NULL,
    url_overridable BOOLEAN NOT NULL DEFAULT FALSE,
    transport       TEXT NOT NULL DEFAULT 'streamable-http',
    auth            JSONB NOT NULL DEFAULT '{"kind":"none","fields":[]}',
    default_headers JSONB NOT NULL DEFAULT '{}',
    suggested_tools JSONB NOT NULL DEFAULT '[]',
    docs_url        TEXT NOT NULL DEFAULT '',
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);
CREATE UNIQUE INDEX mcp_catalog_tenant_slug_unique
    ON mcp_catalog (COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid), slug)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_mcp_catalog_tenant_id ON mcp_catalog(tenant_id);

ALTER TABLE connectors ADD COLUMN catalog_id UUID REFERENCES mcp_catalog(id);
CREATE INDEX idx_connectors_catalog_id ON connectors(catalog_id) WHERE catalog_id IS NOT NULL;
