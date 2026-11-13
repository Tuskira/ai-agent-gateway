-- Strict model policy for the LLM plane. A strict caller may only call
-- models that have an enabled row in the model registry; a model with no
-- row is refused (403) instead of being forwarded as-is.
--   tenants.strict_models   the tenant's policy (off by default)
--   api_keys.strict_models  a per-key override: NULL inherits the
--                           tenant's policy, true/false replaces it
ALTER TABLE tenants ADD COLUMN strict_models BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE api_keys ADD COLUMN strict_models BOOLEAN;
