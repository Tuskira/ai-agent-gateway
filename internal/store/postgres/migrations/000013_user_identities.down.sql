-- Hand rollback only (Migrate never applies a down migration).
ALTER TABLE user_sessions DROP COLUMN auth_method;
DROP TABLE user_identities;
