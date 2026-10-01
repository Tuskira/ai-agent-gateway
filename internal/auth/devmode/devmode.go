// Package devmode implements a gap-filling Authenticator for local
// development: when a request carries no credential at all, it manufactures
// an admin Principal instead of rejecting the request, loudly logging every
// time it does so. It must never be enabled in a shared environment.
package devmode

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// authenticator is the devmode Authenticator. Construct with New.
type authenticator struct {
	tenantID string
	roles    []string
	logger   *slog.Logger
}

var _ auth.Authenticator = (*authenticator)(nil)

// New returns an Authenticator that only fills the gap left by every other
// Authenticator in a chain: it fires solely when the incoming request
// carries no credential whatsoever (no Authorization header, no
// X-Gateway-Key), in which case it grants an admin Principal scoped to
// tenantID and logs a WARN every time, so its presence in a chain is loud
// and unmissable in logs. When a credential IS present -- even one this
// Authenticator can't parse -- it returns auth.ErrNoCredential so the
// chain's real Authenticators are the ones to accept or reject it.
//
// The principal is a tenant admin only; platform is true to also grant
// platform-admin (auth.dev_mode.platform).
func New(tenantID string, platform bool, logger *slog.Logger) auth.Authenticator {
	if logger == nil {
		logger = slog.Default()
	}
	roles := []string{"admin"}
	if platform {
		roles = auth.RolesForKey(auth.RolePlatformAdmin)
	}
	return &authenticator{tenantID: tenantID, roles: roles, logger: logger}
}

// Name identifies this Authenticator within an auth.Chain.
func (a *authenticator) Name() string { return "dev" }

// Authenticate grants an admin Principal when r carries no credential at
// all, and defers (ErrNoCredential) otherwise.
func (a *authenticator) Authenticate(_ context.Context, r *http.Request) (*auth.Principal, error) {
	if hasCredential(r) {
		return nil, auth.ErrNoCredential
	}

	a.logger.Warn("dev-mode authentication: granting admin principal for unauthenticated request; do not enable this outside local development",
		"tenant_id", a.tenantID, "path", r.URL.Path, "method", r.Method)

	return &auth.Principal{
		Subject:    "dev",
		TenantID:   a.tenantID,
		Roles:      a.roles,
		AuthMethod: "dev",
	}, nil
}

// hasCredential reports whether r carries any credential a "real"
// Authenticator might recognize, regardless of whether it turns out to be
// valid. devmode only fills the total absence of a credential.
func hasCredential(r *http.Request) bool {
	return r.Header.Get("Authorization") != "" || r.Header.Get("X-Gateway-Key") != ""
}
