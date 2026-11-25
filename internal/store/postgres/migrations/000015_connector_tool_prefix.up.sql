-- An optional per-connector tool_prefix: the namespace the connector's tools,
-- prompts and resources are advertised and routed under, in place of its
-- slug. NULL (the default) keeps today's behaviour. Unique among a tenant's
-- live connectors, case-insensitively.
ALTER TABLE connectors ADD COLUMN IF NOT EXISTS tool_prefix VARCHAR(100);

CREATE UNIQUE INDEX IF NOT EXISTS connectors_tenant_tool_prefix_live
    ON connectors (tenant_id, lower(tool_prefix))
    WHERE deleted_at IS NULL AND tool_prefix IS NOT NULL;
