package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/skills"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type skillStore struct{ db *sql.DB }

var _ store.SkillStore = (*skillStore)(nil)

func marshalArguments(args []store.CommandArgument) ([]byte, error) {
	if args == nil {
		args = []store.CommandArgument{}
	}
	return json.Marshal(args)
}

func unmarshalArguments(raw []byte) ([]store.CommandArgument, error) {
	if len(raw) == 0 {
		return []store.CommandArgument{}, nil
	}
	args := []store.CommandArgument{}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	return args, nil
}

func marshalSkillFiles(files []store.SkillFile) ([]byte, error) {
	if files == nil {
		files = []store.SkillFile{}
	}
	return json.Marshal(files)
}

// Create persists sk and its version-1 files in one transaction. sk's
// Description and Frontmatter are taken as given -- see the SkillStore
// interface doc comment for why Create, unlike AddVersion, does not
// re-derive them from SKILL.md itself.
func (s *skillStore) Create(ctx context.Context, sk *store.Skill, files []store.SkillFile, createdBy string) error {
	fm, err := marshalJSONB(sk.Frontmatter)
	if err != nil {
		return fmt.Errorf("postgres: marshal skill frontmatter: %w", err)
	}
	args, err := marshalArguments(sk.Arguments)
	if err != nil {
		return fmt.Errorf("postgres: marshal skill arguments: %w", err)
	}
	meta, err := marshalJSONB(sk.Metadata)
	if err != nil {
		return fmt.Errorf("postgres: marshal skill metadata: %w", err)
	}
	filesJSON, err := marshalSkillFiles(files)
	if err != nil {
		return fmt.Errorf("postgres: marshal skill files: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: begin create skill tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	const insertSkillQ = `
		INSERT INTO skills (tenant_id, name, kind, description, frontmatter, arguments, latest_version, enabled, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $8)
		RETURNING id, created_at, updated_at`
	if err := tx.QueryRowContext(ctx, insertSkillQ, nullableTenant(sk.TenantID), sk.Name, sk.Kind, sk.Description, fm, args, sk.Enabled, meta).
		Scan(&sk.ID, &sk.CreatedAt, &sk.UpdatedAt); err != nil {
		return mapWriteErr(err)
	}
	sk.LatestVersion = 1

	const insertVersionQ = `INSERT INTO skill_versions (skill_id, version, files, created_by) VALUES ($1, 1, $2, $3)`
	if _, err := tx.ExecContext(ctx, insertVersionQ, sk.ID, filesJSON, createdBy); err != nil {
		return fmt.Errorf("postgres: insert skill version 1: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: commit create skill tx: %w", err)
	}
	return nil
}

func (s *skillStore) Get(ctx context.Context, tenantID, id string) (*store.Skill, error) {
	const q = skillSelect + ` WHERE id = $2 AND (tenant_id IS NOT DISTINCT FROM $1 OR tenant_id IS NULL) AND deleted_at IS NULL`
	sk, err := scanSkillRow(s.db.QueryRowContext(ctx, q, nullableTenant(tenantID), id))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return sk, nil
}

func (s *skillStore) GetByName(ctx context.Context, tenantID, name string) (*store.Skill, error) {
	const q = skillSelect + `
		WHERE name = $2 AND (tenant_id IS NOT DISTINCT FROM $1 OR tenant_id IS NULL) AND deleted_at IS NULL
		ORDER BY tenant_id NULLS LAST LIMIT 1`
	sk, err := scanSkillRow(s.db.QueryRowContext(ctx, q, nullableTenant(tenantID), name))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return sk, nil
}

func (s *skillStore) List(ctx context.Context, tenantID string, opts store.SkillListOptions) ([]store.Skill, int, error) {
	q := `SELECT DISTINCT ON (name) ` + skillColumns + `
		FROM skills
		WHERE (tenant_id IS NOT DISTINCT FROM $1 OR tenant_id IS NULL) AND deleted_at IS NULL`
	args := []any{nullableTenant(tenantID)}
	if opts.Kind != "" {
		q += ` AND kind = $2`
		args = append(args, opts.Kind)
	}
	q += ` ORDER BY name, tenant_id NULLS LAST`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("postgres: list skills: %w", err)
	}
	defer rows.Close()

	var out []store.Skill
	for rows.Next() {
		sk, err := scanSkillRow(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *sk)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, len(out), nil
}

// Update rewrites description, enabled, metadata and arguments only --
// name, kind and frontmatter are immutable outside of AddVersion.
func (s *skillStore) Update(ctx context.Context, sk *store.Skill) error {
	meta, err := marshalJSONB(sk.Metadata)
	if err != nil {
		return fmt.Errorf("postgres: marshal skill metadata: %w", err)
	}
	args, err := marshalArguments(sk.Arguments)
	if err != nil {
		return fmt.Errorf("postgres: marshal skill arguments: %w", err)
	}

	const q = `
		UPDATE skills SET description = $3, enabled = $4, metadata = $5, arguments = $6, updated_at = now()
		WHERE tenant_id IS NOT DISTINCT FROM $1 AND id = $2 AND deleted_at IS NULL
		RETURNING updated_at`
	err = s.db.QueryRowContext(ctx, q, nullableTenant(sk.TenantID), sk.ID, sk.Description, sk.Enabled, meta, args).
		Scan(&sk.UpdatedAt)
	if err != nil {
		return mapWriteErr(mapReadErr(err))
	}
	return nil
}

func (s *skillStore) SoftDelete(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE skills SET deleted_at = now(), updated_at = now() WHERE tenant_id IS NOT DISTINCT FROM $1 AND id = $2 AND deleted_at IS NULL`
	return execExpectingOneRow(ctx, s.db, q, nullableTenant(tenantID), id)
}

// AddVersion appends a new SkillVersion and refreshes the row's
// description/frontmatter from the new files' SKILL.md, all inside one
// transaction so a concurrent AddVersion never races past this one's
// SELECT ... FOR UPDATE.
func (s *skillStore) AddVersion(ctx context.Context, tenantID, id string, files []store.SkillFile, createdBy string) (*store.SkillVersion, error) {
	filesJSON, err := marshalSkillFiles(files)
	if err != nil {
		return nil, fmt.Errorf("postgres: marshal skill files: %w", err)
	}
	skillMD, err := skills.FindSkillMD(files)
	if err != nil {
		return nil, fmt.Errorf("postgres: add skill version: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("postgres: begin add skill version tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var name string
	var latest int
	const selectQ = `
		SELECT name, latest_version FROM skills
		WHERE id = $2 AND (tenant_id IS NOT DISTINCT FROM $1 OR tenant_id IS NULL) AND deleted_at IS NULL
		FOR UPDATE`
	if err := tx.QueryRowContext(ctx, selectQ, nullableTenant(tenantID), id).Scan(&name, &latest); err != nil {
		return nil, mapReadErr(err)
	}

	fm, err := skills.ParseFrontmatter(skillMD, name)
	if err != nil {
		return nil, fmt.Errorf("postgres: add skill version: %w", err)
	}
	fmJSON, err := marshalJSONB(fm)
	if err != nil {
		return nil, fmt.Errorf("postgres: marshal refreshed skill frontmatter: %w", err)
	}
	description := skills.DescriptionFrom(fm)

	newVersion := latest + 1
	const updateSkillQ = `
		UPDATE skills SET latest_version = $3, description = $4, frontmatter = $5, updated_at = now()
		WHERE id = $2 AND (tenant_id IS NOT DISTINCT FROM $1 OR tenant_id IS NULL) AND deleted_at IS NULL`
	if _, err := tx.ExecContext(ctx, updateSkillQ, nullableTenant(tenantID), id, newVersion, description, fmJSON); err != nil {
		return nil, fmt.Errorf("postgres: bump skill latest_version: %w", err)
	}

	var createdAt time.Time
	const insertVersionQ = `INSERT INTO skill_versions (skill_id, version, files, created_by) VALUES ($1, $2, $3, $4) RETURNING created_at`
	if err := tx.QueryRowContext(ctx, insertVersionQ, id, newVersion, filesJSON, createdBy).Scan(&createdAt); err != nil {
		return nil, mapWriteErr(err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("postgres: commit add skill version tx: %w", err)
	}

	return &store.SkillVersion{SkillID: id, Version: newVersion, Files: files, CreatedBy: createdBy, CreatedAt: createdAt}, nil
}

func (s *skillStore) GetVersion(ctx context.Context, skillID string, version int) (*store.SkillVersion, error) {
	const q = `SELECT skill_id, version, files, created_by, created_at FROM skill_versions WHERE skill_id = $1 AND version = $2`
	var v store.SkillVersion
	var filesJSON []byte
	err := s.db.QueryRowContext(ctx, q, skillID, version).Scan(&v.SkillID, &v.Version, &filesJSON, &v.CreatedBy, &v.CreatedAt)
	if err != nil {
		return nil, mapReadErr(err)
	}
	if err := json.Unmarshal(filesJSON, &v.Files); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal skill version files: %w", err)
	}
	return &v, nil
}

// ListVersions returns every version, newest first, with Files left nil
// (a listing doesn't need file bodies -- see GetVersion).
func (s *skillStore) ListVersions(ctx context.Context, skillID string) ([]store.SkillVersion, error) {
	const q = `SELECT skill_id, version, created_by, created_at FROM skill_versions WHERE skill_id = $1 ORDER BY version DESC`
	rows, err := s.db.QueryContext(ctx, q, skillID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list skill versions: %w", err)
	}
	defer rows.Close()

	var out []store.SkillVersion
	for rows.Next() {
		var v store.SkillVersion
		if err := rows.Scan(&v.SkillID, &v.Version, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan skill version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

const skillColumns = `id, tenant_id, name, kind, description, frontmatter, arguments, latest_version, enabled, metadata, created_at, updated_at, deleted_at`

const skillSelect = `SELECT ` + skillColumns + ` FROM skills`

func scanSkillRow(row rowScanner) (*store.Skill, error) {
	var sk store.Skill
	var tenant sql.NullString
	var fm, args, meta []byte
	if err := row.Scan(&sk.ID, &tenant, &sk.Name, &sk.Kind, &sk.Description, &fm, &args, &sk.LatestVersion, &sk.Enabled, &meta, &sk.CreatedAt, &sk.UpdatedAt, &sk.DeletedAt); err != nil {
		return nil, err
	}
	sk.TenantID = tenant.String
	var err error
	if sk.Frontmatter, err = unmarshalJSONB(fm); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal skill frontmatter: %w", err)
	}
	if sk.Arguments, err = unmarshalArguments(args); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal skill arguments: %w", err)
	}
	if sk.Metadata, err = unmarshalJSONB(meta); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal skill metadata: %w", err)
	}
	return &sk, nil
}
