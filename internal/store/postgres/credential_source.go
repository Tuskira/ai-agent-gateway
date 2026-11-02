package postgres

import (
	"context"
	"database/sql"
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
		INSERT INTO credential_sources (tenant_id, name, provider, config, enabled)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at, updated_at`
	err = s.db.QueryRowContext(ctx, q, src.TenantID, src.Name, src.Provider, cfg, src.Enabled).
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

func (s *credentialSourceStore) Update(ctx context.Context, src *store.CredentialSource) error {
	cfg, err := marshalJSONB(src.Config)
	if err != nil {
		return fmt.Errorf("postgres: marshal credential source config: %w", err)
	}
	const q = `
		UPDATE credential_sources SET name = $3, config = $4, enabled = $5, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
		RETURNING updated_at`
	err = s.db.QueryRowContext(ctx, q, src.TenantID, src.ID, src.Name, cfg, src.Enabled).Scan(&src.UpdatedAt)
	if err != nil {
		return mapWriteErr(mapReadErr(err))
	}
	return nil
}

func (s *credentialSourceStore) SoftDelete(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE credential_sources SET deleted_at = now(), updated_at = now() WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	return execExpectingOneRow(ctx, s.db, q, tenantID, id)
}

const credentialSourceSelect = `
	SELECT id, tenant_id, name, provider, config, enabled, created_at, updated_at, deleted_at
	FROM credential_sources`

func scanCredentialSourceRow(row rowScanner) (*store.CredentialSource, error) {
	var src store.CredentialSource
	var cfg []byte
	if err := row.Scan(&src.ID, &src.TenantID, &src.Name, &src.Provider, &cfg, &src.Enabled, &src.CreatedAt, &src.UpdatedAt, &src.DeletedAt); err != nil {
		return nil, err
	}
	var err error
	if src.Config, err = unmarshalJSONB(cfg); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal credential source config: %w", err)
	}
	return &src, nil
}
