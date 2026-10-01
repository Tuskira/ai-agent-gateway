package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type agentProfileStore struct{ db *sql.DB }

var _ store.AgentProfileStore = (*agentProfileStore)(nil)

func (s *agentProfileStore) Create(ctx context.Context, p *store.AgentProfile) error {
	meta, err := marshalJSONB(p.Metadata)
	if err != nil {
		return fmt.Errorf("postgres: marshal profile metadata: %w", err)
	}

	const q = `
		INSERT INTO agent_profiles (tenant_id, name, slug, description, metadata, instructions)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at, updated_at`
	err = s.db.QueryRowContext(ctx, q, p.TenantID, p.Name, p.Slug, p.Description, meta, p.Instructions).
		Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	return mapWriteErr(err)
}

func (s *agentProfileStore) Get(ctx context.Context, tenantID, id string) (*store.AgentProfile, error) {
	const q = agentProfileSelect + ` WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	p, err := scanAgentProfileRow(s.db.QueryRowContext(ctx, q, tenantID, id))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return p, nil
}

func (s *agentProfileStore) GetBySlug(ctx context.Context, tenantID, slug string) (*store.AgentProfile, error) {
	const q = agentProfileSelect + ` WHERE tenant_id = $1 AND slug = $2 AND deleted_at IS NULL`
	p, err := scanAgentProfileRow(s.db.QueryRowContext(ctx, q, tenantID, slug))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return p, nil
}

func (s *agentProfileStore) List(ctx context.Context, tenantID string) ([]*store.AgentProfile, error) {
	const q = agentProfileSelect + ` WHERE tenant_id = $1 AND deleted_at IS NULL ORDER BY created_at`
	rows, err := s.db.QueryContext(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list agent profiles: %w", err)
	}
	defer rows.Close()

	var out []*store.AgentProfile
	for rows.Next() {
		p, err := scanAgentProfileRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *agentProfileStore) Update(ctx context.Context, p *store.AgentProfile) error {
	meta, err := marshalJSONB(p.Metadata)
	if err != nil {
		return fmt.Errorf("postgres: marshal profile metadata: %w", err)
	}

	const q = `
		UPDATE agent_profiles SET name = $3, description = $4, metadata = $5, instructions = $6, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
		RETURNING updated_at`
	err = s.db.QueryRowContext(ctx, q, p.TenantID, p.ID, p.Name, p.Description, meta, p.Instructions).Scan(&p.UpdatedAt)
	if err != nil {
		return mapWriteErr(mapReadErr(err))
	}
	return nil
}

func (s *agentProfileStore) SoftDelete(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE agent_profiles SET deleted_at = now(), updated_at = now() WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	return execExpectingOneRow(ctx, s.db, q, tenantID, id)
}

// SetTools replaces profileID's entire tool allow-list with tools, in one
// transaction (delete-then-insert).
func (s *agentProfileStore) SetTools(ctx context.Context, tenantID, profileID string, tools []store.ProfileTool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: begin SetTools tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Verify the profile exists (and belongs to this tenant) before
	// touching its tools, so a bad id fails with ErrNotFound rather than
	// silently no-op'ing.
	var exists bool
	const checkQ = `SELECT EXISTS(SELECT 1 FROM agent_profiles WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL)`
	if err := tx.QueryRowContext(ctx, checkQ, tenantID, profileID).Scan(&exists); err != nil {
		return fmt.Errorf("postgres: check profile exists: %w", err)
	}
	if !exists {
		return store.ErrNotFound
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_profile_tools WHERE agent_profile_id = $1`, profileID); err != nil {
		return fmt.Errorf("postgres: clear profile tools: %w", err)
	}

	const insertQ = `INSERT INTO agent_profile_tools (agent_profile_id, connector_id, tool_namespace, tool_name) VALUES ($1, $2, $3, $4)`
	for _, tool := range tools {
		if _, err := tx.ExecContext(ctx, insertQ, profileID, tool.ConnectorID, tool.ToolNamespace, tool.ToolName); err != nil {
			return mapWriteErr(err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: commit SetTools tx: %w", err)
	}
	return nil
}

func (s *agentProfileStore) GetTools(ctx context.Context, tenantID, profileID string) ([]store.ProfileTool, error) {
	const q = `
		SELECT pt.agent_profile_id, pt.connector_id, pt.tool_namespace, pt.tool_name
		FROM agent_profile_tools pt
		JOIN agent_profiles p ON p.id = pt.agent_profile_id
		WHERE p.tenant_id = $1 AND pt.agent_profile_id = $2
		ORDER BY pt.tool_namespace, pt.tool_name`
	rows, err := s.db.QueryContext(ctx, q, tenantID, profileID)
	if err != nil {
		return nil, fmt.Errorf("postgres: get profile tools: %w", err)
	}
	defer rows.Close()

	var out []store.ProfileTool
	for rows.Next() {
		var t store.ProfileTool
		if err := rows.Scan(&t.AgentProfileID, &t.ConnectorID, &t.ToolNamespace, &t.ToolName); err != nil {
			return nil, fmt.Errorf("postgres: scan profile tool: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetSkills replaces profileID's entire skill/command attachment set, in
// one transaction (delete-then-insert), the same shape as SetTools. It is
// not tenant-scoped -- see the SkillStore.SetSkills doc comment on
// pkg/store for why (the caller verifies ownership before calling this).
func (s *agentProfileStore) SetSkills(ctx context.Context, profileID string, items []store.ProfileSkill) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: begin SetSkills tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var exists bool
	const checkQ = `SELECT EXISTS(SELECT 1 FROM agent_profiles WHERE id = $1 AND deleted_at IS NULL)`
	if err := tx.QueryRowContext(ctx, checkQ, profileID).Scan(&exists); err != nil {
		return fmt.Errorf("postgres: check profile exists: %w", err)
	}
	if !exists {
		return store.ErrNotFound
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM profile_skills WHERE agent_profile_id = $1`, profileID); err != nil {
		return fmt.Errorf("postgres: clear profile skills: %w", err)
	}

	const insertQ = `INSERT INTO profile_skills (agent_profile_id, skill_id, version) VALUES ($1, $2, $3)`
	for _, item := range items {
		var version sql.NullInt64
		if item.Version != nil {
			version = sql.NullInt64{Int64: int64(*item.Version), Valid: true}
		}
		if _, err := tx.ExecContext(ctx, insertQ, profileID, item.SkillID, version); err != nil {
			return mapWriteErr(err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: commit SetSkills tx: %w", err)
	}
	return nil
}

// GetSkills returns profileID's current skill/command attachments.
func (s *agentProfileStore) GetSkills(ctx context.Context, profileID string) ([]store.ProfileSkill, error) {
	const q = `SELECT agent_profile_id, skill_id, version FROM profile_skills WHERE agent_profile_id = $1 ORDER BY skill_id`
	rows, err := s.db.QueryContext(ctx, q, profileID)
	if err != nil {
		return nil, fmt.Errorf("postgres: get profile skills: %w", err)
	}
	defer rows.Close()

	var out []store.ProfileSkill
	for rows.Next() {
		var item store.ProfileSkill
		var version sql.NullInt64
		if err := rows.Scan(&item.AgentProfileID, &item.SkillID, &version); err != nil {
			return nil, fmt.Errorf("postgres: scan profile skill: %w", err)
		}
		if version.Valid {
			v := int(version.Int64)
			item.Version = &v
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

const agentProfileSelect = `
	SELECT id, tenant_id, name, slug, description, metadata, instructions, created_at, updated_at, deleted_at
	FROM agent_profiles`

func scanAgentProfileRow(row rowScanner) (*store.AgentProfile, error) {
	var p store.AgentProfile
	var meta []byte
	if err := row.Scan(&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Description, &meta, &p.Instructions, &p.CreatedAt, &p.UpdatedAt, &p.DeletedAt); err != nil {
		return nil, err
	}
	var err error
	if p.Metadata, err = unmarshalJSONB(meta); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal profile metadata: %w", err)
	}
	return &p, nil
}
