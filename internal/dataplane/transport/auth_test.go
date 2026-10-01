package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

func TestAuthMiddleware_DeniesRoleWithoutMCPAccess(t *testing.T) {
	reader := &pkgauth.Principal{Subject: "r", TenantID: "t", Roles: []string{"reader"}, AuthMethod: "test"}
	z := pkgauth.NewRoleAuthorizer()
	z.Rules["reader"] = []string{"*.read"}
	h := AuthMiddleware(fixedAuthenticator{p: reader}, z, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reader role on MCP = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	agent := &pkgauth.Principal{Subject: "a", TenantID: "t", Roles: []string{"agent"}, AuthMethod: "test"}
	h = AuthMiddleware(fixedAuthenticator{p: agent}, z, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("agent role on MCP = %d, want 200", rec.Code)
	}
}

type fixedAuthenticator struct{ p *pkgauth.Principal }

func (f fixedAuthenticator) Name() string { return "fixed" }
func (f fixedAuthenticator) Authenticate(_ context.Context, _ *http.Request) (*pkgauth.Principal, error) {
	return f.p, nil
}
