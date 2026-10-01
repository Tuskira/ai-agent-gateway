package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type mcpCatalogStore struct{ db *sql.DB }

var _ store.MCPCatalogStore = (*mcpCatalogStore)(nil)

const mcpCatalogCols = `id, COALESCE(tenant_id::text, ''), slug, name, description, icon, category, url, url_overridable, transport,
		auth, default_headers, suggested_tools, docs_url, enabled, created_at, updated_at, deleted_at`

const mcpCatalogSelect = `SELECT ` + mcpCatalogCols + ` FROM mcp_catalog`

// visible matches the rows a tenant reads: its own plus the platform rows.
// With $1 = ” (NULLIF gives NULL) the equality is never true, leaving the
// platform rows alone.
const mcpCatalogVisible = `(tenant_id IS NULL OR tenant_id = NULLIF($1, '')::uuid)`

func (s *mcpCatalogStore) List(ctx context.Context, tenantID string) ([]*store.MCPCatalogEntry, error) {
	// DISTINCT ON keeps one row per slug, the tenant's own ahead of the
	// platform's; the outer query then orders for display.
	q := `SELECT ` + mcpCatalogCols + ` FROM (
		SELECT DISTINCT ON (slug) * FROM mcp_catalog
		WHERE deleted_at IS NULL AND ` + mcpCatalogVisible + `
		ORDER BY slug, (tenant_id IS NULL)
	) merged ORDER BY name, slug`
	rows, err := s.db.QueryContext(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list mcp catalog: %w", err)
	}
	defer rows.Close()
	var out []*store.MCPCatalogEntry
	for rows.Next() {
		e, err := scanMCPCatalogRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *mcpCatalogStore) Get(ctx context.Context, tenantID, id string) (*store.MCPCatalogEntry, error) {
	e, err := scanMCPCatalogRow(s.db.QueryRowContext(ctx,
		mcpCatalogSelect+` WHERE id = $2 AND deleted_at IS NULL AND `+mcpCatalogVisible, tenantID, id))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return e, nil
}

func (s *mcpCatalogStore) GetBySlug(ctx context.Context, tenantID, slug string) (*store.MCPCatalogEntry, error) {
	e, err := scanMCPCatalogRow(s.db.QueryRowContext(ctx,
		mcpCatalogSelect+` WHERE slug = $2 AND deleted_at IS NULL AND `+mcpCatalogVisible+
			` ORDER BY (tenant_id IS NULL) LIMIT 1`, tenantID, slug))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return e, nil
}

// mcpCatalogJSON normalises and encodes the JSONB columns.
func mcpCatalogJSON(e *store.MCPCatalogEntry) (auth, hdrs, tools []byte, err error) {
	if e.Transport == "" {
		e.Transport = "streamable-http"
	}
	if e.Auth.Kind == "" {
		e.Auth.Kind = "none"
	}
	if e.Auth.Fields == nil {
		e.Auth.Fields = []store.MCPCatalogField{}
	}
	if e.DefaultHeaders == nil {
		e.DefaultHeaders = map[string]string{}
	}
	if e.SuggestedTools == nil {
		e.SuggestedTools = []string{}
	}
	if auth, err = json.Marshal(e.Auth); err != nil {
		return nil, nil, nil, fmt.Errorf("postgres: marshal mcp catalog auth: %w", err)
	}
	if hdrs, err = json.Marshal(e.DefaultHeaders); err != nil {
		return nil, nil, nil, fmt.Errorf("postgres: marshal mcp catalog default_headers: %w", err)
	}
	if tools, err = json.Marshal(e.SuggestedTools); err != nil {
		return nil, nil, nil, fmt.Errorf("postgres: marshal mcp catalog suggested_tools: %w", err)
	}
	return auth, hdrs, tools, nil
}

const mcpCatalogInsert = `
	INSERT INTO mcp_catalog (tenant_id, slug, name, description, icon, category, url, url_overridable, transport,
		auth, default_headers, suggested_tools, docs_url, enabled)
	VALUES (NULLIF($1, '')::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

func (s *mcpCatalogStore) Create(ctx context.Context, e *store.MCPCatalogEntry) error {
	auth, hdrs, tools, err := mcpCatalogJSON(e)
	if err != nil {
		return err
	}
	err = s.db.QueryRowContext(ctx, mcpCatalogInsert+` RETURNING id, created_at, updated_at`,
		e.TenantID, e.Slug, e.Name, e.Description, e.Icon, e.Category, e.URL, e.URLOverridable, e.Transport,
		auth, hdrs, tools, e.DocsURL, e.Enabled).Scan(&e.ID, &e.CreatedAt, &e.UpdatedAt)
	return mapWriteErr(err)
}

func (s *mcpCatalogStore) Upsert(ctx context.Context, e *store.MCPCatalogEntry) error {
	auth, hdrs, tools, err := mcpCatalogJSON(e)
	if err != nil {
		return err
	}
	const conflict = `
		ON CONFLICT ((COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid)), slug) WHERE deleted_at IS NULL
		DO UPDATE SET
			name = EXCLUDED.name, description = EXCLUDED.description, icon = EXCLUDED.icon,
			category = EXCLUDED.category, url = EXCLUDED.url, url_overridable = EXCLUDED.url_overridable,
			transport = EXCLUDED.transport, auth = EXCLUDED.auth, default_headers = EXCLUDED.default_headers,
			suggested_tools = EXCLUDED.suggested_tools, docs_url = EXCLUDED.docs_url,
			enabled = EXCLUDED.enabled, updated_at = now()
		RETURNING id, created_at, updated_at`
	err = s.db.QueryRowContext(ctx, mcpCatalogInsert+conflict,
		e.TenantID, e.Slug, e.Name, e.Description, e.Icon, e.Category, e.URL, e.URLOverridable, e.Transport,
		auth, hdrs, tools, e.DocsURL, e.Enabled).Scan(&e.ID, &e.CreatedAt, &e.UpdatedAt)
	return mapWriteErr(err)
}

func (s *mcpCatalogStore) Update(ctx context.Context, e *store.MCPCatalogEntry) error {
	auth, hdrs, tools, err := mcpCatalogJSON(e)
	if err != nil {
		return err
	}
	const q = `
		UPDATE mcp_catalog SET name = $3, description = $4, icon = $5, category = $6, url = $7,
			url_overridable = $8, transport = $9, auth = $10, default_headers = $11, suggested_tools = $12,
			docs_url = $13, enabled = $14, updated_at = now()
		WHERE id = $2 AND tenant_id IS NOT DISTINCT FROM NULLIF($1, '')::uuid AND deleted_at IS NULL
		RETURNING updated_at`
	err = s.db.QueryRowContext(ctx, q, e.TenantID, e.ID, e.Name, e.Description, e.Icon, e.Category, e.URL,
		e.URLOverridable, e.Transport, auth, hdrs, tools, e.DocsURL, e.Enabled).Scan(&e.UpdatedAt)
	if err != nil {
		return mapWriteErr(mapReadErr(err))
	}
	return nil
}

func (s *mcpCatalogStore) SoftDelete(ctx context.Context, tenantID, id string) error {
	return execExpectingOneRow(ctx, s.db,
		`UPDATE mcp_catalog SET deleted_at = now(), updated_at = now()
		 WHERE id = $2 AND tenant_id IS NOT DISTINCT FROM NULLIF($1, '')::uuid AND deleted_at IS NULL`, tenantID, id)
}

func scanMCPCatalogRow(row rowScanner) (*store.MCPCatalogEntry, error) {
	var e store.MCPCatalogEntry
	var auth, hdrs, tools []byte
	if err := row.Scan(&e.ID, &e.TenantID, &e.Slug, &e.Name, &e.Description, &e.Icon, &e.Category, &e.URL, &e.URLOverridable,
		&e.Transport, &auth, &hdrs, &tools, &e.DocsURL, &e.Enabled, &e.CreatedAt, &e.UpdatedAt, &e.DeletedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(auth, &e.Auth); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal mcp catalog auth: %w", err)
	}
	if e.Auth.Fields == nil {
		e.Auth.Fields = []store.MCPCatalogField{}
	}
	if err := json.Unmarshal(hdrs, &e.DefaultHeaders); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal mcp catalog default_headers: %w", err)
	}
	if e.DefaultHeaders == nil {
		e.DefaultHeaders = map[string]string{}
	}
	if err := json.Unmarshal(tools, &e.SuggestedTools); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal mcp catalog suggested_tools: %w", err)
	}
	if e.SuggestedTools == nil {
		e.SuggestedTools = []string{}
	}
	return &e, nil
}
