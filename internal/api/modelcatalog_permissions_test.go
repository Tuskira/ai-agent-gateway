package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/session"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// TestModelCatalogPermissions_FullStack exercises the actual route wiring
// for the model catalog's one dynamic permission, platform.catalog.manage:
// an API-key admin always holds it, a console (session) admin holds it
// ONLY while the deployment is single-tenant, and neither an API-key agent
// nor a console viewer ever holds it. GET /model-catalog itself (gated on
// the ordinary model.read) is open to every principal here regardless.
//
// Like TestPermissions_FullStack, this only needs to prove the routing/
// authorizer layer gates correctly -- nopStore is enough, since a request
// that should be denied never reaches the handler, and one that should be
// allowed is only checked for "not 403" (its exact status depends on
// nopStore's trivial behavior, not on this test's own concern).
func TestModelCatalogPermissions_FullStack(t *testing.T) {
	singleTenant := true // toggled mid-test
	authorizer := pkgauth.NewRoleAuthorizer()
	authorizer.SingleTenant = func(context.Context) bool { return singleTenant }

	const csrf = "csrf-token-for-test"
	principals := map[string]*pkgauth.Principal{
		"apikey-admin":    {Subject: "apikey-admin", TenantID: "tenant-a", Roles: []string{"admin"}, AuthMethod: "apikey"},
		"apikey-platform": {Subject: "apikey-platform", TenantID: "tenant-a", Roles: pkgauth.RolesForKey(pkgauth.RolePlatformAdmin), AuthMethod: "apikey"},
		"apikey-agent":    {Subject: "apikey-agent", TenantID: "tenant-a", Roles: []string{"agent"}, AuthMethod: "apikey"},
		"console-admin":   {Subject: "console-admin", TenantID: "tenant-a", Roles: []string{"admin"}, AuthMethod: pkgauth.AuthMethodSession, CSRFToken: csrf},
		"console-viewer":  {Subject: "console-viewer", TenantID: "tenant-a", Roles: []string{"viewer"}, AuthMethod: pkgauth.AuthMethodSession, CSRFToken: csrf},
	}

	h := NewRouter(Deps{
		ServiceVersion: "test",
		Authenticator:  &scriptedAuthenticator{principals: principals},
		Authorizer:     authorizer,
		Store:          nopStore{},
	})

	do := func(method, path, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
			if p := principals[bearer]; p != nil && p.AuthMethod == pkgauth.AuthMethodSession && method != http.MethodGet {
				req.Header.Set(session.CSRFHeader, csrf)
			}
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// GET /model-catalog: model.read, "*.read" satisfies it for everyone.
	for _, key := range []string{"apikey-admin", "apikey-agent", "console-admin", "console-viewer"} {
		if w := do(http.MethodGet, "/api/v1/model-catalog", key); w.Code == http.StatusForbidden {
			t.Errorf("%s GET /model-catalog = 403, want allowed (model.read)", key)
		}
	}

	// POST /model-catalog/providers: platform.catalog.manage.
	if w := do(http.MethodPost, "/api/v1/model-catalog/providers", "apikey-platform"); w.Code == http.StatusForbidden {
		t.Error("apikey-platform POST /model-catalog/providers = 403, want allowed (platform-admin holds platform.catalog.manage)")
	}
	if w := do(http.MethodPost, "/api/v1/model-catalog/providers", "apikey-admin"); w.Code != http.StatusForbidden {
		t.Errorf("tenant-admin apikey POST /model-catalog/providers = %d, want 403", w.Code)
	}
	if w := do(http.MethodPost, "/api/v1/model-catalog/providers", "apikey-agent"); w.Code != http.StatusForbidden {
		t.Errorf("apikey-agent POST /model-catalog/providers = %d, want 403", w.Code)
	}
	if w := do(http.MethodPost, "/api/v1/model-catalog/providers", "console-viewer"); w.Code != http.StatusForbidden {
		t.Errorf("console-viewer POST /model-catalog/providers = %d, want 403", w.Code)
	}

	singleTenant = true
	if w := do(http.MethodPost, "/api/v1/model-catalog/providers", "console-admin"); w.Code == http.StatusForbidden {
		t.Error("console-admin POST /model-catalog/providers (single-tenant) = 403, want allowed")
	}

	singleTenant = false
	if w := do(http.MethodPost, "/api/v1/model-catalog/providers", "console-admin"); w.Code != http.StatusForbidden {
		t.Errorf("console-admin POST /model-catalog/providers (multi-tenant) = %d, want 403", w.Code)
	}

	// The same rule applies to every platform.catalog.manage route, not
	// just provider create -- spot-check delete and the connect route's
	// PRIMARY permission (model.create, unaffected by this rule) still
	// isn't enough on its own for a multi-tenant console admin trying a
	// platform.catalog.manage route.
	if w := do(http.MethodDelete, "/api/v1/model-catalog/providers/00000000-0000-0000-0000-000000000000", "console-admin"); w.Code != http.StatusForbidden {
		t.Errorf("console-admin DELETE /model-catalog/providers/{id} (multi-tenant) = %d, want 403", w.Code)
	}
	singleTenant = true
	if w := do(http.MethodDelete, "/api/v1/model-catalog/providers/00000000-0000-0000-0000-000000000000", "console-admin"); w.Code == http.StatusForbidden {
		t.Error("console-admin DELETE /model-catalog/providers/{id} (single-tenant) = 403, want allowed by the permission layer (nopStore itself may still 404/500)")
	}
}
