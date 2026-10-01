-- Fails if a slug was reused after a soft delete; purge those deleted rows first.
DROP INDEX IF EXISTS connectors_tenant_slug_unique;
ALTER TABLE connectors ADD CONSTRAINT connectors_tenant_slug_unique UNIQUE (tenant_id, slug);

DROP INDEX IF EXISTS agent_profiles_tenant_slug_unique;
ALTER TABLE agent_profiles ADD CONSTRAINT agent_profiles_tenant_slug_unique UNIQUE (tenant_id, slug);
