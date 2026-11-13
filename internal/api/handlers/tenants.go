package handlers

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Tenants implements POST/GET /api/v1/tenants.
//
// Both routes are gated on the "platform.admin" permission (see
// router.go), not "tenant.read"/"tenant.create": Tenant rows have no
// owning tenant_id of their own (a tenant IS the isolation boundary), so
// List(ctx) is inherently cross-tenant, and pkgauth.RoleAuthorizer
// special-cases the "platform." namespace so an ordinary "*.read" (or
// even bare "*") grant does not imply it -- see matchPermission. The
// built-in platform-admin role (an API key, or a console user with
// store.UserRolePlatformAdmin) is the one that holds "platform.admin"
// out of the box (see NewRoleAuthorizer); custom roles with a
// "platform.*" pattern can be defined via config.Auth.Roles. These
// handlers never call requireTenant, so X-Gateway-Acting-Tenant does not
// scope them.
type Tenants struct{ Deps }

const maxSlugLen = 100

type createTenantRequest struct {
	// ID, when set, is the new tenant's id (a canonical UUID) instead of
	// a generated one, so the id can match an external system's. Creating
	// a tenant is platform.admin only, so only a platform caller can send it.
	ID   string `json:"id,omitempty"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	// StrictModels sets the tenant's strict model policy (default false):
	// the LLM plane then refuses a model with no registry row.
	StrictModels bool `json:"strict_models,omitempty"`
}

type tenantView struct {
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	StrictModels bool   `json:"strict_models"`
	CreatedAt    string `json:"created_at"`
}

func newTenantView(t *store.Tenant) tenantView {
	return tenantView{
		ID:           t.ID,
		Slug:         t.Slug,
		Name:         t.Name,
		StrictModels: t.StrictModels,
		CreatedAt:    formatTime(t.CreatedAt),
	}
}

// Create handles POST /api/v1/tenants.
func (h Tenants) Create(w http.ResponseWriter, r *http.Request) {
	var req createTenantRequest
	if !httpx.Decode(w, r, &req) {
		return
	}

	if err := validateName("name", req.Name); err != nil {
		httpx.ValidationError(w, err.Error())
		return
	}
	if req.Slug == "" || len([]rune(req.Slug)) > maxSlugLen {
		httpx.ValidationError(w, "slug is required and must be at most 100 characters")
		return
	}

	id := ""
	if req.ID != "" {
		u, err := uuid.Parse(req.ID)
		if err != nil || u == uuid.Nil || u.String() != req.ID {
			httpx.ValidationError(w, "id must be a lowercase UUID in the canonical 8-4-4-4-12 form and not the nil UUID")
			return
		}
		id = u.String()
	}

	if req.StrictModels {
		if _, ok := h.Store.Tenants().(store.TenantModelPolicy); !ok {
			httpx.WriteError(w, http.StatusNotImplemented, "not_implemented", "this store cannot set a strict model policy")
			return
		}
	}

	t := &store.Tenant{ID: id, Slug: req.Slug, Name: req.Name, StrictModels: req.StrictModels}
	if err := h.Store.Tenants().Create(r.Context(), t); err != nil {
		writeStoreErr(w, r, "create tenant", err)
		return
	}

	AuditNote(r.Context(), "created_tenant_id", t.ID, "created_tenant_slug", t.Slug, "id_supplied", id != "")
	httpx.WriteJSON(w, http.StatusCreated, newTenantView(t))
}

type updateTenantRequest struct {
	Name *string `json:"name"`
	// StrictModels sets the tenant's strict model policy. Absent (or
	// null) leaves it unchanged.
	StrictModels *bool `json:"strict_models"`
}

// Update handles PATCH /api/v1/tenants/{tenant_id}: rename a tenant and/or
// set its strict model policy. At least one of name and strict_models is
// required. The slug and id are immutable. An id that is not a tenant,
// including a malformed one, is 404.
func (h Tenants) Update(w http.ResponseWriter, r *http.Request) {
	var req updateTenantRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if req.Name == nil && req.StrictModels == nil {
		httpx.ValidationError(w, "name or strict_models is required")
		return
	}
	if req.Name != nil {
		if err := validateName("name", *req.Name); err != nil {
			httpx.ValidationError(w, err.Error())
			return
		}
	}
	var policy store.TenantModelPolicy
	if req.StrictModels != nil {
		var ok bool
		if policy, ok = h.Store.Tenants().(store.TenantModelPolicy); !ok {
			httpx.WriteError(w, http.StatusNotImplemented, "not_implemented", "this store cannot set a strict model policy")
			return
		}
	}

	id := chi.URLParam(r, "tenant_id")
	var t *store.Tenant
	var err error
	if req.Name != nil {
		if t, err = h.Store.Tenants().UpdateName(r.Context(), id, *req.Name); err != nil {
			writeStoreErr(w, r, "rename tenant", err)
			return
		}
		AuditNote(r.Context(), "renamed_tenant_id", t.ID, "new_name", t.Name)
	}
	if req.StrictModels != nil {
		if t, err = policy.SetStrictModels(r.Context(), id, *req.StrictModels); err != nil {
			writeStoreErr(w, r, "set tenant model policy", err)
			return
		}
		// The LLM plane caches the tenant's policy with its registry.
		if h.ModelInvalidator != nil {
			h.ModelInvalidator.Invalidate(t.ID)
		}
		AuditNote(r.Context(), "policy_tenant_id", t.ID, "strict_models", t.StrictModels)
	}

	httpx.WriteJSON(w, http.StatusOK, newTenantView(t))
}

// List handles GET /api/v1/tenants.
//
// ?slug=<slug> narrows the list to the tenant with that exact slug (an
// empty list when there is none), so an operator who knows only the slug
// can find the id the tenant-scoped routes need.
func (h Tenants) List(w http.ResponseWriter, r *http.Request) {
	var tenants []*store.Tenant
	var err error
	q := r.URL.Query()
	for _, v := range q["slug"] {
		if v == "" {
			httpx.ValidationError(w, "slug must not be empty")
			return
		}
	}
	if slug := q.Get("slug"); slug != "" {
		var t *store.Tenant
		if t, err = h.Store.Tenants().GetBySlug(r.Context(), slug); err == nil {
			tenants = []*store.Tenant{t}
		} else if errors.Is(err, store.ErrNotFound) {
			err = nil
		}
	} else {
		tenants, err = h.Store.Tenants().List(r.Context())
	}
	if err != nil {
		writeStoreErr(w, r, "list tenants", err)
		return
	}

	page := httpx.ParsePagination(r)
	views := make([]tenantView, 0, len(tenants))
	for _, t := range tenants {
		views = append(views, newTenantView(t))
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page{Items: paginate(views, page), Total: len(views)})
}
