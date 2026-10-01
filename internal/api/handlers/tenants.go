package handlers

import (
	"net/http"

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

	t := &store.Tenant{Slug: req.Slug, Name: req.Name}
	if err := h.Store.Tenants().Create(r.Context(), t); err != nil {
		writeStoreErr(w, r, "create tenant", err)
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, newTenantView(t))
}

// List handles GET /api/v1/tenants.
func (h Tenants) List(w http.ResponseWriter, r *http.Request) {
	tenants, err := h.Store.Tenants().List(r.Context())
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
