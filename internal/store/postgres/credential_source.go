package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type credentialSourceStore struct{ db *sql.DB }

var _ store.CredentialSourceStore = (*credentialSourceStore)(nil)

func (s *credentialSourceStore) Create(ctx context.Context, src *store.CredentialSource) error {
	cfg, err := marshalJSONB(src.Config)
	if err != nil {
		return fmt.Errorf("postgres: marshal credential source config: %w", err)
	}
	const q = `
		INSERT INTO credential_sources (tenant_id, name, provider, config, enabled, locked)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at, updated_at`
	err = s.db.QueryRowContext(ctx, q, src.TenantID, src.Name, src.Provider, cfg, src.Enabled, src.Locked).
		Scan(&src.ID, &src.CreatedAt, &src.UpdatedAt)
	return mapWriteErr(err)
}

func (s *credentialSourceStore) Get(ctx context.Context, tenantID, id string) (*store.CredentialSource, error) {
	const q = credentialSourceSelect + ` WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	src, err := scanCredentialSourceRow(s.db.QueryRowContext(ctx, q, tenantID, id))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return src, nil
}

func (s *credentialSourceStore) GetByName(ctx context.Context, tenantID, name string) (*store.CredentialSource, error) {
	const q = credentialSourceSelect + ` WHERE tenant_id = $1 AND name = $2 AND deleted_at IS NULL`
	src, err := scanCredentialSourceRow(s.db.QueryRowContext(ctx, q, tenantID, name))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return src, nil
}

func (s *credentialSourceStore) List(ctx context.Context, tenantID string) ([]*store.CredentialSource, error) {
	const q = credentialSourceSelect + ` WHERE tenant_id = $1 AND deleted_at IS NULL ORDER BY name`
	rows, err := s.db.QueryContext(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list credential sources: %w", err)
	}
	defer rows.Close()

	var out []*store.CredentialSource
	for rows.Next() {
		src, err := scanCredentialSourceRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// Update never writes locked (SetLocked does), so a lock set by a platform
// admin between a caller's read and this write is never cleared. Update and
// SoftDelete carry the lock check in their WHERE clause ($n OR NOT locked);
// lockedOrMissing then tells ErrLocked from ErrNotFound.
func (s *credentialSourceStore) Update(ctx context.Context, src *store.CredentialSource, guard store.LockGuard) error {
	cfg, err := marshalJSONB(src.Config)
	if err != nil {
		return fmt.Errorf("postgres: marshal credential source config: %w", err)
	}
	const q = `
		UPDATE credential_sources SET name = $3, config = $4, enabled = $5, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL AND ($6 OR NOT locked)
		RETURNING updated_at`
	err = s.db.QueryRowContext(ctx, q, src.TenantID, src.ID, src.Name, cfg, src.Enabled, bool(guard)).Scan(&src.UpdatedAt)
	if err != nil {
		return s.lockedOrMissing(ctx, src.TenantID, src.ID, mapWriteErr(mapReadErr(err)))
	}
	return nil
}

func (s *credentialSourceStore) SoftDelete(ctx context.Context, tenantID, id string, guard store.LockGuard) error {
	const q = `UPDATE credential_sources SET deleted_at = now(), updated_at = now() WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL AND ($3 OR NOT locked)`
	err := execExpectingOneRow(ctx, s.db, q, tenantID, id, bool(guard))
	return s.lockedOrMissing(ctx, tenantID, id, err)
}

func (s *credentialSourceStore) UpdateLocked(ctx context.Context, src *store.CredentialSource) error {
	cfg, err := marshalJSONB(src.Config)
	if err != nil {
		return fmt.Errorf("postgres: marshal credential source config: %w", err)
	}
	const q = `
		UPDATE credential_sources SET name = $3, config = $4, enabled = $5, locked = true, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
		RETURNING updated_at`
	if err := s.db.QueryRowContext(ctx, q, src.TenantID, src.ID, src.Name, cfg, src.Enabled).Scan(&src.UpdatedAt); err != nil {
		return mapWriteErr(mapReadErr(err))
	}
	src.Locked = true
	return nil
}

func (s *credentialSourceStore) SetLocked(ctx context.Context, tenantID, id string, locked bool) error {
	const q = `UPDATE credential_sources SET locked = $3, updated_at = now() WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	return execExpectingOneRow(ctx, s.db, q, tenantID, id, locked)
}

// lockedOrMissing turns a guarded write's ErrNotFound into ErrLocked when
// the live row exists and is locked. Any other err is returned unchanged.
func (s *credentialSourceStore) lockedOrMissing(ctx context.Context, tenantID, id string, err error) error {
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	var locked bool
	qerr := s.db.QueryRowContext(ctx, `SELECT locked FROM credential_sources WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, id).Scan(&locked)
	if qerr == nil && locked {
		return store.ErrLocked
	}
	return err
}

const credentialSourceSelect = `
	SELECT id, tenant_id, name, provider, config, enabled, locked, created_at, updated_at, deleted_at
	FROM credential_sources`

func scanCredentialSourceRow(row rowScanner) (*store.CredentialSource, error) {
	var src store.CredentialSource
	var cfg []byte
	if err := row.Scan(&src.ID, &src.TenantID, &src.Name, &src.Provider, &cfg, &src.Enabled, &src.Locked, &src.CreatedAt, &src.UpdatedAt, &src.DeletedAt); err != nil {
		return nil, err
	}
	var err error
	if src.Config, err = unmarshalJSONB(cfg); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal credential source config: %w", err)
	}
	return &src, nil
}
