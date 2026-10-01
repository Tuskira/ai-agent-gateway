-- Model catalog: a platform-wide list of known providers and their models
-- (templates). Not callable on its own -- a tenant "connects" a provider
-- (POST /model-catalog/providers/{id}/connect) by supplying an API key
-- once; the gateway then creates ordinary tenant `models` rows from the
-- selected catalog models. Editing the catalog afterward never changes a
-- tenant's already-connected rows (see models.catalog_model_id below).

CREATE TABLE IF NOT EXISTS model_catalog_providers (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug         TEXT NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,31}$'),
    display_name TEXT NOT NULL,
    -- v1: OpenAI-compatible upstreams only.
    vendor       TEXT NOT NULL DEFAULT 'openai_compat' CHECK (vendor IN ('openai_compat')),
    -- OpenAI-compatible base INCLUDING the version segment, no trailing
    -- slash (e.g. https://api.openai.com/v1) -- see docs/llm-plane.md,
    -- "Model registry" for the convention and internal/llmplane's join fix.
    base_url     TEXT NOT NULL,
    docs_url     TEXT NOT NULL DEFAULT '',
    enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS model_catalog_models (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id    UUID NOT NULL REFERENCES model_catalog_providers(id) ON DELETE CASCADE,
    -- The vendor's own id, e.g. zai-org/GLM-5.3 (any case, may contain '/').
    model_id       TEXT NOT NULL,
    display_name   TEXT NOT NULL DEFAULT '',
    -- The registry name a Connect call gives the tenant model it creates;
    -- unique across the WHOLE catalog (not just the provider), since it
    -- becomes a real models.name.
    suggested_name TEXT NOT NULL CHECK (suggested_name ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    -- {"input": usd_per_1M, "output": usd_per_1M}, same shape as
    -- models.price; NULL = unknown, never a guessed rate.
    price          JSONB,
    -- {"tools": bool|null, "vision": bool|null, "streaming": bool|null,
    -- "max_context": int|null}; a missing/null field means unknown.
    capabilities   JSONB NOT NULL DEFAULT '{}',
    -- Free-text admin note, e.g. explaining a false capability above (a
    -- model whose tool use isn't supported through the current
    -- translation path yet).
    notes          TEXT NOT NULL DEFAULT '',
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, model_id),
    UNIQUE (suggested_name)
);
CREATE INDEX IF NOT EXISTS idx_model_catalog_models_provider_id ON model_catalog_models(provider_id);

-- catalog_model_id links a tenant's registry row back to the catalog model
-- it was connected from ("" / NULL for a hand-created row); ON DELETE SET
-- NULL so deleting a catalog model/provider detaches, rather than breaks,
-- a tenant's already-registered model. capabilities mirrors
-- model_catalog_models.capabilities for a connected row (NULL for a
-- hand-created one); informational only, the LLM plane does not enforce
-- it.
ALTER TABLE models ADD COLUMN IF NOT EXISTS catalog_model_id UUID NULL REFERENCES model_catalog_models(id) ON DELETE SET NULL;
ALTER TABLE models ADD COLUMN IF NOT EXISTS capabilities JSONB NULL;

-- Default catalog. Idempotent (ON CONFLICT DO NOTHING keyed on the unique
-- slug/suggested_name constraints above) so re-running this file by hand
-- against an already-seeded database is a no-op, not an error; admins may
-- edit or delete these rows afterward like any other. Prices are left
-- unknown (NULL) deliberately -- never invented numbers. Capabilities are
-- known here (unlike price): Kimi/GLM's tool use works; GPT-6/Gemini-3's
-- multi-turn tool use through the current Chat Completions translation
-- path does not (plain chat works) -- see docs/llm-plane.md.
INSERT INTO model_catalog_providers (slug, display_name, base_url, docs_url) VALUES
    ('nebius',   'Nebius AI Studio', 'https://api.tokenfactory.eu-west2.nebius.com/v1', 'https://docs.nebius.com/studio/inference/'),
    ('together', 'Together AI',      'https://api.together.ai/v1',                      'https://docs.together.ai/'),
    ('openai',   'OpenAI',           'https://api.openai.com/v1',                       'https://platform.openai.com/docs'),
    ('gemini',   'Google Gemini',    'https://generativelanguage.googleapis.com/v1beta/openai', 'https://ai.google.dev/gemini-api/docs')
ON CONFLICT (slug) DO NOTHING;

INSERT INTO model_catalog_models (provider_id, model_id, suggested_name, capabilities, notes)
SELECT p.id, v.model_id, v.suggested_name, v.capabilities::jsonb, v.notes
FROM (VALUES
    ('nebius',   'moonshotai/Kimi-K3',  'kimi-k3-nebius',        '{"tools": true, "streaming": true}',  ''),
    ('nebius',   'zai-org/GLM-5.3',     'glm-5.3-nebius',        '{"tools": true, "streaming": true}',  ''),
    ('together', 'zai-org/GLM-5.3',     'glm-5.3-together',      '{"tools": true, "streaming": true}',  ''),
    ('together', 'moonshotai/Kimi-K3',  'kimi-k3-together',      '{"tools": true, "streaming": true}',  ''),
    ('openai',   'gpt-6-astra',         'gpt-6-astra-openai',    '{"tools": false, "streaming": true}', 'Multi-turn tool use through Chat Completions translation is not supported yet; plain chat works.'),
    ('openai',   'gpt-6.1-sol',         'gpt-6.1-sol-openai',    '{"tools": false, "streaming": true}', 'Multi-turn tool use through Chat Completions translation is not supported yet; plain chat works.'),
    ('openai',   'gpt-6-luna',          'gpt-6-luna-openai',     '{"tools": false, "streaming": true}', 'Multi-turn tool use through Chat Completions translation is not supported yet; plain chat works.'),
    ('gemini',   'gemini-3.8-flash',    'gemini-3.8-flash-gemini', '{"tools": false, "streaming": true}', 'Multi-turn tool use through Chat Completions translation is not supported yet; plain chat works.'),
    ('gemini',   'gemini-3.7-flash',    'gemini-3.7-flash-gemini', '{"tools": false, "streaming": true}', 'Multi-turn tool use through Chat Completions translation is not supported yet; plain chat works.')
) AS v(provider_slug, model_id, suggested_name, capabilities, notes)
JOIN model_catalog_providers p ON p.slug = v.provider_slug
ON CONFLICT (suggested_name) DO NOTHING;
