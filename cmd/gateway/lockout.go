package main

import (
	"context"
	"log/slog"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// adminCounter is the slice of the store warnAdminlessTenants needs.
type adminCounter interface {
	Tenants() store.TenantStore
	Users() store.UserStore
}

// warnAdminlessTenants logs one WARN per tenant that has no active admin
// user, but only when console API-key login is off: then nobody can sign
// in to that tenant's console. It returns the slugs it warned about. A
// store error is logged and skips the check; it never blocks startup.
func warnAdminlessTenants(ctx context.Context, st adminCounter, apiKeyLogin bool, logger *slog.Logger) []string {
	if apiKeyLogin {
		return nil
	}
	tenants, err := st.Tenants().List(ctx)
	if err != nil {
		logger.Warn("admin lockout check skipped: list tenants failed", "error", err)
		return nil
	}
	var warned []string
	for _, t := range tenants {
		n, err := st.Users().CountActiveAdmins(ctx, t.ID)
		if err != nil {
			logger.Warn("admin lockout check skipped: count admins failed", "tenant", t.Slug, "error", err)
			continue
		}
		if n == 0 {
			logger.Warn("console API-key login is disabled and tenant " + t.Slug +
				" has no active admin user; create one with 'gateway create-user'")
			warned = append(warned, t.Slug)
		}
	}
	return warned
}
