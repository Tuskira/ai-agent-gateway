-- Console users may hold the platform-admin role: a tenant admin that also
-- holds every "platform." permission (tenant enumeration/creation, the
-- platform catalogs) and may act in any tenant from the console. The
-- CHECK constraint mirrors pkg/store's closed role set, so it is widened
-- here; the constraint name is the one Postgres gave the inline CHECK in
-- 000005_users.up.sql.
ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check
    CHECK (role IN ('admin', 'viewer', 'platform-admin'));

-- actor_tenant_id records the tenant the acting principal belongs to when
-- it differs from tenant_id (a platform-admin acting in another tenant).
-- NULL otherwise. Like tenant_id it is deliberately not a foreign key, so
-- the trail outlives what it describes.
ALTER TABLE auth_audit ADD COLUMN actor_tenant_id UUID;
