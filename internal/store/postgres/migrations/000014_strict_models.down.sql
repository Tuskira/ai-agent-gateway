-- Hand rollback only (Migrate never applies a down migration).
ALTER TABLE api_keys DROP COLUMN strict_models;
ALTER TABLE tenants DROP COLUMN strict_models;
