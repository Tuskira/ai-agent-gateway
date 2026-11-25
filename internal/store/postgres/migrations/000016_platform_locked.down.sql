-- Hand rollback only (Migrate never applies a down migration).
ALTER TABLE credential_sources DROP COLUMN IF EXISTS locked;
ALTER TABLE credentials DROP COLUMN IF EXISTS locked;
