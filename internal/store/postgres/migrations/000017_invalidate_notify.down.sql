-- Hand rollback only (Migrate never applies a down migration).
DROP TRIGGER api_keys_invalidate ON api_keys;
DROP TRIGGER skill_versions_invalidate ON skill_versions;
DROP TRIGGER skills_invalidate ON skills;
DROP TRIGGER profile_skills_invalidate ON profile_skills;
DROP TRIGGER agent_profile_tools_invalidate ON agent_profile_tools;
DROP TRIGGER credential_sources_invalidate ON credential_sources;
DROP TRIGGER credentials_invalidate ON credentials;
DROP TRIGGER agent_profiles_invalidate ON agent_profiles;
DROP FUNCTION gateway_invalidate_key();
DROP FUNCTION gateway_invalidate_skill_version();
DROP FUNCTION gateway_invalidate_skill();
DROP FUNCTION gateway_invalidate_profile_child();
DROP FUNCTION gateway_invalidate_self();
DROP FUNCTION gateway_notify_invalidate(text, uuid, text);
