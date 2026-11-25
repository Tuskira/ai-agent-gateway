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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: begin create connector: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockConnectorNames(ctx, tx, c.TenantID); err != nil {
		return err
	}
	if err := checkConnectorNames(ctx, tx, c); err != nil {
		return err
	}

	const q = `
		INSERT INTO connectors (tenant_id, name, slug, endpoint, timeout_ms, status, capabilities, metadata, catalog_id, tool_prefix)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, '')::uuid, NULLIF($10, ''))
		RETURNING id, created_at, updated_at`
	err = tx.QueryRowContext(ctx, q, c.TenantID, c.Name, c.Slug, c.Endpoint, c.TimeoutMS, c.Status, caps, meta, c.CatalogID, c.ToolPrefix).
		Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return mapWriteErr(err)
	}
	if err := tx.Commit(); err != nil {
		return mapWriteErr(err)
	}
	return nil
}

// lockConnectorNames serializes every write that can change a tenant's
// connector slugs or tool prefixes, so the collision check in
// checkConnectorNames and the write that follows it are atomic: two
// concurrent writers cannot both pass the check. It is transaction-scoped
// and released at commit or rollback.
func lockConnectorNames(ctx context.Context, tx *sql.Tx, tenantID string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('connector_names:' || $1::text))`, tenantID); err != nil {
		return fmt.Errorf("postgres: lock connector names: %w", err)
	}
	return nil
}

// checkConnectorNames returns ErrConflict when c's tool prefix equals
// (case-insensitively) another live connector's slug, or c's slug equals
// another live connector's tool prefix. Prefix against prefix is the
// unique index's job. The caller holds lockConnectorNames.
func checkConnectorNames(ctx context.Context, tx *sql.Tx, c *store.Connector) error {
	const q = `
		SELECT EXISTS (
			SELECT 1 FROM connectors
			WHERE tenant_id = $1 AND deleted_at IS NULL AND id::text <> $2
			  AND (($3 <> '' AND lower(slug) = lower($3))
			    OR ($4 <> '' AND lower(tool_prefix) = lower($4)))
		)`
	var clash bool
	if err := tx.QueryRowContext(ctx, q, c.TenantID, c.ID, c.ToolPrefix, c.Slug).Scan(&clash); err != nil {
		return fmt.Errorf("postgres: check connector names: %w", mapReadErr(err))
	}
	if clash {
		return store.ErrConflict
	}
	return nil
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

func (s *connectorStore) GetByToolPrefix(ctx context.Context, tenantID, prefix string) (*store.Connector, error) {
	const q = connectorSelect + ` WHERE tenant_id = $1 AND lower(tool_prefix) = lower($2) AND deleted_at IS NULL`
	c, err := scanConnectorRow(s.db.QueryRowContext(ctx, q, tenantID, prefix))
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: begin update connector: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockConnectorNames(ctx, tx, c.TenantID); err != nil {
		return err
	}
	if err := checkConnectorNames(ctx, tx, c); err != nil {
		return err
	}

	const q = `
		UPDATE connectors SET name = $3, slug = $9, endpoint = $4, timeout_ms = $5, status = $6,
			capabilities = $7, metadata = $8, tool_prefix = NULLIF($10, ''), updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
		RETURNING updated_at`
	err = tx.QueryRowContext(ctx, q, c.TenantID, c.ID, c.Name, c.Endpoint, c.TimeoutMS, c.Status, caps, meta, c.Slug, c.ToolPrefix).
		Scan(&c.UpdatedAt)
	if err != nil {
		return mapWriteErr(mapReadErr(err))
	}
	if err := tx.Commit(); err != nil {
		return mapWriteErr(err)
	}
	return nil
}

func (s *connectorStore) SoftDelete(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE connectors SET deleted_at = now(), updated_at = now() WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`
	return execExpectingOneRow(ctx, s.db, q, tenantID, id)
}

const connectorSelect = `
	SELECT id, tenant_id, name, slug, COALESCE(tool_prefix, ''), endpoint, timeout_ms, status, capabilities, metadata, COALESCE(catalog_id::text, ''), created_at, updated_at, deleted_at
	FROM connectors`

func scanConnectorRow(row rowScanner) (*store.Connector, error) {
	var c store.Connector
	var caps, meta []byte
	if err := row.Scan(&c.ID, &c.TenantID, &c.Name, &c.Slug, &c.ToolPrefix, &c.Endpoint, &c.TimeoutMS, &c.Status, &caps, &meta, &c.CatalogID, &c.CreatedAt, &c.UpdatedAt, &c.DeletedAt); err != nil {
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
