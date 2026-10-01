package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type modelStore struct{ db *sql.DB }

var _ store.ModelStore = (*modelStore)(nil)

// nullableTenant maps the store's "" = platform convention onto a NULL
// uuid bind parameter. Every tenant-scoped predicate below compares with
// IS NOT DISTINCT FROM so the same statement addresses a platform row
// (NULL) and a tenant row alike.
func nullableTenant(tenantID string) sql.NullString {
	return sql.NullString{String: tenantID, Valid: tenantID != ""}
}

func marshalTargets(t []store.ModelTarget) ([]byte, error) {
	if t == nil {
		t = []store.ModelTarget{}
	}
	return json.Marshal(t)
}

// marshalPrice renders a nil price as SQL NULL (the column is nullable;
// NULL means "use the rate card").
func marshalPrice(p *store.ModelPrice) (any, error) {
	if p == nil {
		return nil, nil
	}
	return json.Marshal(p)
}

// nullableCatalogModelID maps the store's "" = "not connected from the
// catalog" convention onto a NULL uuid bind parameter, matching
// nullableTenant's pattern for models.tenant_id.
func nullableCatalogModelID(id string) sql.NullString {
	return sql.NullString{String: id, Valid: id != ""}
}

// marshalCapabilities renders a nil map as SQL NULL (capabilities is
// nullable; NULL means "not connected from the catalog", distinct from an
// empty JSON object).
func marshalCapabilities(c map[string]any) (any, error) {
	if c == nil {
		return nil, nil
	}
	return json.Marshal(c)
}

func (s *modelStore) Create(ctx context.Context, m *store.Model) error {
	targets, err := marshalTargets(m.Targets)
	if err != nil {
		return fmt.Errorf("postgres: marshal model targets: %w", err)
	}
	price, err := marshalPrice(m.Price)
	if err != nil {
		return fmt.Errorf("postgres: marshal model price: %w", err)
	}
	meta, err := marshalJSONB(m.Metadata)
	if err != nil {
		return fmt.Errorf("postgres: marshal model metadata: %w", err)
	}
	limits, err := marshalLimits(m.Limits)
	if err != nil {
		return err
	}
	caps, err := marshalCapabilities(m.Capabilities)
	if err != nil {
		return fmt.Errorf("postgres: marshal model capabilities: %w", err)
	}

	const q = `
		INSERT INTO models (tenant_id, name, description, enabled, targets, price, metadata, limits, catalog_model_id, capabilities)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, created_at, updated_at`
	err = s.db.QueryRowContext(ctx, q, nullableTenant(m.TenantID), m.Name, m.Description, m.Enabled, targets, price, meta, limits,
		nullableCatalogModelID(m.CatalogModelID), caps).
		Scan(&m.ID, &m.CreatedAt, &m.UpdatedAt)
	return mapWriteErr(err)
}

// Get returns the row by id when it is the tenant's own or a platform row.
func (s *modelStore) Get(ctx context.Context, tenantID, id string) (*store.Model, error) {
	const q = modelSelect + ` WHERE id = $2 AND (tenant_id IS NOT DISTINCT FROM $1 OR tenant_id IS NULL) AND deleted_at IS NULL`
	m, err := scanModelRow(s.db.QueryRowContext(ctx, q, nullableTenant(tenantID), id))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return m, nil
}

// GetByName prefers the tenant's own row over the platform row of the
// same name (NULLS LAST on tenant_id puts the tenant row first).
func (s *modelStore) GetByName(ctx context.Context, tenantID, name string) (*store.Model, error) {
	const q = modelSelect + `
		WHERE name = $2 AND (tenant_id IS NOT DISTINCT FROM $1 OR tenant_id IS NULL) AND deleted_at IS NULL
		ORDER BY tenant_id NULLS LAST LIMIT 1`
	m, err := scanModelRow(s.db.QueryRowContext(ctx, q, nullableTenant(tenantID), name))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return m, nil
}

