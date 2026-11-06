package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type userIdentityStore struct{ db *sql.DB }

var _ store.UserIdentityStore = (*userIdentityStore)(nil)

const identityColumns = `id, tenant_id, provider, issuer, subject, user_id, email, created_at, last_login_at`

func scanIdentityRow(row rowScanner) (*store.UserIdentity, error) {
	var i store.UserIdentity
	if err := row.Scan(&i.ID, &i.TenantID, &i.Provider, &i.Issuer, &i.Subject, &i.UserID, &i.Email, &i.CreatedAt, &i.LastLoginAt); err != nil {
		return nil, err
	}
	return &i, nil
}

func (s *userIdentityStore) Create(ctx context.Context, i *store.UserIdentity) error {
	const q = `
		INSERT INTO user_identities (tenant_id, provider, issuer, subject, user_id, email)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at, last_login_at`
	err := s.db.QueryRowContext(ctx, q, i.TenantID, i.Provider, i.Issuer, i.Subject, i.UserID, i.Email).
		Scan(&i.ID, &i.CreatedAt, &i.LastLoginAt)
	return mapWriteErr(err)
}

func (s *userIdentityStore) GetBySubject(ctx context.Context, tenantID, provider, issuer, subject string) (*store.UserIdentity, error) {
	const q = `SELECT ` + identityColumns + ` FROM user_identities
		WHERE tenant_id = $1 AND provider = $2 AND issuer = $3 AND subject = $4`
	i, err := scanIdentityRow(s.db.QueryRowContext(ctx, q, tenantID, provider, issuer, subject))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return i, nil
}

func (s *userIdentityStore) GetByUser(ctx context.Context, tenantID, provider, issuer, userID string) (*store.UserIdentity, error) {
	const q = `SELECT ` + identityColumns + ` FROM user_identities
		WHERE tenant_id = $1 AND provider = $2 AND issuer = $3 AND user_id = $4`
	i, err := scanIdentityRow(s.db.QueryRowContext(ctx, q, tenantID, provider, issuer, userID))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return i, nil
}

func (s *userIdentityStore) ListByUser(ctx context.Context, tenantID, userID string) ([]*store.UserIdentity, error) {
	const q = `SELECT ` + identityColumns + ` FROM user_identities
		WHERE tenant_id = $1 AND user_id = $2 ORDER BY provider, issuer`
	rows, err := s.db.QueryContext(ctx, q, tenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list user identities: %w", err)
	}
	defer rows.Close()

	out := []*store.UserIdentity{}
	for rows.Next() {
		i, err := scanIdentityRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *userIdentityStore) RecordLogin(ctx context.Context, tenantID, id, email string, at time.Time) error {
	const q = `UPDATE user_identities SET last_login_at = $3, email = $4 WHERE tenant_id = $1 AND id = $2`
	return execExpectingOneRow(ctx, s.db, q, tenantID, id, at, email)
}

func (s *userIdentityStore) Delete(ctx context.Context, tenantID, id string) error {
	const q = `DELETE FROM user_identities WHERE tenant_id = $1 AND id = $2`
	return execExpectingOneRow(ctx, s.db, q, tenantID, id)
}
