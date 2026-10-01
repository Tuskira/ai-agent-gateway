package handlers

import (
	"errors"
	"net/http"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// writeStoreErr maps a store-layer error onto the shared HTTP error
// envelope: store.ErrNotFound -> 404, store.ErrConflict -> 409, anything
// else -> 500 (logged server-side via the request-scoped logger in ctx;
// never echoed to the caller).
func writeStoreErr(w http.ResponseWriter, r *http.Request, action string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.NotFound(w, "not found")
	case errors.Is(err, store.ErrConflict):
		httpx.Conflict(w, "already exists")
	default:
		applog.From(r.Context()).Error(action+" failed", "error", err)
		httpx.Internal(w, "internal error")
	}
}

// tenantID returns the caller's tenant id from the request context.
// Callers must run behind internalauth.Middleware, which always attaches
// a Principal on success; ok is false only if that invariant is somehow
// broken.
func tenantID(r *http.Request) (string, bool) {
	p, ok := pkgauth.PrincipalFrom(r.Context())
	if !ok || p == nil || p.TenantID == "" {
		return "", false
	}
	return p.TenantID, true
}

// requireTenant writes a 500 (an internal invariant violation, not a
// caller error -- RequirePermission already guarantees an authenticated
// Principal reached this handler) and returns false if the request has no
// tenant.
func requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tid, ok := tenantID(r)
	if !ok {
		httpx.Internal(w, "no authenticated tenant in context")
		return "", false
	}
	return tid, true
}

// principalSubject returns the caller's Principal.Subject (e.g. an API
// key id), or "" if somehow absent. Used to stamp CreatedBy on new rows.
func principalSubject(r *http.Request) string {
	p, ok := pkgauth.PrincipalFrom(r.Context())
	if !ok || p == nil {
		return ""
	}
	return p.Subject
}
