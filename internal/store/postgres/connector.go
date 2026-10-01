package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type connectorStore struct{ db *sql.DB }

var _ store.ConnectorStore = (*connectorStore)(nil)

func (s *connectorStore) Create(ctx context.Context, c *store.Connector) error {
	caps, err := marshalJSONB(c.Capabilities)
	if err != nil {
		return fmt.Errorf("postgres: marshal connector capabilities: %w", err)
	}
	meta, err := marshalJSONB(c.Metadata)
	if err != nil {
		return fmt.Errorf("postgres: marshal connector metadata: %w", err)
	}
	if c.Status == "" {
		c.Status = "unknown"
	}
	if c.TimeoutMS == 0 {
		c.TimeoutMS = 30000
	}

	const q = `
		INSERT INTO connectors (tenant_id, name, slug, endpoint, timeout_ms, status, capabilities, metadata, catalog_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, '')::uuid)
		RETURNING id, created_at, updated_at`
	err = s.db.QueryRowContext(ctx, q, c.TenantID, c.Name, c.Slug, c.Endpoint, c.TimeoutMS, c.Status, caps, meta, c.CatalogID).
		Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt)
	return mapWriteErr(err)
}

func (s *connectorStore) Get(ctx context.Context, tenantID, id string) (*store.Connector, error) {
	const q = connectorSelect + ` WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	c, err := scanConnectorRow(s.db.QueryRowContext(ctx, q, tenantID, id))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return c, nil
}

func (s *connectorStore) GetBySlug(ctx context.Context, tenantID, slug string) (*store.Connector, error) {
	const q = connectorSelect + ` WHERE tenant_id = $1 AND slug = $2 AND deleted_at IS NULL`
	c, err := scanConnectorRow(s.db.QueryRowContext(ctx, q, tenantID, slug))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return c, nil
}

func (s *connectorStore) List(ctx context.Context, tenantID string) ([]*store.Connector, error) {
	const q = connectorSelect + ` WHERE tenant_id = $1 AND deleted_at IS NULL ORDER BY created_at`
	rows, err := s.db.QueryContext(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list connectors: %w", err)
	}
	defer rows.Close()

	var out []*store.Connector
	for rows.Next() {
		c, err := scanConnectorRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *connectorStore) Update(ctx context.Context, c *store.Connector) error {
	caps, err := marshalJSONB(c.Capabilities)
	if err != nil {
		return fmt.Errorf("postgres: marshal connector capabilities: %w", err)
	}
	meta, err := marshalJSONB(c.Metadata)
	if err != nil {
		return fmt.Errorf("postgres: marshal connector metadata: %w", err)
	}

	// slug is included in the SET list (not just name/endpoint/timeout_ms/
	// status/capabilities/metadata) so a caller-requested slug override
	// (internal/api/handlers.Connectors.Update) persists; a caller that
	// leaves it unchanged just writes back the same value it read via
	// Get, which is a no-op here and never trips the unique constraint on
	// its own row. A collision with a different connector's slug in the
	// same tenant surfaces as store.ErrConflict via mapWriteErr, same as
	// Create.
	const q = `
		UPDATE connectors SET name = $3, slug = $9, endpoint = $4, timeout_ms = $5, status = $6,
			capabilities = $7, metadata = $8, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
		RETURNING updated_at`
	err = s.db.QueryRowContext(ctx, q, c.TenantID, c.ID, c.Name, c.Endpoint, c.TimeoutMS, c.Status, caps, meta, c.Slug).
		Scan(&c.UpdatedAt)
	if err != nil {
		return mapWriteErr(mapReadErr(err))
	}
	return nil
}

func (s *connectorStore) SoftDelete(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE connectors SET deleted_at = now(), updated_at = now() WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	return execExpectingOneRow(ctx, s.db, q, tenantID, id)
}

const connectorSelect = `
	SELECT id, tenant_id, name, slug, endpoint, timeout_ms, status, capabilities, metadata, COALESCE(catalog_id::text, ''), created_at, updated_at, deleted_at
	FROM connectors`

func scanConnectorRow(row rowScanner) (*store.Connector, error) {
	var c store.Connector
	var caps, meta []byte
	if err := row.Scan(&c.ID, &c.TenantID, &c.Name, &c.Slug, &c.Endpoint, &c.TimeoutMS, &c.Status, &caps, &meta, &c.CatalogID, &c.CreatedAt, &c.UpdatedAt, &c.DeletedAt); err != nil {
		return nil, err
	}
	var err error
	if c.Capabilities, err = unmarshalJSONB(caps); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal connector capabilities: %w", err)
	}
	if c.Metadata, err = unmarshalJSONB(meta); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal connector metadata: %w", err)
	}
	return &c, nil
}
