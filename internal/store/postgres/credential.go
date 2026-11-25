package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type credentialStore struct{ db *sql.DB }

var _ store.CredentialStore = (*credentialStore)(nil)

// pgTypeMap adapts Postgres array results (e.g. credentials.field_names,
// a text[]) onto Go slices when scanned through database/sql; see
// TestConnQueryScanGoArray in pgx/v5/stdlib for why this wrapper is needed
// on read (writes accept a plain []string as a bind parameter directly).
var pgTypeMap = pgtype.NewMap()

func (s *credentialStore) Create(ctx context.Context, c *store.Credential) error {
	fieldNames := c.FieldNames
	if fieldNames == nil {
		// field_names is NOT NULL; an explicit NULL bind param bypasses
		// the column's DEFAULT '{}', so normalize nil -> empty here.
		fieldNames = []string{}
	}

	const q = `
		INSERT INTO credentials (tenant_id, name, type, ciphertext, nonce, key_id, field_names, created_by, locked)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at`
	err := s.db.QueryRowContext(ctx, q,
		c.TenantID, c.Name, c.Type, c.Ciphertext, c.Nonce, c.KeyID, fieldNames, c.CreatedBy, c.Locked,
	).Scan(&c.ID, &c.CreatedAt)
	return mapWriteErr(err)
}

func (s *credentialStore) Get(ctx context.Context, tenantID, name string) (*store.Credential, error) {
	const q = `
		SELECT id, tenant_id, name, type, ciphertext, nonce, key_id, field_names, created_by, created_at, rotated_at, locked
		FROM credentials WHERE tenant_id = $1 AND name = $2`
	c, err := scanCredentialRow(s.db.QueryRowContext(ctx, q, tenantID, name), true)
	if err != nil {
		return nil, mapReadErr(err)
	}
	return c, nil
}

// List returns metadata only: Ciphertext and Nonce are never populated.
func (s *credentialStore) List(ctx context.Context, tenantID string) ([]*store.Credential, error) {
	const q = `
		SELECT id, tenant_id, name, type, key_id, field_names, created_by, created_at, rotated_at, locked
		FROM credentials WHERE tenant_id = $1 ORDER BY created_at`
	rows, err := s.db.QueryContext(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list credentials: %w", err)
	}
	defer rows.Close()

	var out []*store.Credential
	for rows.Next() {
		var c store.Credential
		if err := rows.Scan(&c.ID, &c.TenantID, &c.Name, &c.Type, &c.KeyID, pgTypeMap.SQLScanner(&c.FieldNames), &c.CreatedBy, &c.CreatedAt, &c.RotatedAt, &c.Locked); err != nil {
			return nil, fmt.Errorf("postgres: scan credential: %w", err)
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// Rotate and Delete carry the lock check in their WHERE clause ($n OR NOT
// locked), so a lock set after the caller's own read still stops the write;
// lockedOrMissing then tells ErrLocked from ErrNotFound.
func (s *credentialStore) Rotate(ctx context.Context, tenantID, name string, ciphertext, nonce []byte, keyID string, fieldNames []string, guard store.LockGuard) error {
	const q = `
		UPDATE credentials SET ciphertext = $3, nonce = $4, key_id = $5, field_names = $6, rotated_at = now()
		WHERE tenant_id = $1 AND name = $2 AND ($7 OR NOT locked)`
	err := execExpectingOneRow(ctx, s.db, q, tenantID, name, ciphertext, nonce, keyID, fieldNames, bool(guard))
	return s.lockedOrMissing(ctx, tenantID, name, err)
}

func (s *credentialStore) Delete(ctx context.Context, tenantID, name string, guard store.LockGuard) error {
	const q = `DELETE FROM credentials WHERE tenant_id = $1 AND name = $2 AND ($3 OR NOT locked)`
	err := execExpectingOneRow(ctx, s.db, q, tenantID, name, bool(guard))
	return s.lockedOrMissing(ctx, tenantID, name, err)
}

// lockedOrMissing turns a guarded write's ErrNotFound into ErrLocked when
// the row exists and is locked. Any other err is returned unchanged.
func (s *credentialStore) lockedOrMissing(ctx context.Context, tenantID, name string, err error) error {
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	var locked bool
	qerr := s.db.QueryRowContext(ctx, `SELECT locked FROM credentials WHERE tenant_id = $1 AND name = $2`, tenantID, name).Scan(&locked)
	if qerr == nil && locked {
		return store.ErrLocked
	}
	return err
}

func (s *credentialStore) SetLocked(ctx context.Context, tenantID, name string, locked bool) error {
	const q = `UPDATE credentials SET locked = $3 WHERE tenant_id = $1 AND name = $2`
	return execExpectingOneRow(ctx, s.db, q, tenantID, name, locked)
}

func scanCredentialRow(row rowScanner, withSecret bool) (*store.Credential, error) {
	var c store.Credential
	var err error
	if withSecret {
		err = row.Scan(&c.ID, &c.TenantID, &c.Name, &c.Type, &c.Ciphertext, &c.Nonce, &c.KeyID, pgTypeMap.SQLScanner(&c.FieldNames), &c.CreatedBy, &c.CreatedAt, &c.RotatedAt, &c.Locked)
	} else {
		err = row.Scan(&c.ID, &c.TenantID, &c.Name, &c.Type, &c.KeyID, pgTypeMap.SQLScanner(&c.FieldNames), &c.CreatedBy, &c.CreatedAt, &c.RotatedAt, &c.Locked)
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}
