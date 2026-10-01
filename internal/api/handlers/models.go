package handlers

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// ModelInvalidator is the hook the Models handler calls after every
// successful write so a running LLM plane's registry cache
// (internal/llmplane.Registry) drops the tenant's entries at once instead
// of serving the old target for up to its TTL. A nil interface is a
// documented no-op -- e.g. an api pod without the LLM plane wired in.
type ModelInvalidator interface {
	Invalidate(tenantID string)
}

// Models implements the /api/v1/models routes: the tenant's view of the
// LLM plane's model registry. Reads return the tenant's own rows merged
// with the platform defaults (scope "platform"); writes address tenant
// rows only -- a platform row can be shadowed by creating a tenant row of
// the same name, never edited or deleted through this API.
type Models struct{ Deps }

// maxModelTargets bounds a row's fallback chain; more than a handful of
// targets is almost certainly a configuration mistake.
const maxModelTargets = 8

type modelRequest struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Enabled     *bool               `json:"enabled,omitempty"`
	Targets     []store.ModelTarget `json:"targets"`
	Price       *store.ModelPrice   `json:"price,omitempty"`
	// Limits are the model-level budgets and caps (same shape and
	// validation as an API key's). Absent or null = none; like price, PUT
	// replaces them.
	Limits   *store.Limits  `json:"limits,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type modelView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	// Scope is "tenant" for the caller's own row, "platform" for a
	// default every tenant sees (read-only through this API).
	Scope string `json:"scope"`
	// Targets carries credential NAMES only; the values never leave the
	// secret store.
	Targets   []store.ModelTarget `json:"targets"`
	Price     *store.ModelPrice   `json:"price"`
	Limits    *store.Limits       `json:"limits"`
	Metadata  map[string]any      `json:"metadata"`
	CreatedAt string              `json:"created_at"`
	UpdatedAt string              `json:"updated_at"`
}

func newModelView(m *store.Model) modelView {
	scope := "tenant"
	if m.TenantID == "" {
		scope = "platform"
	}
	targets := m.Targets
	if targets == nil {
		targets = []store.ModelTarget{}
	}
	meta := m.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	return modelView{
		ID:          m.ID,
		Name:        m.Name,
		Description: m.Description,
		Enabled:     m.Enabled,
		Scope:       scope,
		Targets:     targets,
		Price:       m.Price,
		Limits:      m.Limits,
		Metadata:    meta,
		CreatedAt:   formatTime(m.CreatedAt),
		UpdatedAt:   formatTime(m.UpdatedAt),
	}
}

// validate checks the request's shape (config.ValidateModelName /
// ValidateModelTarget, shared with the YAML seed) and that every
// credential a target names exists in the tenant's credential store, so a
// typo surfaces here as a 400 rather than as a 502 on the first call.
func (h Models) validate(r *http.Request, tenantID string, req modelRequest) error {
	if err := config.ValidateModelName(req.Name); err != nil {
		return err
	}
	if len([]rune(req.Description)) > 1024 {
		return fmt.Errorf("description must be at most 1024 characters")
	}
	if len(req.Targets) == 0 {
		return fmt.Errorf("targets: at least one target is required")
	}
	if len(req.Targets) > maxModelTargets {
		return fmt.Errorf("targets: at most %d targets are allowed", maxModelTargets)
	}
	for i, t := range req.Targets {
		if err := config.ValidateModelTarget(r.Context(), t.Vendor, t.Model, t.BaseURL, t.Region); err != nil {
			return fmt.Errorf("targets[%d]: %w", i, err)
		}
		if err := config.ValidateModelTargetLabel(t.Vendor, t.Label); err != nil {
			return fmt.Errorf("targets[%d]: %w", i, err)
		}
		if t.Credential == "" {
			continue
		}
		if t.AllowCallerKey {
			return fmt.Errorf("targets[%d]: allow_caller_key cannot be combined with credential (a target's credential always wins)", i)
		}
		if err := validateName(fmt.Sprintf("targets[%d].credential", i), t.Credential); err != nil {
			return err
		}
		if _, err := h.Store.Credentials().Get(r.Context(), tenantID, t.Credential); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("targets[%d]: credential %q does not exist in this tenant", i, t.Credential)
			}
			return fmt.Errorf("%w: targets[%d] credential %q: %w", errCredentialLookup, i, t.Credential, err)
		}
	}
	if req.Price != nil {
		if err := config.ValidateModelPrice(req.Price.Input, req.Price.Output, req.Price.CacheRead, req.Price.CacheWrite); err != nil {
			return fmt.Errorf("price: %w", err)
		}
	}
	if req.Limits != nil {
		if err := validateLimits(req.Limits); err != nil {
			return err
		}
	}
	return nil
}

// invalidate tells the LLM plane's registry cache (when wired) that the
// tenant's rows changed.
func (h Models) invalidate(tenantID string) {
	if h.ModelInvalidator != nil {
		h.ModelInvalidator.Invalidate(tenantID)
	}
}

// Create handles POST /api/v1/models. enabled defaults to true. A name
// already used by a live row of this tenant is a 409; a name that only a
// platform row uses is fine and shadows it for this tenant.
func (h Models) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	var req modelRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if err := h.validate(r, tid, req); err != nil {
		if errors.Is(err, errCredentialLookup) {
			writeStoreErr(w, r, "validate model", err)
			return
		}
		httpx.ValidationError(w, err.Error())
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	meta := req.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	m := &store.Model{
		TenantID:    tid,
		Name:        req.Name,
		Description: req.Description,
		Enabled:     enabled,
		Targets:     req.Targets,
		Price:       req.Price,
		Limits:      req.Limits,
		Metadata:    meta,
	}
	if err := h.Store.Models().Create(r.Context(), m); err != nil {
		writeStoreErr(w, r, "create model", err)
		return
	}
	h.invalidate(tid)
	httpx.WriteJSON(w, http.StatusCreated, newModelView(m))
}

// List handles GET /api/v1/models: the tenant's rows plus the platform
// defaults (the tenant's row wins on a shared name), ordered by name.
func (h Models) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}

	models, err := h.Store.Models().List(r.Context(), tid, store.ListOptions{})
	if err != nil {
		writeStoreErr(w, r, "list models", err)
		return
	}

	page := httpx.ParsePagination(r)
	views := make([]modelView, 0, len(models))
	for _, m := range models {
		views = append(views, newModelView(m))
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: paginate(views, page), Total: len(views)})
}

// Get handles GET /api/v1/models/{id} (tenant or platform row).
func (h Models) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	m, err := h.Store.Models().Get(r.Context(), tid, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreErr(w, r, "get model", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newModelView(m))
}

// Update handles PUT /api/v1/models/{id} with full-replace semantics for
// name/description/enabled/targets/price/limits; metadata, if omitted, is left
// as-is (JSON null and absence both decode to nil, so this is the safer
// default, as for connectors). A platform row is 403: shadow it with a
// tenant row instead.
func (h Models) Update(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	existing, err := h.Store.Models().Get(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "update model", err)
		return
	}
	if existing.TenantID == "" {
		httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission, "platform models are read-only; create a tenant model with the same name to override it")
		return
	}

	var req modelRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if err := h.validate(r, tid, req); err != nil {
		if errors.Is(err, errCredentialLookup) {
			writeStoreErr(w, r, "validate model", err)
			return
		}
		httpx.ValidationError(w, err.Error())
		return
	}

	existing.Name = req.Name
	existing.Description = req.Description
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	existing.Targets = req.Targets
	existing.Price = req.Price
	existing.Limits = req.Limits
	if req.Metadata != nil {
		existing.Metadata = req.Metadata
	}

	if err := h.Store.Models().Update(r.Context(), existing); err != nil {
		writeStoreErr(w, r, "update model", err)
		return
	}
	h.invalidate(tid)
	httpx.WriteJSON(w, http.StatusOK, newModelView(existing))
}

// Delete handles DELETE /api/v1/models/{id} (soft). Platform rows are 403.
func (h Models) Delete(w http.ResponseWriter, r *http.Request) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	existing, err := h.Store.Models().Get(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "delete model", err)
		return
	}
	if existing.TenantID == "" {
		httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission, "platform models are read-only")
		return
	}
	if err := h.Store.Models().SoftDelete(r.Context(), tid, id); err != nil {
		writeStoreErr(w, r, "delete model", err)
		return
	}
	h.invalidate(tid)
	applog.From(r.Context()).Info("model deleted", "model_id", id, "name", existing.Name)
	w.WriteHeader(http.StatusNoContent)
}

// errCredentialLookup marks a validate failure caused by the credential
// store itself (not a missing row, which is the caller's mistake and a
// plain 400): the handlers answer it as a 500 via writeStoreErr.
var errCredentialLookup = errors.New("credential lookup failed")
