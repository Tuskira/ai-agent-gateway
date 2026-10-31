-- The containment ladder an admin (or a security tool acting through the
-- admin API) moves an agent's key along:
--   normal   both NULL
--   restrict restricted_at set: the key may call models, but the MCP plane
--            refuses every tools/call
--   kill     suspended_at set: refused on every plane like a revoked key,
--            but resumable
-- containment_note records why the key was last restricted, killed or
-- restored.
ALTER TABLE api_keys ADD COLUMN suspended_at TIMESTAMPTZ;
ALTER TABLE api_keys ADD COLUMN restricted_at TIMESTAMPTZ;
ALTER TABLE api_keys ADD COLUMN containment_note TEXT NOT NULL DEFAULT '';

-- Per-tool permission on a profile grant: 'allow' runs the call, 'monitor'
-- runs it and reports it to the detection agent as a monitored call. A tool
-- with no row is not granted.
ALTER TABLE agent_profile_tools
    ADD COLUMN permission VARCHAR(16) NOT NULL DEFAULT 'allow'
    CONSTRAINT agent_profile_tools_permission_check CHECK (permission IN ('allow', 'monitor'));
