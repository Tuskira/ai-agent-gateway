package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"

	"github.com/go-chi/chi/v5"

	dataplaneheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// newTestDeps builds a Deps over a fresh in-memory fakeStore, a real
// secrets.Service (over a real AES key, so Create/Get/Rotate round-trip
// exactly like production), and a real header Registry with the
// secret_store provider registered (needed by connector tests that
// reference a credential).
func newTestDeps() Deps {
	fs := newFakeStore()
	ring := &secrets.KeyRing{
		Keys:        map[string][]byte{"k1": bytes.Repeat([]byte{0x42}, 32)},
		ActiveKeyID: "k1",
	}
	svc := secrets.NewService(fs.Credentials(), ring)

	registry := dataplaneheaders.NewRegistry()
	if err := registry.RegisterExternal(secrets.NewSecretStoreProvider(svc)); err != nil {
		panic(err)
	}

	return Deps{
		Store:   fs,
		Secrets: svc,
		Headers: registry,
		// Mirrors pkgauth.NewRoleAuthorizer's built-ins, since these
		// tests don't go through router.go's allowedRoles(deps.Authorizer)
		// wiring. Tests that need a custom role set (or KeyInvalidator)
		// build their own Deps instead of calling this helper.
		AllowedRoles: map[string]bool{"admin": true, "agent": true},
	}
}

// withPrincipal attaches a test Principal to r's context.
func withPrincipal(r *http.Request, tenantID string, roles ...string) *http.Request {
	p := &pkgauth.Principal{
		Subject: "test-principal", TenantID: tenantID, Roles: roles, AuthMethod: "test",
	}
	return r.WithContext(pkgauth.WithPrincipal(r.Context(), p))
}

// serve mounts h at pattern/method on a fresh chi router (so
// chi.URLParam works inside the handler) and returns the recorded
// response for one request.
func serve(method, pattern string, h http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.MethodFunc(method, pattern, h)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
