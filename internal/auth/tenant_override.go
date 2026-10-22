package auth

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// ActingTenantHeader names the tenant a platform-admin acts in for one
// request. Its value is the tenant's UUID. It is the only request header
// that can select a tenant, and only for a principal holding
// pkgauth.PermPlatformAdmin (see TenantOverride).
const ActingTenantHeader = "X-Gateway-Acting-Tenant"

// TenantOverride is the single choke point for the acting-tenant header.
// Mount it after the authentication middleware (and, on the API plane,
// after the CSRF and must-change-password gates, so those still run on
// the real account first).
//
//   - Header absent: the request passes through untouched.
//   - Header present but z does not grant the principal platform.admin:
//     403 permission_error. A 403 rather than a silent ignore, because
//     acting on the home tenant unnoticed is the dangerous failure mode
//     (a console bug would write to the wrong tenant). The console never
//     sends the header for non-platform sessions, so ordinary users never
//     see this. API keys get the same treatment: a plain admin key sending
//     the header is refused too.
//   - Header present and permitted: the value must be a UUID (400
//     validation_error) naming an existing tenant (404 not_found).
//   - Success: the context Principal is replaced by a copy whose TenantID
//     is the acting tenant and whose HomeTenantID is the principal's own,
//     so every tenant-scoped handler is scoped to the acting tenant while
//     self-service routes and audit attribution can still use Home(). The
//     request logger gains acting_tenant_id.
//
// The header selecting the principal's own tenant is accepted and is a
// no-op: HomeTenantID stays empty.
func TenantOverride(z pkgauth.Authorizer, tenants store.TenantStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			acting := r.Header.Get(ActingTenantHeader)
			if acting == "" {
				next.ServeHTTP(w, r)
				return
			}

			ctx := r.Context()
			p, _ := pkgauth.PrincipalFrom(ctx)
			if z == nil || !z.Allow(ctx, p, pkgauth.PermPlatformAdmin) {
				writeError(w, http.StatusForbidden, "permission_error", "acting tenant requires platform.admin")
				return
			}
			if _, err := uuid.Parse(acting); err != nil {
				writeError(w, http.StatusBadRequest, "validation_error", ActingTenantHeader+" must be a tenant UUID")
				return
			}
			if tenants == nil {
				// Nothing to look tenants up in: refuse rather than act on
				// an unverified tenant id.
				writeError(w, http.StatusNotFound, "not_found", "tenant not found")
				return
			}
			if _, err := tenants.GetByID(ctx, acting); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writeError(w, http.StatusNotFound, "not_found", "tenant not found")
					return
				}
				applog.From(ctx).Error("acting tenant lookup failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
				return
			}

			if acting == p.TenantID {
				next.ServeHTTP(w, r)
				return
			}

			cp := *p
			cp.HomeTenantID = p.Home()
			cp.TenantID = acting
			ctx = pkgauth.WithPrincipal(ctx, &cp)
			ctx = applog.With(ctx, applog.From(ctx).With("acting_tenant_id", acting))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
