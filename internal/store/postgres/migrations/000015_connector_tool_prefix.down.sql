-- Hand rollback only (Migrate never applies a down migration).
DROP INDEX IF EXISTS connectors_tenant_tool_prefix_live;
ALTER TABLE connectors DROP COLUMN IF EXISTS tool_prefix;
