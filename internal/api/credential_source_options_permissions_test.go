package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// TestPermissions_CredentialSourceOptions pins the options route to
// connector.update (the connector editor's permission) while the rest of
// /credential-sources stays admin.manage. The request carries no "field",
// so an allowed caller is stopped by validation (400) before any store
// access, which is all the permission layer needs to be proven.
func TestPermissions_CredentialSourceOptions(t *testing.T) {
	authorizer := pkgauth.NewRoleAuthorizer()
	authorizer.Rules["connector-editor"] = []string{"connector.*"}

	principals := map[string]*pkgauth.Principal{
		"admin":  {Subject: "admin", TenantID: "tenant-a", Roles: []string{"admin"}, AuthMethod: "test"},
		"editor": {Subject: "editor", TenantID: "tenant-a", Roles: []string{"connector-editor"}, AuthMethod: "test"},
		"agent":  {Subject: "agent", TenantID: "tenant-a", Roles: []string{"agent"}, AuthMethod: "test"},
		"viewer": {Subject: "viewer", TenantID: "tenant-a", Roles: []string{"viewer"}, AuthMethod: "test"},
	}
	h := NewRouter(Deps{
		ServiceVersion: "test",
		Authenticator:  &scriptedAuthenticator{principals: principals},
		Authorizer:     authorizer,
		Store:          nopStore{},
	})
	do := func(method, path, bearer string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	const options = "/api/v1/credential-sources/7f3c2a10-8b1e-4c55-9d3a-0123456789ab/options"
	for who, want := range map[string]int{
		"admin":  http.StatusBadRequest,
		"editor": http.StatusBadRequest,
		"agent":  http.StatusForbidden,
		"viewer": http.StatusForbidden,
	} {
		if got := do(http.MethodGet, options, who); got != want {
			t.Errorf("%s GET %s = %d, want %d", who, options, got, want)
		}
	}

	// The editor role does not widen the other source routes.
	for _, path := range []string{"/api/v1/credential-sources", "/api/v1/credential-sources/7f3c2a10-8b1e-4c55-9d3a-0123456789ab"} {
		if got := do(http.MethodGet, path, "editor"); got != http.StatusForbidden {
			t.Errorf("editor GET %s = %d, want 403", path, got)
		}
	}
	if got := do(http.MethodPost, "/api/v1/credential-sources/7f3c2a10-8b1e-4c55-9d3a-0123456789ab/test", "editor"); got != http.StatusForbidden {
		t.Errorf("editor POST .../test = %d, want 403", got)
	}
}
