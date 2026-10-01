package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type apiKeyStore struct{ db *sql.DB }

var _ store.APIKeyStore = (*apiKeyStore)(nil)

func (s *apiKeyStore) Create(ctx context.Context, k *store.APIKey) error {
	const q = `
		INSERT INTO api_keys (tenant_id, name, role, key_hash, key_prefix, expires_at, created_by, limits, profile_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at`
	lim, err := marshalLimits(k.Limits)
	if err != nil {
		return err
	}
	err = s.db.QueryRowContext(ctx, q, k.TenantID, k.Name, k.Role, k.KeyHash, k.KeyPrefix, k.ExpiresAt, k.CreatedBy, lim, k.ProfileID).
		Scan(&k.ID, &k.CreatedAt)
	return mapWriteErr(err)
}

func (s *apiKeyStore) GetByHash(ctx context.Context, keyHash string) (*store.APIKey, error) {
	const q = `
		SELECT id, tenant_id, name, role, key_hash, key_prefix, expires_at, revoked_at, last_used_at, created_by, created_at, limits, profile_id
		FROM api_keys WHERE key_hash = $1`
	k, err := scanAPIKeyRow(s.db.QueryRowContext(ctx, q, keyHash))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return k, nil
}

func (s *apiKeyStore) GetByID(ctx context.Context, tenantID, id string) (*store.APIKey, error) {
	const q = `
		SELECT id, tenant_id, name, role, key_hash, key_prefix, expires_at, revoked_at, last_used_at, created_by, created_at, limits, profile_id
		FROM api_keys WHERE tenant_id = $1 AND id = $2`
	k, err := scanAPIKeyRow(s.db.QueryRowContext(ctx, q, tenantID, id))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return k, nil
}

func (s *apiKeyStore) List(ctx context.Context, tenantID string) ([]*store.APIKey, error) {
	const q = `
		SELECT id, tenant_id, name, role, key_hash, key_prefix, expires_at, revoked_at, last_used_at, created_by, created_at, limits, profile_id
		FROM api_keys WHERE tenant_id = $1 ORDER BY created_at`
	rows, err := s.db.QueryContext(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list api keys: %w", err)
	}
	defer rows.Close()

	var out []*store.APIKey
	for rows.Next() {
		k, err := scanAPIKeyRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *apiKeyStore) Revoke(ctx context.Context, tenantID, id string) error {
	// One statement, so the UPDATE and the pg_notify commit (or roll back)
	// together: every gateway process LISTENing on keyRevokedChannel
	// evicts this key's cache entry the moment the revoke is visible.
	const q = `
		WITH revoked AS (
			UPDATE api_keys SET revoked_at = now()
			WHERE tenant_id = $1 AND id = $2
			RETURNING key_hash
		)
		SELECT count(pg_notify('` + keyRevokedChannel + `', key_hash)) FROM revoked`
	var n int
	if err := s.db.QueryRowContext(ctx, q, tenantID, id).Scan(&n); err != nil {
		return mapWriteErr(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *apiKeyStore) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	const q = `UPDATE api_keys SET last_used_at = $2 WHERE id = $1`
	return execExpectingOneRow(ctx, s.db, q, id, at)
}

func (s *apiKeyStore) SetLimits(ctx context.Context, tenantID, id string, l *store.Limits) error {
	const q = `UPDATE api_keys SET limits = $3 WHERE tenant_id = $1 AND id = $2`
	lim, err := marshalLimits(l)
	if err != nil {
		return err
	}
	return execExpectingOneRow(ctx, s.db, q, tenantID, id, lim)
}

func (s *apiKeyStore) SetProfile(ctx context.Context, tenantID, id string, profileID *string) error {
	const q = `UPDATE api_keys SET profile_id = $3 WHERE tenant_id = $1 AND id = $2`
	return execExpectingOneRow(ctx, s.db, q, tenantID, id, profileID)
}

func scanAPIKeyRow(row rowScanner) (*store.APIKey, error) {
	var k store.APIKey
	var lim []byte
	if err := row.Scan(
		&k.ID, &k.TenantID, &k.Name, &k.Role, &k.KeyHash, &k.KeyPrefix,
		&k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt, &k.CreatedBy, &k.CreatedAt, &lim, &k.ProfileID,
	); err != nil {
		return nil, err
	}
	var err error
	if k.Limits, err = unmarshalLimits(lim); err != nil {
		return nil, err
	}
	return &k, nil
}

// marshalLimits encodes l for a nullable limits JSONB column (api_keys,
// models): nil (SQL NULL) when l sets nothing, so "no limits" has exactly
// one stored form.
func marshalLimits(l *store.Limits) (any, error) {
	if l.Empty() {
		return nil, nil
	}
	b, err := json.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("postgres: encode limits: %w", err)
	}
	return string(b), nil
}

// unmarshalLimits decodes a limits column; NULL and an object that sets
// nothing are both nil.
func unmarshalLimits(raw []byte) (*store.Limits, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	l := &store.Limits{}
	if err := json.Unmarshal(raw, l); err != nil {
		return nil, fmt.Errorf("postgres: decode limits: %w", err)
	}
	if l.Empty() {
		return nil, nil
	}
	return l, nil
}

// execExpectingOneRow runs an UPDATE/DELETE and maps "zero rows affected"
// onto store.ErrNotFound.
func execExpectingOneRow(ctx context.Context, db *sql.DB, query string, args ...any) error {
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return mapWriteErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: rows affected: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
