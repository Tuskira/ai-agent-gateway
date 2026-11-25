-- Platform-locked credentials and credential sources. A platform operator
-- (platform.admin) marks a row locked; a tenant admin may still use it
-- (connector headers can reference a locked source or credential) but
-- cannot change, rotate, disable, delete or re-point it.
ALTER TABLE credentials ADD COLUMN IF NOT EXISTS locked BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE credential_sources ADD COLUMN IF NOT EXISTS locked BOOLEAN NOT NULL DEFAULT false;

-- "allow_locked" on a connector header is server-owned: the control API
-- sets it only for a platform admin, and only such a header may resolve a
-- platform-locked credential. Before this migration the API stored unknown
-- header keys as sent, so drop any copy a tenant may have saved earlier.
UPDATE connectors c
SET metadata = jsonb_set(c.metadata, '{headers}', (
    SELECT jsonb_object_agg(e.key, CASE WHEN jsonb_typeof(e.value) = 'object' THEN e.value - 'allow_locked' ELSE e.value END)
    FROM jsonb_each(c.metadata->'headers') AS e
))
WHERE jsonb_typeof(c.metadata->'headers') = 'object'
  AND EXISTS (
    SELECT 1 FROM jsonb_each(c.metadata->'headers') AS e
    WHERE jsonb_typeof(e.value) = 'object' AND e.value ? 'allow_locked'
  );
