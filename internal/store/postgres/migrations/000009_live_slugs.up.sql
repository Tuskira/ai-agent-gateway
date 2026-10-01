-- Connector and agent-profile slugs are unique among live rows only, so a
-- soft-deleted row no longer holds its slug: deleting an MCP or profile and
-- adding it again under the same slug works. Same convention as models,
-- skills and the MCP catalog.
ALTER TABLE connectors DROP CONSTRAINT connectors_tenant_slug_unique;
CREATE UNIQUE INDEX connectors_tenant_slug_unique
    ON connectors (tenant_id, slug) WHERE deleted_at IS NULL;

ALTER TABLE agent_profiles DROP CONSTRAINT agent_profiles_tenant_slug_unique;
CREATE UNIQUE INDEX agent_profiles_tenant_slug_unique
    ON agent_profiles (tenant_id, slug) WHERE deleted_at IS NULL;
