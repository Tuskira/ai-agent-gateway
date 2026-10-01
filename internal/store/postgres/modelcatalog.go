package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type modelCatalogStore struct{ db *sql.DB }

var _ store.ModelCatalogStore = (*modelCatalogStore)(nil)

const providerColumns = `id, slug, display_name, vendor, base_url, docs_url, enabled, created_at, updated_at`
const providerSelect = `SELECT ` + providerColumns + ` FROM model_catalog_providers`

func scanProviderRow(row rowScanner) (*store.ModelCatalogProvider, error) {
	var p store.ModelCatalogProvider
	if err := row.Scan(&p.ID, &p.Slug, &p.DisplayName, &p.Vendor, &p.BaseURL, &p.DocsURL, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

const catalogModelColumns = `id, provider_id, model_id, display_name, suggested_name, price, capabilities, notes, enabled, created_at, updated_at`
const catalogModelSelect = `SELECT ` + catalogModelColumns + ` FROM model_catalog_models`

func scanCatalogModelRow(row rowScanner) (*store.ModelCatalogModel, error) {
	var m store.ModelCatalogModel
	var price, caps []byte
	if err := row.Scan(&m.ID, &m.ProviderID, &m.ModelID, &m.DisplayName, &m.SuggestedName, &price, &caps, &m.Notes, &m.Enabled, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, err
	}
	if len(price) > 0 {
		var p store.ModelPrice
		if err := json.Unmarshal(price, &p); err != nil {
			return nil, fmt.Errorf("postgres: unmarshal catalog model price: %w", err)
		}
		m.Price = &p
	}
	caps2, err := unmarshalJSONB(caps)
	if err != nil {
		return nil, fmt.Errorf("postgres: unmarshal catalog model capabilities: %w", err)
	}
	m.Capabilities = caps2
	return &m, nil
}

// ---------------------------------------------------------------------------
// Providers
// ---------------------------------------------------------------------------

func (s *modelCatalogStore) ListProviders(ctx context.Context) ([]*store.ModelCatalogProvider, error) {
	rows, err := s.db.QueryContext(ctx, providerSelect+` ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list catalog providers: %w", err)
	}
	defer rows.Close()

	var out []*store.ModelCatalogProvider
	for rows.Next() {
		p, err := scanProviderRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	models, err := s.listAllModels(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range out {
		p.Models = models[p.ID]
	}
	return out, nil
}

// listAllModels returns every catalog model, grouped by provider id and
// ordered by suggested_name within each group -- one query for
// ListProviders rather than N.
func (s *modelCatalogStore) listAllModels(ctx context.Context) (map[string][]store.ModelCatalogModel, error) {
	rows, err := s.db.QueryContext(ctx, catalogModelSelect+` ORDER BY suggested_name`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list catalog models: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]store.ModelCatalogModel)
	for rows.Next() {
		m, err := scanCatalogModelRow(rows)
		if err != nil {
			return nil, err
		}
		out[m.ProviderID] = append(out[m.ProviderID], *m)
	}
	return out, rows.Err()
}

func (s *modelCatalogStore) GetProvider(ctx context.Context, id string) (*store.ModelCatalogProvider, error) {
	p, err := scanProviderRow(s.db.QueryRowContext(ctx, providerSelect+` WHERE id = $1`, id))
	if err != nil {
		return nil, mapReadErr(err)
	}
	rows, err := s.db.QueryContext(ctx, catalogModelSelect+` WHERE provider_id = $1 ORDER BY suggested_name`, id)
	if err != nil {
		return nil, fmt.Errorf("postgres: list provider's catalog models: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanCatalogModelRow(rows)
		if err != nil {
			return nil, err
		}
		p.Models = append(p.Models, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *modelCatalogStore) CreateProvider(ctx context.Context, p *store.ModelCatalogProvider) error {
	if p.Vendor == "" {
		p.Vendor = "openai_compat"
	}
	const q = `
		INSERT INTO model_catalog_providers (slug, display_name, vendor, base_url, docs_url, enabled)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at, updated_at`
	err := s.db.QueryRowContext(ctx, q, p.Slug, p.DisplayName, p.Vendor, p.BaseURL, p.DocsURL, p.Enabled).
		Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	return mapWriteErr(err)
}

func (s *modelCatalogStore) UpdateProvider(ctx context.Context, p *store.ModelCatalogProvider) error {
	const q = `
		UPDATE model_catalog_providers SET slug = $2, display_name = $3, base_url = $4, docs_url = $5, enabled = $6, updated_at = now()
		WHERE id = $1
		RETURNING updated_at`
	err := s.db.QueryRowContext(ctx, q, p.ID, p.Slug, p.DisplayName, p.BaseURL, p.DocsURL, p.Enabled).Scan(&p.UpdatedAt)
	if err != nil {
		return mapWriteErr(mapReadErr(err))
	}
	return nil
}

func (s *modelCatalogStore) DeleteProvider(ctx context.Context, id string) error {
	const q = `DELETE FROM model_catalog_providers WHERE id = $1`
	return execExpectingOneRow(ctx, s.db, q, id)
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

func (s *modelCatalogStore) GetModel(ctx context.Context, id string) (*store.ModelCatalogModel, error) {
	m, err := scanCatalogModelRow(s.db.QueryRowContext(ctx, catalogModelSelect+` WHERE id = $1`, id))
	if err != nil {
		return nil, mapReadErr(err)
	}
	return m, nil
}

func (s *modelCatalogStore) CreateModel(ctx context.Context, m *store.ModelCatalogModel) error {
	price, err := marshalPrice(m.Price)
	if err != nil {
		return fmt.Errorf("postgres: marshal catalog model price: %w", err)
	}
	caps, err := marshalJSONB(m.Capabilities)
	if err != nil {
		return fmt.Errorf("postgres: marshal catalog model capabilities: %w", err)
	}
	const q = `
		INSERT INTO model_catalog_models (provider_id, model_id, display_name, suggested_name, price, capabilities, notes, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at, updated_at`
	err = s.db.QueryRowContext(ctx, q, m.ProviderID, m.ModelID, m.DisplayName, m.SuggestedName, price, caps, m.Notes, m.Enabled).
		Scan(&m.ID, &m.CreatedAt, &m.UpdatedAt)
	return mapWriteErr(err)
}

func (s *modelCatalogStore) UpdateModel(ctx context.Context, m *store.ModelCatalogModel) error {
	price, err := marshalPrice(m.Price)
	if err != nil {
		return fmt.Errorf("postgres: marshal catalog model price: %w", err)
	}
	caps, err := marshalJSONB(m.Capabilities)
	if err != nil {
		return fmt.Errorf("postgres: marshal catalog model capabilities: %w", err)
	}
	// provider_id is immutable -- not in the SET list.
	const q = `
		UPDATE model_catalog_models SET model_id = $2, display_name = $3, suggested_name = $4, price = $5, capabilities = $6, notes = $7, enabled = $8, updated_at = now()
		WHERE id = $1
		RETURNING updated_at`
	err = s.db.QueryRowContext(ctx, q, m.ID, m.ModelID, m.DisplayName, m.SuggestedName, price, caps, m.Notes, m.Enabled).Scan(&m.UpdatedAt)
	if err != nil {
		return mapWriteErr(mapReadErr(err))
	}
	return nil
}

func (s *modelCatalogStore) DeleteModel(ctx context.Context, id string) error {
	const q = `DELETE FROM model_catalog_models WHERE id = $1`
	return execExpectingOneRow(ctx, s.db, q, id)
}

func (s *modelCatalogStore) CountTenantUsage(ctx context.Context, catalogModelID string) (int, error) {
	const q = `SELECT count(*) FROM models WHERE catalog_model_id = $1 AND deleted_at IS NULL`
	var n int
	if err := s.db.QueryRowContext(ctx, q, catalogModelID).Scan(&n); err != nil {
		return 0, fmt.Errorf("postgres: count catalog model usage: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Connect
// ---------------------------------------------------------------------------

// Connect implements store.ModelCatalogStore.Connect: the credential
// (existing lookup, or new insert of an already-sealed payload) and every
// requested tenant model are written in one transaction, so a caller never
// observes a partial connect (some models registered, others not) and a
// mid-way conflict leaves nothing behind.
func (s *modelCatalogStore) Connect(ctx context.Context, tenantID string, cred store.CatalogConnectCredential, models []store.CatalogConnectModel) (*store.CatalogConnectResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	result := &store.CatalogConnectResult{}

	switch {
	case cred.Existing != "":
		var exists bool
		const q = `SELECT EXISTS(SELECT 1 FROM credentials WHERE tenant_id = $1 AND name = $2)`
		if err := tx.QueryRowContext(ctx, q, tenantID, cred.Existing).Scan(&exists); err != nil {
			return nil, fmt.Errorf("postgres: connect: check existing credential: %w", err)
		}
		if !exists {
			return nil, store.ErrNotFound
		}
		result.CredentialName = cred.Existing
		result.CredentialCreated = false

	case cred.New != nil:
		var nameTaken bool
		const q = `SELECT EXISTS(SELECT 1 FROM credentials WHERE tenant_id = $1 AND name = $2)`
		if err := tx.QueryRowContext(ctx, q, tenantID, cred.New.Name).Scan(&nameTaken); err != nil {
			return nil, fmt.Errorf("postgres: connect: check new credential name: %w", err)
		}
		if nameTaken {
			return nil, &store.CredentialNameConflictError{Name: cred.New.Name}
		}
		fieldNames := cred.New.FieldNames
		if fieldNames == nil {
			fieldNames = []string{}
		}
		const ins = `
			INSERT INTO credentials (tenant_id, name, type, ciphertext, nonce, key_id, field_names)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`
		if _, err := tx.ExecContext(ctx, ins, tenantID, cred.New.Name, cred.New.Type, cred.New.Ciphertext, cred.New.Nonce, cred.New.KeyID, fieldNames); err != nil {
			return nil, mapWriteErr(err)
		}
		result.CredentialName = cred.New.Name
		result.CredentialCreated = true

	default:
		return nil, fmt.Errorf("postgres: connect: credential spec has neither Existing nor New set")
	}

	for _, cm := range models {
		mr, err := s.connectOne(ctx, tx, tenantID, result.CredentialName, cm)
		if err != nil {
			return nil, err
		}
		result.Models = append(result.Models, *mr)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("postgres: connect: commit: %w", err)
	}
	return result, nil
}

// connectOne registers one catalog model for tenantID within tx, applying
// the name-clash / already-registered rules documented on
// store.ModelCatalogStore.Connect.
func (s *modelCatalogStore) connectOne(ctx context.Context, tx *sql.Tx, tenantID, credentialName string, cm store.CatalogConnectModel) (*store.CatalogConnectModelResult, error) {
	const findQ = `SELECT id, catalog_model_id FROM models WHERE tenant_id = $1 AND name = $2 AND deleted_at IS NULL`
	var existingID string
	var existingCatalogID sql.NullString
	err := tx.QueryRowContext(ctx, findQ, tenantID, cm.Name).Scan(&existingID, &existingCatalogID)
	switch {
	case err == nil:
		if existingCatalogID.Valid && existingCatalogID.String == cm.CatalogModelID {
			return &store.CatalogConnectModelResult{CatalogModelID: cm.CatalogModelID, ModelID: existingID, Name: cm.Name, Status: "exists"}, nil
		}
		return nil, &store.ModelNameConflictError{Name: cm.Name}
	case err != sql.ErrNoRows:
		return nil, fmt.Errorf("postgres: connect: check model name %q: %w", cm.Name, err)
	}

	targets := []store.ModelTarget{{
		Vendor:     "openai_compat",
		Model:      cm.ModelID,
		BaseURL:    cm.BaseURL,
		Credential: credentialName,
		Label:      cm.Label,
	}}
	targetsJSON, err := marshalTargets(targets)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: marshal targets: %w", err)
	}
	price, err := marshalPrice(cm.Price)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: marshal price: %w", err)
	}
	caps, err := marshalCapabilities(cm.Capabilities)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: marshal capabilities: %w", err)
	}

	const insQ = `
		INSERT INTO models (tenant_id, name, description, enabled, targets, price, metadata, catalog_model_id, capabilities)
		VALUES ($1, $2, $3, TRUE, $4, $5, '{}', $6, $7)
		RETURNING id`
	var newID string
	if err := tx.QueryRowContext(ctx, insQ, tenantID, cm.Name, cm.Description, targetsJSON, price, cm.CatalogModelID, caps).Scan(&newID); err != nil {
		return nil, mapWriteErr(err)
	}
	return &store.CatalogConnectModelResult{CatalogModelID: cm.CatalogModelID, ModelID: newID, Name: cm.Name, Status: "created"}, nil
}

// ---------------------------------------------------------------------------
// ApplyPrices
// ---------------------------------------------------------------------------

// nullableRaw returns b unchanged, except a nil slice is returned as a bare
// untyped nil -- so it binds as SQL NULL rather than an empty bytea/jsonb
// value when passed as a query arg (driver.Value treats a nil []byte and an
// untyped nil differently for some comparisons; this keeps the "price IS
// NOT DISTINCT FROM $n" comparison in ApplyPrices unambiguous).
func nullableRaw(b []byte) any {
	if b == nil {
		return nil
	}
	return b
}

// ApplyPrices implements store.ModelCatalogStore.ApplyPrices. Every catalog
// model named in updates is locked (SELECT ... FOR UPDATE) and read before
// being rewritten, so the "tenant row's price still equals the OLD catalog
// price" comparison below is race-free against a concurrent ApplyPrices
// call on the same row.
func (s *modelCatalogStore) ApplyPrices(ctx context.Context, updates []store.PriceUpdate, updateTenantModels bool) (*store.ApplyPricesResult, error) {
	result := &store.ApplyPricesResult{}
	if len(updates) == 0 {
		return result, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("postgres: apply catalog prices: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	tenantIDs := make(map[string]struct{})
	for _, u := range updates {
		var oldPrice []byte
		const selQ = `SELECT price FROM model_catalog_models WHERE id = $1 FOR UPDATE`
		if err := tx.QueryRowContext(ctx, selQ, u.CatalogModelID).Scan(&oldPrice); err != nil {
			return nil, mapReadErr(err)
		}

		newPrice, err := marshalPrice(u.Price)
		if err != nil {
			return nil, fmt.Errorf("postgres: apply catalog prices: marshal price: %w", err)
		}

		const updQ = `UPDATE model_catalog_models SET price = $2, updated_at = now() WHERE id = $1`
		if _, err := tx.ExecContext(ctx, updQ, u.CatalogModelID, newPrice); err != nil {
			return nil, mapWriteErr(err)
		}
		result.CatalogUpdated++

		if updateTenantModels {
			const tenantQ = `
				UPDATE models SET price = $2, updated_at = now()
				WHERE catalog_model_id = $1 AND deleted_at IS NULL AND price IS NOT DISTINCT FROM $3
				RETURNING tenant_id`
			rows, err := tx.QueryContext(ctx, tenantQ, u.CatalogModelID, newPrice, nullableRaw(oldPrice))
			if err != nil {
				return nil, mapWriteErr(err)
			}
			for rows.Next() {
				var tid sql.NullString
				if err := rows.Scan(&tid); err != nil {
					rows.Close()
					return nil, fmt.Errorf("postgres: apply catalog prices: scan tenant_id: %w", err)
				}
				result.TenantUpdated++
				if tid.Valid {
					tenantIDs[tid.String] = struct{}{}
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("postgres: apply catalog prices: commit: %w", err)
	}
	for tid := range tenantIDs {
		result.TenantIDs = append(result.TenantIDs, tid)
	}
	return result, nil
}
