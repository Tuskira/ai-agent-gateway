-- Fails while any live or soft-deleted user still has role 'platform-admin':
-- the narrowed CHECK is validated against every existing row. Demote
-- (UPDATE users SET role = 'admin' WHERE role = 'platform-admin') or purge
-- those rows first, then re-run.
ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check
    CHECK (role IN ('admin', 'viewer'));

ALTER TABLE auth_audit DROP COLUMN actor_tenant_id;
