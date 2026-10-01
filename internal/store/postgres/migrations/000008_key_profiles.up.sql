-- Bind an API key to one agent profile. When set, the MCP plane enforces
-- that profile for the key and refuses a different X-Agent-Profile-Name;
-- NULL keeps the old behaviour (the caller names the profile by header).
ALTER TABLE api_keys
    ADD COLUMN profile_id UUID NULL REFERENCES agent_profiles(id) ON DELETE SET NULL;
