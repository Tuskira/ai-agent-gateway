package handlers

import (
	"errors"
	"net/http"

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
// even bare "*") grant does not imply it -- see matchPermission. For
// phase 1 there is no separate "platform-admin" role; the built-in admin
// role is the only one that holds "platform.admin" out of the box (see
// NewRoleAuthorizer). Deployments that want a narrower platform-admin
// role can define one via config.Auth.Roles.
type Tenants struct{ Deps }

const maxSlugLen = 100

type createTenantRequest struct {
	// ID, when set, is the new tenant's id (a canonical UUID) instead of
	// a generated one, so the id can match an external system's. Creating
	// a tenant is platform.admin only, so only a platform caller can send it.
	ID   string `json:"id,omitempty"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type tenantView struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

func newTenantView(t *store.Tenant) tenantView {
	return tenantView{
		ID:        t.ID,
		Slug:      t.Slug,
		Name:      t.Name,
		CreatedAt: formatTime(t.CreatedAt),
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

	t := &store.Tenant{ID: id, Slug: req.Slug, Name: req.Name}
	if err := h.Store.Tenants().Create(r.Context(), t); err != nil {
		writeStoreErr(w, r, "create tenant", err)
		return
	}

	AuditNote(r.Context(), "created_tenant_id", t.ID, "created_tenant_slug", t.Slug, "id_supplied", id != "")
	httpx.WriteJSON(w, http.StatusCreated, newTenantView(t))
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
	if q.Has("slug") && q.Get("slug") == "" {
		httpx.ValidationError(w, "slug must not be empty")
		return
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
