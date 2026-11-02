-- Hand rollback only (Migrate never applies a down migration).
ALTER TABLE agent_profile_tools DROP COLUMN permission;
ALTER TABLE api_keys DROP COLUMN containment_note;
ALTER TABLE api_keys DROP COLUMN restricted_at;
ALTER TABLE api_keys DROP COLUMN suspended_at;
