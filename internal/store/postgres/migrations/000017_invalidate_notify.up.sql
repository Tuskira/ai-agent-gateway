-- Push cache invalidations between gateway processes.
--
-- Every committed write to a row a gateway process caches publishes one
-- event on the gateway_invalidate channel, {"kind","tenant","id"}, from a
-- row trigger: the notification is queued inside the writing transaction
-- and delivered only if it commits, whatever statement or process wrote.
-- Identical events inside one transaction are delivered once (Postgres
-- collapses duplicate payloads), so a bulk write is one event.
--
--   profile     agent_profiles, agent_profile_tools, profile_skills
--               (id = the profile), skills, skill_versions (id empty: any
--               profile in the tenant may attach the skill; tenant empty:
--               a platform skill, every tenant)
--   credential  credentials (id = the credential)
--   source      credential_sources (id = the source)
--   key         api_keys (id = the key hash), on any change but last_used_at

CREATE FUNCTION gateway_notify_invalidate(kind text, tenant uuid, id text) RETURNS void
LANGUAGE sql AS $$
    SELECT pg_notify('gateway_invalidate',
        json_build_object('kind', kind, 'tenant', COALESCE(tenant::text, ''), 'id', COALESCE(id, ''))::text)
$$;

-- Rows with their own tenant_id and id.
CREATE FUNCTION gateway_invalidate_self() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    r record;
BEGIN
    IF TG_OP = 'DELETE' THEN r := OLD; ELSE r := NEW; END IF;
    PERFORM gateway_notify_invalidate(TG_ARGV[0], r.tenant_id, r.id::text);
    RETURN NULL;
END
$$;

CREATE TRIGGER agent_profiles_invalidate AFTER INSERT OR UPDATE OR DELETE ON agent_profiles
    FOR EACH ROW EXECUTE FUNCTION gateway_invalidate_self('profile');
CREATE TRIGGER credentials_invalidate AFTER INSERT OR UPDATE OR DELETE ON credentials
    FOR EACH ROW EXECUTE FUNCTION gateway_invalidate_self('credential');
CREATE TRIGGER credential_sources_invalidate AFTER INSERT OR UPDATE OR DELETE ON credential_sources
    FOR EACH ROW EXECUTE FUNCTION gateway_invalidate_self('source');

-- Rows that belong to a profile (agent_profile_id).
CREATE FUNCTION gateway_invalidate_profile_child() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    pid uuid;
    tid uuid;
BEGIN
    IF TG_OP = 'DELETE' THEN pid := OLD.agent_profile_id; ELSE pid := NEW.agent_profile_id; END IF;
    SELECT tenant_id INTO tid FROM agent_profiles WHERE id = pid;
    -- A profile row already deleted by a cascade announced itself.
    IF FOUND THEN
        PERFORM gateway_notify_invalidate('profile', tid, pid::text);
    END IF;
    RETURN NULL;
END
$$;

CREATE TRIGGER agent_profile_tools_invalidate AFTER INSERT OR UPDATE OR DELETE ON agent_profile_tools
    FOR EACH ROW EXECUTE FUNCTION gateway_invalidate_profile_child();
CREATE TRIGGER profile_skills_invalidate AFTER INSERT OR UPDATE OR DELETE ON profile_skills
    FOR EACH ROW EXECUTE FUNCTION gateway_invalidate_profile_child();

-- Skill registry: any profile of the tenant (or of every tenant, for a
-- platform skill) may attach the skill, so the event names no profile.
CREATE FUNCTION gateway_invalidate_skill() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    r record;
BEGIN
    IF TG_OP = 'DELETE' THEN r := OLD; ELSE r := NEW; END IF;
    PERFORM gateway_notify_invalidate('profile', r.tenant_id, '');
    RETURN NULL;
END
$$;

CREATE FUNCTION gateway_invalidate_skill_version() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    sid uuid;
    tid uuid;
BEGIN
    IF TG_OP = 'DELETE' THEN sid := OLD.skill_id; ELSE sid := NEW.skill_id; END IF;
    SELECT tenant_id INTO tid FROM skills WHERE id = sid;
    IF FOUND THEN
        PERFORM gateway_notify_invalidate('profile', tid, '');
    END IF;
    RETURN NULL;
END
$$;

CREATE TRIGGER skills_invalidate AFTER INSERT OR UPDATE OR DELETE ON skills
    FOR EACH ROW EXECUTE FUNCTION gateway_invalidate_skill();
CREATE TRIGGER skill_versions_invalidate AFTER INSERT OR UPDATE OR DELETE ON skill_versions
    FOR EACH ROW EXECUTE FUNCTION gateway_invalidate_skill_version();

-- API keys: a change to anything but last_used_at (touched on use) or a
-- delete. Revocation also still publishes on gateway_key_revoked.
CREATE FUNCTION gateway_invalidate_key() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM gateway_notify_invalidate('key', OLD.tenant_id, OLD.key_hash);
    ELSIF to_jsonb(NEW) - 'last_used_at' IS DISTINCT FROM to_jsonb(OLD) - 'last_used_at' THEN
        PERFORM gateway_notify_invalidate('key', NEW.tenant_id, NEW.key_hash);
    END IF;
    RETURN NULL;
END
$$;

CREATE TRIGGER api_keys_invalidate AFTER UPDATE OR DELETE ON api_keys
    FOR EACH ROW EXECUTE FUNCTION gateway_invalidate_key();
