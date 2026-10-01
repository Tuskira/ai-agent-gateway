package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type tenantStore struct{ db *sql.DB }

var _ store.TenantStore = (*tenantStore)(nil)

func (s *tenantStore) Create(ctx context.Context, t *store.Tenant) error {
	settings, err := marshalJSONB(t.Settings)
	if err != nil {
		return fmt.Errorf("postgres: marshal tenant settings: %w", err)
	}

	const q = `
		INSERT INTO tenants (slug, name, settings)
		VALUES ($1, $2, $3)
		RETURNING id, created_at`
	err = s.db.QueryRowContext(ctx, q, t.Slug, t.Name, settings).Scan(&t.ID, &t.CreatedAt)
	return mapWriteErr(err)
}

func (s *tenantStore) GetBySlug(ctx context.Context, slug string) (*store.Tenant, error) {
	const q = `SELECT id, slug, name, settings, created_at FROM tenants WHERE slug = $1`
	return scanTenant(s.db.QueryRowContext(ctx, q, slug))
}

func (s *tenantStore) List(ctx context.Context) ([]*store.Tenant, error) {
	const q = `SELECT id, slug, name, settings, created_at FROM tenants ORDER BY created_at`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("postgres: list tenants: %w", err)
	}
	defer rows.Close()

	var out []*store.Tenant
	for rows.Next() {
		t, err := scanTenantRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTenant(row rowScanner) (*store.Tenant, error) {
	t, err := scanTenantRow(row)
	if err != nil {
		return nil, mapReadErr(err)
	}
	return t, nil
}

func scanTenantRow(row rowScanner) (*store.Tenant, error) {
	var t store.Tenant
	var settings []byte
	if err := row.Scan(&t.ID, &t.Slug, &t.Name, &settings, &t.CreatedAt); err != nil {
		return nil, err
	}
	m, err := unmarshalJSONB(settings)
	if err != nil {
		return nil, fmt.Errorf("postgres: unmarshal tenant settings: %w", err)
	}
	t.Settings = m
	return &t, nil
}
