-- LLM budgets and limits (see docs/llm-plane.md "Budgets and limits"),
-- per API key and per registered model. NULL = no limits. Shape:
--   {"daily_usd": 5.0, "monthly_usd": 100.0, "rpm": 60, "max_tokens": 4096}
-- every field optional.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS limits JSONB NULL;
ALTER TABLE models ADD COLUMN IF NOT EXISTS limits JSONB NULL;
