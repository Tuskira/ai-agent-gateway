package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type tenantStore struct{ db *sql.DB }

var (
	_ store.TenantStore       = (*tenantStore)(nil)
	_ store.TenantModelPolicy = (*tenantStore)(nil)
)

func (s *tenantStore) Create(ctx context.Context, t *store.Tenant) error {
	settings, err := marshalJSONB(t.Settings)
	if err != nil {
		return fmt.Errorf("postgres: marshal tenant settings: %w", err)
	}

	const q = `
		INSERT INTO tenants (id, slug, name, settings, strict_models)
		VALUES (COALESCE(NULLIF($1::text, '')::uuid, gen_random_uuid()), $2, $3, $4, $5)
		RETURNING id, created_at`
	err = s.db.QueryRowContext(ctx, q, t.ID, t.Slug, t.Name, settings, t.StrictModels).Scan(&t.ID, &t.CreatedAt)
	return mapWriteErr(err)
}

func (s *tenantStore) GetByID(ctx context.Context, id string) (*store.Tenant, error) {
	// $1::text so a malformed id is a lookup miss (ErrNotFound), not a
	// uuid cast error; callers validate the shape before asking.
	const q = `SELECT id, slug, name, settings, strict_models, created_at FROM tenants WHERE id::text = $1`
	return scanTenant(s.db.QueryRowContext(ctx, q, id))
}

func (s *tenantStore) GetBySlug(ctx context.Context, slug string) (*store.Tenant, error) {
	const q = `SELECT id, slug, name, settings, strict_models, created_at FROM tenants WHERE slug = $1`
	return scanTenant(s.db.QueryRowContext(ctx, q, slug))
}

func (s *tenantStore) List(ctx context.Context) ([]*store.Tenant, error) {
	const q = `SELECT id, slug, name, settings, strict_models, created_at FROM tenants ORDER BY created_at`
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

func (s *tenantStore) UpdateName(ctx context.Context, id, name string) (*store.Tenant, error) {
	// id::text so a malformed id is a miss (ErrNotFound), as in GetByID.
	const q = `UPDATE tenants SET name = $2 WHERE id::text = $1
		RETURNING id, slug, name, settings, strict_models, created_at`
	return scanTenant(s.db.QueryRowContext(ctx, q, id, name))
}

// SetStrictModels sets the tenant's strict model policy.
func (s *tenantStore) SetStrictModels(ctx context.Context, id string, strict bool) (*store.Tenant, error) {
	// id::text so a malformed id is a miss (ErrNotFound), as in GetByID.
	const q = `UPDATE tenants SET strict_models = $2 WHERE id::text = $1
		RETURNING id, slug, name, settings, strict_models, created_at`
	return scanTenant(s.db.QueryRowContext(ctx, q, id, strict))
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
	if err := row.Scan(&t.ID, &t.Slug, &t.Name, &settings, &t.StrictModels, &t.CreatedAt); err != nil {
		return nil, err
	}
	m, err := unmarshalJSONB(settings)
	if err != nil {
		return nil, fmt.Errorf("postgres: unmarshal tenant settings: %w", err)
	}
	t.Settings = m
	return &t, nil
}
