package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type toolCacheStore struct{ db *sql.DB }

var _ store.ToolCacheStore = (*toolCacheStore)(nil)

// Upsert inserts or updates each tool, keyed by its natural identity
// (tenant_id, connector_id, tool_namespace, tool_name).
func (s *toolCacheStore) Upsert(ctx context.Context, tools ...*store.CachedTool) error {
	if len(tools) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: begin Upsert tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	const q = `
		INSERT INTO tool_cache (tenant_id, connector_id, tool_namespace, tool_name, description, input_schema, cached_at, expires_at, is_stale)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (tenant_id, connector_id, tool_namespace, tool_name)
		DO UPDATE SET description = EXCLUDED.description, input_schema = EXCLUDED.input_schema,
			cached_at = EXCLUDED.cached_at, expires_at = EXCLUDED.expires_at, is_stale = EXCLUDED.is_stale`

	for _, ct := range tools {
		schema, err := marshalJSONB(ct.InputSchema)
		if err != nil {
			return fmt.Errorf("postgres: marshal tool input_schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, q, ct.TenantID, ct.ConnectorID, ct.ToolNamespace, ct.ToolName, ct.Description, schema, ct.CachedAt, ct.ExpiresAt, ct.Stale); err != nil {
			return mapWriteErr(err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: commit Upsert tx: %w", err)
	}
	return nil
}

func (s *toolCacheStore) ListByConnector(ctx context.Context, tenantID, connectorID string) ([]*store.CachedTool, error) {
	const q = toolCacheSelect + ` WHERE tenant_id = $1 AND connector_id = $2 AND ` + connectorNotDeleted + ` ORDER BY tool_namespace, tool_name`
	return s.query(ctx, q, tenantID, connectorID)
}

func (s *toolCacheStore) ListByTenant(ctx context.Context, tenantID string) ([]*store.CachedTool, error) {
	const q = toolCacheSelect + ` WHERE tenant_id = $1 AND ` + connectorNotDeleted + ` ORDER BY connector_id, tool_namespace, tool_name`
	return s.query(ctx, q, tenantID)
}

// Search matches on the generated tsvector (tool_name/namespace/
// description) OR a plain ILIKE substring, so partial/mid-word queries
// that don't line up with a full-text token still find a match.
// includeStale false (the default from the handler) excludes rows with
// is_stale = TRUE.
func (s *toolCacheStore) Search(ctx context.Context, tenantID, query string, limit int, includeStale bool) ([]*store.CachedTool, error) {
	if limit <= 0 {
		limit = 50
	}
	staleFilter := ""
	if !includeStale {
		staleFilter = "AND is_stale = FALSE"
	}
	q := toolCacheSelect + `
		WHERE tenant_id = $1
		AND (
			search_vector @@ plainto_tsquery('english', $2)
			OR tool_name ILIKE '%' || $2 || '%'
			OR tool_namespace ILIKE '%' || $2 || '%'
			OR description ILIKE '%' || $2 || '%'
		)
		` + staleFilter + `
		AND ` + connectorNotDeleted + `
		ORDER BY ts_rank(search_vector, plainto_tsquery('english', $2)) DESC, tool_name
		LIMIT $3`
	return s.query(ctx, q, tenantID, query, limit)
}

func (s *toolCacheStore) MarkStale(ctx context.Context, connectorID string) error {
	const q = `UPDATE tool_cache SET is_stale = TRUE WHERE connector_id = $1`
	_, err := s.db.ExecContext(ctx, q, connectorID)
	if err != nil {
		return fmt.Errorf("postgres: mark tool cache stale: %w", err)
	}
	return nil
}

func (s *toolCacheStore) DeleteByConnector(ctx context.Context, connectorID string) error {
	const q = `DELETE FROM tool_cache WHERE connector_id = $1`
	_, err := s.db.ExecContext(ctx, q, connectorID)
	if err != nil {
		return fmt.Errorf("postgres: delete tool cache by connector: %w", err)
	}
	return nil
}

func (s *toolCacheStore) DeleteExpired(ctx context.Context) (int64, error) {
	const q = `DELETE FROM tool_cache WHERE expires_at < now()`
	res, err := s.db.ExecContext(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("postgres: delete expired tool cache: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("postgres: rows affected: %w", err)
	}
	return n, nil
}

const toolCacheSelect = `
	SELECT tenant_id, connector_id, tool_namespace, tool_name, description, input_schema, cached_at, expires_at, is_stale
	FROM tool_cache`

// connectorNotDeleted excludes rows whose connector has been soft-deleted
// (connectors.deleted_at set). It is the second layer of the delete-time
// cache invalidation in internal/api/handlers.Connectors.Delete: that
// handler clears a deleted connector's rows outright, but if that call
// is missed (no CacheOps wired in) or the process crashes between the
// two writes, this predicate keeps them from ever being read back
// instead. The EXISTS subquery drives off connectors' primary key, so
// this stays an index lookup rather than a join-driven scan.
const connectorNotDeleted = `EXISTS (SELECT 1 FROM connectors c WHERE c.id = tool_cache.connector_id AND c.deleted_at IS NULL)`

func (s *toolCacheStore) query(ctx context.Context, q string, args ...any) ([]*store.CachedTool, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: query tool cache: %w", err)
	}
	defer rows.Close()

	var out []*store.CachedTool
	for rows.Next() {
		var ct store.CachedTool
		var schema []byte
		if err := rows.Scan(&ct.TenantID, &ct.ConnectorID, &ct.ToolNamespace, &ct.ToolName, &ct.Description, &schema, &ct.CachedAt, &ct.ExpiresAt, &ct.Stale); err != nil {
			return nil, fmt.Errorf("postgres: scan cached tool: %w", err)
		}
		if ct.InputSchema, err = unmarshalJSONB(schema); err != nil {
			return nil, fmt.Errorf("postgres: unmarshal tool input_schema: %w", err)
		}
		out = append(out, &ct)
	}
	return out, rows.Err()
}
