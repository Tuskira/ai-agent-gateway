ALTER TABLE models DROP COLUMN IF EXISTS capabilities;
ALTER TABLE models DROP COLUMN IF EXISTS catalog_model_id;
DROP TABLE IF EXISTS model_catalog_models;
DROP TABLE IF EXISTS model_catalog_providers;
