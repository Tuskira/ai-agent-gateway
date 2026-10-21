package handlers

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
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
// tenant. It writes a 404 and returns false when there is none. The store
// has no get-by-id, so this scans the tenant list (platform-admin only,
// low volume); an id that is not a tenant -- including a malformed one --
// is simply not found.
func (h APIKeys) platformKeyTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "tenant_id")
	tenants, err := h.Store.Tenants().List(r.Context())
	if err != nil {
		writeStoreErr(w, r, "look up tenant", err)
		return "", false
	}
	for _, t := range tenants {
		if t.ID == id {
			return t.ID, true
		}
	}
	httpx.WriteError(w, http.StatusNotFound, httpx.TypeNotFound, "tenant not found")
	return "", false
}

// auditPlatformKeyAction records who did what to which tenant's keys. The
// API plane has no access-log sink; the acting principal also lands in the
// key row's created_by for create and rotate.
func auditPlatformKeyAction(r *http.Request, action, tenantID, keyID string) {
	actor, method, actorTenant := "", "", ""
	if p, ok := pkgauth.PrincipalFrom(r.Context()); ok {
		actor, method, actorTenant = p.Subject, p.AuthMethod, p.TenantID
	}
	slog.InfoContext(r.Context(), "platform api-key action",
		"action", action,
		"actor", actor,
		"actor_auth_method", method,
		"actor_tenant_id", actorTenant,
		"target_tenant_id", tenantID,
		"key_id", keyID,
	)
}

// PlatformCreate handles POST /api/v1/tenants/{tenant_id}/api-keys.
func (h APIKeys) PlatformCreate(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.platformKeyTenant(w, r)
	if !ok {
		return
	}
	if id := h.create(w, r, tid); id != "" {
		auditPlatformKeyAction(r, "create", tid, id)
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

// PlatformRevoke handles DELETE /api/v1/tenants/{tenant_id}/api-keys/{id}.
func (h APIKeys) PlatformRevoke(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.platformKeyTenant(w, r)
	if !ok {
		return
	}
	if id := h.revoke(w, r, tid); id != "" {
		auditPlatformKeyAction(r, "revoke", tid, id)
	}
}

// PlatformRotate handles POST
// /api/v1/tenants/{tenant_id}/api-keys/{id}/rotate.
func (h APIKeys) PlatformRotate(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.platformKeyTenant(w, r)
	if !ok {
		return
	}
	if id := h.rotate(w, r, tid); id != "" {
		auditPlatformKeyAction(r, "rotate", tid, id)
	}
}