// List merges the tenant's rows with the platform rows; DISTINCT ON (name)
// with the same NULLS LAST ordering keeps the tenant's row when both exist.
func (s *modelStore) List(ctx context.Context, tenantID string, opts store.ListOptions) ([]*store.Model, error) {
	q := `SELECT DISTINCT ON (name) ` + modelColumns + `
		FROM models
		WHERE (tenant_id IS NOT DISTINCT FROM $1 OR tenant_id IS NULL) AND deleted_at IS NULL`
	if opts.EnabledOnly {
		q += ` AND enabled`
	}
	q += ` ORDER BY name, tenant_id NULLS LAST`
	rows, err := s.db.QueryContext(ctx, q, nullableTenant(tenantID))
	if err != nil {
		return nil, fmt.Errorf("postgres: list models: %w", err)
	}
	defer rows.Close()

	var out []*store.Model
	for rows.Next() {
		m, err := scanModelRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *modelStore) Update(ctx context.Context, m *store.Model) error {
	targets, err := marshalTargets(m.Targets)
	if err != nil {
		return fmt.Errorf("postgres: marshal model targets: %w", err)
	}
	price, err := marshalPrice(m.Price)
	if err != nil {
		return fmt.Errorf("postgres: marshal model price: %w", err)
	}
	meta, err := marshalJSONB(m.Metadata)
	if err != nil {
		return fmt.Errorf("postgres: marshal model metadata: %w", err)
	}
	limits, err := marshalLimits(m.Limits)
	if err != nil {
		return err
	}
	caps, err := marshalCapabilities(m.Capabilities)
	if err != nil {
		return fmt.Errorf("postgres: marshal model capabilities: %w", err)
	}

	// name is in the SET list so a rename persists; writing back an
	// unchanged name is a no-op for the unique index, and a collision
	// with another live row surfaces as store.ErrConflict, as on Create.
	// catalog_model_id/capabilities are written back from m too -- the
	// /models PUT handler round-trips them unchanged from its prior Get,
	// so a catalog-connected row's link and capabilities survive an
	// ordinary edit of its other fields.
	const q = `
		UPDATE models SET name = $3, description = $4, enabled = $5, targets = $6, price = $7, metadata = $8, limits = $9, catalog_model_id = $10, capabilities = $11, updated_at = now()
		WHERE tenant_id IS NOT DISTINCT FROM $1 AND id = $2 AND deleted_at IS NULL
		RETURNING updated_at`
	err = s.db.QueryRowContext(ctx, q, nullableTenant(m.TenantID), m.ID, m.Name, m.Description, m.Enabled, targets, price, meta, limits,
		nullableCatalogModelID(m.CatalogModelID), caps).
		Scan(&m.UpdatedAt)
	if err != nil {
		return mapWriteErr(mapReadErr(err))
	}
	return nil
}

func (s *modelStore) SoftDelete(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE models SET deleted_at = now(), updated_at = now() WHERE tenant_id IS NOT DISTINCT FROM $1 AND id = $2 AND deleted_at IS NULL`
	return execExpectingOneRow(ctx, s.db, q, nullableTenant(tenantID), id)
}

const modelColumns = `id, tenant_id, name, description, enabled, targets, price, metadata, limits, catalog_model_id, capabilities, created_at, updated_at, deleted_at`

const modelSelect = `SELECT ` + modelColumns + ` FROM models`

func scanModelRow(row rowScanner) (*store.Model, error) {
	var m store.Model
	var tenant, catalogModelID sql.NullString
	var targets, price, meta, limits, caps []byte
	if err := row.Scan(&m.ID, &tenant, &m.Name, &m.Description, &m.Enabled, &targets, &price, &meta, &limits, &catalogModelID, &caps, &m.CreatedAt, &m.UpdatedAt, &m.DeletedAt); err != nil {
		return nil, err
	}
	var err error
	if m.Limits, err = unmarshalLimits(limits); err != nil {
		return nil, err
	}
	m.TenantID = tenant.String // "" when NULL: a platform row
	m.CatalogModelID = catalogModelID.String
	if err := json.Unmarshal(targets, &m.Targets); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal model targets: %w", err)
	}
	if len(price) > 0 {
		var p store.ModelPrice
		if err := json.Unmarshal(price, &p); err != nil {
			return nil, fmt.Errorf("postgres: unmarshal model price: %w", err)
		}
		m.Price = &p
	}
	if m.Metadata, err = unmarshalJSONB(meta); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal model metadata: %w", err)
	}
	if len(caps) > 0 {
		var c map[string]any
		if err := json.Unmarshal(caps, &c); err != nil {
			return nil, fmt.Errorf("postgres: unmarshal model capabilities: %w", err)
		}
		m.Capabilities = c
	}
	return &m, nil
}
