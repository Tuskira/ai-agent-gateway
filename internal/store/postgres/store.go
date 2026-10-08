package postgres

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Embedded schema migrations. v0.1 ships a small, fixed set, so rather than
// pull in a general-purpose migration framework (and its dependency tree),
// Migrate below applies each in order and tracks the applied versions in a
// small schema_migrations table — the same tracking shape a framework would
// give us, hand-rolled because there are only a handful of rows to track.
//
//go:embed migrations/000001_baseline.up.sql
var baselineMigration string

// modelsMigration adds the LLM plane's model registry (the models table).
//
//go:embed migrations/000002_models.up.sql
var modelsMigration string

//go:embed migrations/000003_limits.up.sql
var limitsMigration string

// skillsMigration adds the skills & commands registry (skills,
// skill_versions, profile_skills) and agent_profiles.instructions. Its
// sibling migrations/000004_skills.down.sql exists for an operator who
// wants to roll back by hand -- like 000001-000003, this package's
// Migrate never applies a down migration itself; there is no rollback
// path wired into the embed+slice mechanism below.
//
//go:embed migrations/000004_skills.up.sql
var skillsMigration string

// usersMigration adds console users, their sessions, and the auth audit
// trail. Its sibling 000005_users.down.sql is for a hand rollback only,
// like 000001-000004.
//
//go:embed migrations/000005_users.up.sql
var usersMigration string

// modelCatalogMigration adds the platform-wide model catalog
// (model_catalog_providers, model_catalog_models, seeded with a default
// catalog) and models.catalog_model_id/capabilities. Its sibling
// migrations/000006_model_catalog.down.sql is for a hand rollback only,
// like 000001-000005.
//
//go:embed migrations/000006_model_catalog.up.sql
var modelCatalogMigration string

// mcpCatalogMigration adds the platform MCP catalog and connectors.catalog_id.
// Its sibling 000007_mcp_catalog.down.sql is for a hand rollback only.
//
//go:embed migrations/000007_mcp_catalog.up.sql
var mcpCatalogMigration string

// keyProfilesMigration adds api_keys.profile_id (a key bound to a profile).
// Its sibling 000008_key_profiles.down.sql is for a hand rollback only.
//
//go:embed migrations/000008_key_profiles.up.sql
var keyProfilesMigration string

// liveSlugsMigration makes connector and profile slugs unique among live
// rows only. Its sibling 000009_live_slugs.down.sql is for a hand rollback only.
//
//go:embed migrations/000009_live_slugs.up.sql
var liveSlugsMigration string

// migrations are applied in version order; each is recorded in
// schema_migrations once applied. Append new migrations here (with the next
// version) — never edit one that has already shipped.
var migrations = []struct {
	version int64
	sql     string
}{
	{1, baselineMigration},
	{2, modelsMigration},
	{3, limitsMigration},
	{4, skillsMigration},
	{5, usersMigration},
	{6, modelCatalogMigration},
	{7, mcpCatalogMigration},
	{8, keyProfilesMigration},
	{9, liveSlugsMigration},
}

// migrationLockKey is an arbitrary constant used as a Postgres advisory
// lock key while migrating, so concurrent callers (e.g. multiple replicas
// booting at once with database.migrate: true) serialize instead of
// racing to create the same tables.
const migrationLockKey = 0x7473_6b5f_6777 // "tsk_gw", arbitrary

// Store is the Postgres implementation of store.Store. Construct with New
// (given an already-open *sql.DB) or, more commonly, via store.Open with
// driver "postgres" once this package has been imported for its side
// effect (see init below).
type Store struct {
	db *sql.DB
}

// New wraps an existing, already-connected *sql.DB as a store.Store. The
// caller retains ownership of db's lifecycle beyond Close: Close here
// closes db too, so callers should not use db elsewhere afterward.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

func init() {
	store.Register("postgres", func(ctx context.Context, cfg config.Database) (store.Store, error) {
		db, err := Open(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return New(db), nil
	})
}

// Stats returns the connection pool's counters (open, idle and in-use
// connections, waits). It backs the gateway's db_* metrics; the *sql.DB
// itself stays private.
func (s *Store) Stats() sql.DBStats { return s.db.Stats() }

func (s *Store) Tenants() store.TenantStore             { return &tenantStore{db: s.db} }
func (s *Store) APIKeys() store.APIKeyStore             { return &apiKeyStore{db: s.db} }
func (s *Store) Credentials() store.CredentialStore     { return &credentialStore{db: s.db} }
func (s *Store) Connectors() store.ConnectorStore       { return &connectorStore{db: s.db} }
func (s *Store) MCPCatalog() store.MCPCatalogStore      { return &mcpCatalogStore{db: s.db} }
func (s *Store) AgentProfiles() store.AgentProfileStore { return &agentProfileStore{db: s.db} }
func (s *Store) ToolCache() store.ToolCacheStore        { return &toolCacheStore{db: s.db} }
func (s *Store) Models() store.ModelStore               { return &modelStore{db: s.db} }
func (s *Store) ModelCatalog() store.ModelCatalogStore  { return &modelCatalogStore{db: s.db} }
func (s *Store) Skills() store.SkillStore               { return &skillStore{db: s.db} }
func (s *Store) Users() store.UserStore                 { return &userStore{db: s.db} }
func (s *Store) UserSessions() store.UserSessionStore   { return &userSessionStore{db: s.db} }
func (s *Store) AuthAudit() store.AuthAuditStore        { return &authAuditStore{db: s.db} }

// Migrate applies the baseline schema if it hasn't been applied yet. It is
// safe to call repeatedly (a no-op once schema_migrations already records
// baselineVersion), and safe to call concurrently from multiple processes
// (serialized by a session-scoped advisory lock, auto-released with the
// transaction).
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: begin migration tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrationLockKey)); err != nil {
		return fmt.Errorf("postgres: acquire migration lock: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    BIGINT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("postgres: create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		var applied bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, m.version).Scan(&applied); err != nil {
			return fmt.Errorf("postgres: check schema_migrations: %w", err)
		}
		if applied {
			continue
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("postgres: apply migration %d: %w", m.version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
			return fmt.Errorf("postgres: record migration %d: %w", m.version, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: commit migration tx: %w", err)
	}
	return nil
}

// Ping verifies connectivity to Postgres.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("postgres: ping: %w", err)
	}
	return nil
}

// Close closes the underlying connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}
