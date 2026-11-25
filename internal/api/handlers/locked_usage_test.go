package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// A tenant model's base_url is the tenant admin's to change, so no tenant
// model may name a platform-locked credential, whoever saves it.
func TestModels_LockedCredentialIsRefused(t *testing.T) {
	deps, fs, _ := newModelDeps(t)
	deps.Authorizer = pkgauth.NewRoleAuthorizer()
	h := Models{Deps: deps}
	if err := deps.Secrets.SetLocked(context.Background(), "tenant-a", "anthropic-prod", true); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"admin", pkgauth.RolePlatformAdmin} {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/models", strings.NewReader(modelCreateBody)), "tenant-a", role)
		w := serve(http.MethodPost, "/models", h.Create, req)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), managedByPlatformMsg) {
			t.Errorf("%s: create = %d %s, want 403 %q", role, w.Code, w.Body.String(), managedByPlatformMsg)
		}
	}
	if ms, _ := fs.Models().List(context.Background(), "tenant-a", store.ListOptions{}); len(ms) != 1 { // the platform "haiku" only
		t.Errorf("models after refused creates = %d, want only the platform row", len(ms))
	}
}

// Catalog connect with an existing locked credential would create tenant
// models wired to it: refused.
func TestModelCatalog_Connect_LockedCredentialIsRefused(t *testing.T) {
	deps, fs, providerID, enabledModelID, _ := newCatalogDeps(t)
	h := ModelCatalog{Deps: deps}
	if _, err := deps.Secrets.CreateLocked(context.Background(), "tenant-a", "plat-key", "api_key", map[string]string{"api_key": "sk-platform"}, "platform", true); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"credential":{"name":"plat-key"},"models":["%s"]}`, enabledModelID)
	for _, role := range []string{"admin", pkgauth.RolePlatformAdmin} {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/connect", strings.NewReader(body)), "tenant-a", role)
		w := doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/connect", "", h.Connect, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: connect = %d %s, want 403", role, w.Code, w.Body.String())
		}
	}
	if ms, _ := fs.Models().List(context.Background(), "tenant-a", store.ListOptions{}); len(ms) != 0 {
		t.Errorf("tenant models after refused connect = %d, want 0", len(ms))
	}
}

// The provider test route reads a locked credential only for a platform
// admin; a tenant admin's request never reaches the provider.
func TestModelCatalog_Test_LockedCredentialNeedsPlatformAdmin(t *testing.T) {
	var hits int
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsListJSON("acme/large-v1")))
	}))
	t.Cleanup(srv.Close)

	deps, fs, providerID, _, _ := newCatalogDeps(t)
	p, _ := fs.ModelCatalog().GetProvider(context.Background(), providerID)
	p.BaseURL = srv.URL
	if err := fs.ModelCatalog().UpdateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Secrets.CreateLocked(context.Background(), "tenant-a", "plat-key", "api_key", map[string]string{"api_key": "sk-platform"}, "platform", true); err != nil {
		t.Fatal(err)
	}
	h := ModelCatalog{Deps: deps}
	call := func(role string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/model-catalog/providers/"+providerID+"/test", strings.NewReader(`{"credential":"plat-key"}`)), "tenant-a", role)
		return doJSON(t, http.MethodPost, "/model-catalog/providers/{id}/test", "", h.Test, req)
	}

	if w := call("admin"); w.Code != http.StatusForbidden {
		t.Errorf("tenant admin test = %d %s, want 403", w.Code, w.Body.String())
	}
	if hits != 0 {
		t.Fatalf("provider was called %d times for the tenant admin", hits)
	}
	if w := call(pkgauth.RolePlatformAdmin); w.Code != http.StatusOK {
		t.Fatalf("platform admin test = %d %s, want 200", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer sk-platform" {
		t.Errorf("provider saw Authorization = %q, want the locked credential", gotAuth)
	}
}
