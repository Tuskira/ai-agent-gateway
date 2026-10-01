DROP INDEX IF EXISTS idx_connectors_catalog_id;
ALTER TABLE connectors DROP COLUMN IF EXISTS catalog_id;
DROP TABLE IF EXISTS mcp_catalog;
