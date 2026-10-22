package handlers

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// The routes below let a platform-admin API key manage API keys in a NAMED
// tenant: POST/GET /api/v1/tenants/{tenant_id}/api-keys, DELETE
// /api/v1/tenants/{tenant_id}/api-keys/{id} and POST
// /api/v1/tenants/{tenant_id}/api-keys/{id}/rotate.
//
// They are gated on platform.admin in router.go, the same permission as
// POST/GET /tenants, so a tenant admin (or a console user, which never
// holds it) gets 403 for every tenant, including its own. Each one resolves
// the tenant first (404 when unknown) and then runs exactly the handler
// body the caller's-own-tenant route runs, with the target tenant id in
// place of the principal's: same validation, limits, role, expires_at and
// profile_id rules, same one-time plaintext, and the profile lookup is
// scoped to the target tenant. Nothing here reads a tenant id from a
// request body.

// platformKeyTenant resolves the {tenant_id} path parameter to an existing
// tenant. It writes a 404 and returns false when there is none; an id that
// is not a tenant -- including a malformed one -- is simply not found.
func (h APIKeys) platformKeyTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	t, err := h.Store.Tenants().GetByID(r.Context(), chi.URLParam(r, "tenant_id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, httpx.TypeNotFound, "tenant not found")
			return "", false
		}
		writeStoreErr(w, r, "look up tenant", err)
		return "", false
	}
	return t.ID, true
}

// PlatformCreate handles POST /api/v1/tenants/{tenant_id}/api-keys.
func (h APIKeys) PlatformCreate(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.platformKeyTenant(w, r)
	if !ok {
		return
	}
	if id := h.create(w, r, tid); id != "" {
		AuditNote(r.Context(), "new_key_id", id)
	}
}

// PlatformList handles GET /api/v1/tenants/{tenant_id}/api-keys.
func (h APIKeys) PlatformList(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.platformKeyTenant(w, r)
	if !ok {
		return
	}
	h.list(w, r, tid)
}

// PlatformUpdate handles PATCH /api/v1/tenants/{tenant_id}/api-keys/{id}:
// set or clear the key's limits or profile binding, as PATCH /api-keys/{id}
// does for the caller's own tenant.
func (h APIKeys) PlatformUpdate(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.platformKeyTenant(w, r)
	if !ok {
		return
	}
	h.update(w, r, tid)
}

// PlatformRevoke handles DELETE /api/v1/tenants/{tenant_id}/api-keys/{id}.
func (h APIKeys) PlatformRevoke(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.platformKeyTenant(w, r)
	if !ok {
		return
	}
	h.revoke(w, r, tid)
}

// PlatformRotate handles POST
// /api/v1/tenants/{tenant_id}/api-keys/{id}/rotate.
func (h APIKeys) PlatformRotate(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.platformKeyTenant(w, r)
	if !ok {
		return
	}
	if id := h.rotate(w, r, tid); id != "" {
		AuditNote(r.Context(), "new_key_id", id)
	}
}
