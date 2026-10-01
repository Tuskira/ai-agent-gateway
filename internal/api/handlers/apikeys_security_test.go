package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func keyDeps() Deps {
	d := newTestDeps()
	d.Authorizer = pkgauth.NewRoleAuthorizer()
	d.AllowedRoles = map[string]bool{"admin": true, "agent": true, pkgauth.RolePlatformAdmin: true}
	return d
}

func keyCall(h http.HandlerFunc, method, pattern, target, body string, tenant string, roles ...string) *httptest.ResponseRecorder {
	req := withPrincipal(httptest.NewRequest(method, target, strings.NewReader(body)), tenant, roles...)
	return serve(method, pattern, h, req)
}

// A tenant admin must not be able to mint (or rotate into existence) a
// platform-admin key; a platform-admin can.
func TestAPIKeys_PlatformRoleNeedsPlatformAdmin(t *testing.T) {
	d := keyDeps()
	h := APIKeys{Deps: d}
	body := `{"name":"p","role":"platform-admin"}`

	if w := keyCall(h.Create, http.MethodPost, "/api-keys", "/api-keys", body, "tenant-a", "admin"); w.Code != http.StatusForbidden {
		t.Fatalf("tenant admin creating platform-admin key = %d, want 403: %s", w.Code, w.Body.String())
	}
	w := keyCall(h.Create, http.MethodPost, "/api-keys", "/api-keys", body, "tenant-a", "admin", pkgauth.RolePlatformAdmin)
	if w.Code != http.StatusCreated {
		t.Fatalf("platform admin creating platform-admin key = %d: %s", w.Code, w.Body.String())
	}
	var created apiKeyCreateView
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	// A tenant admin rotating that platform key would otherwise obtain a
	// fresh platform credential.
	if w := keyCall(h.Rotate, http.MethodPost, "/api-keys/{id}/rotate", "/api-keys/"+created.ID+"/rotate", "", "tenant-a", "admin"); w.Code != http.StatusForbidden {
		t.Errorf("tenant admin rotating a platform key = %d, want 403", w.Code)
	}
	if w := keyCall(h.Rotate, http.MethodPost, "/api-keys/{id}/rotate", "/api-keys/"+created.ID+"/rotate", "", "tenant-a", "admin", pkgauth.RolePlatformAdmin); w.Code != http.StatusOK {
		t.Errorf("platform admin rotating a platform key = %d, want 200", w.Code)
	}

	// An ordinary role is unaffected.
	if w := keyCall(h.Create, http.MethodPost, "/api-keys", "/api-keys", `{"name":"a","role":"agent"}`, "tenant-a", "admin"); w.Code != http.StatusCreated {
		t.Errorf("tenant admin creating agent key = %d, want 201", w.Code)
	}
}

// A custom role configured with a platform.* pattern is guarded too.
func TestAPIKeys_CustomPlatformRoleGuarded(t *testing.T) {
	d := keyDeps()
	ra := pkgauth.NewRoleAuthorizer()
	ra.Rules["ops"] = []string{"platform.catalog.manage"}
	d.Authorizer = ra
	d.AllowedRoles["ops"] = true
	h := APIKeys{Deps: d}
	if w := keyCall(h.Create, http.MethodPost, "/api-keys", "/api-keys", `{"name":"o","role":"ops"}`, "tenant-a", "admin"); w.Code != http.StatusForbidden {
		t.Errorf("tenant admin creating custom platform role key = %d, want 403", w.Code)
	}
}

func seedProfile(t *testing.T, d Deps, tenant, name string) string {
	t.Helper()
	p := &store.AgentProfile{TenantID: tenant, Name: name, Slug: tenant + "-" + name}
	if err := d.Store.AgentProfiles().Create(context.Background(), p); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	return p.ID
}

func TestAPIKeys_BindToProfile(t *testing.T) {
	d := keyDeps()
	h := APIKeys{Deps: d}
	mine := seedProfile(t, d, "tenant-a", "alpha")
	theirs := seedProfile(t, d, "tenant-b", "beta")

	// Another tenant's profile (and junk) is rejected.
	for _, bad := range []string{theirs, "not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		w := keyCall(h.Create, http.MethodPost, "/api-keys", "/api-keys", `{"name":"x","role":"agent","profile_id":"`+bad+`"}`, "tenant-a", "admin")
		if w.Code != http.StatusBadRequest {
			t.Errorf("profile_id %q = %d, want 400: %s", bad, w.Code, w.Body.String())
		}
	}

	w := keyCall(h.Create, http.MethodPost, "/api-keys", "/api-keys", `{"name":"x","role":"agent","profile_id":"`+mine+`"}`, "tenant-a", "admin")
	if w.Code != http.StatusCreated {
		t.Fatalf("create bound key = %d: %s", w.Code, w.Body.String())
	}
	var created apiKeyCreateView
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.ProfileID != mine || created.ProfileName != "alpha" {
		t.Errorf("view = %+v, want profile %s/alpha", created.apiKeyView, mine)
	}
	stored, _ := d.Store.APIKeys().GetByID(context.Background(), "tenant-a", created.ID)
	if stored.ProfileID == nil || *stored.ProfileID != mine {
		t.Fatalf("stored ProfileID = %v", stored.ProfileID)
	}

	// Rotation carries the binding.
	w = keyCall(h.Rotate, http.MethodPost, "/api-keys/{id}/rotate", "/api-keys/"+created.ID+"/rotate", "", "tenant-a", "admin")
	var rotated apiKeyCreateView
	_ = json.Unmarshal(w.Body.Bytes(), &rotated)
	if w.Code != http.StatusOK || rotated.ProfileID != mine {
		t.Errorf("rotate = %d profile %q, want binding kept", w.Code, rotated.ProfileID)
	}

	// PATCH: clear, then rebind; cross-tenant id rejected.
	if w := keyCall(h.Update, http.MethodPatch, "/api-keys/{id}", "/api-keys/"+rotated.ID, `{"profile_id":null}`, "tenant-a", "admin"); w.Code != http.StatusOK {
		t.Fatalf("PATCH clear = %d: %s", w.Code, w.Body.String())
	}
	if k, _ := d.Store.APIKeys().GetByID(context.Background(), "tenant-a", rotated.ID); k.ProfileID != nil {
		t.Errorf("profile_id not cleared: %v", *k.ProfileID)
	}
	if w := keyCall(h.Update, http.MethodPatch, "/api-keys/{id}", "/api-keys/"+rotated.ID, `{"profile_id":"`+theirs+`"}`, "tenant-a", "admin"); w.Code != http.StatusBadRequest {
		t.Errorf("PATCH foreign profile = %d, want 400", w.Code)
	}
	if w := keyCall(h.Update, http.MethodPatch, "/api-keys/{id}", "/api-keys/"+rotated.ID, `{"profile_id":"`+mine+`"}`, "tenant-a", "admin"); w.Code != http.StatusOK {
		t.Errorf("PATCH rebind = %d: %s", w.Code, w.Body.String())
	}
	if w := keyCall(h.Update, http.MethodPatch, "/api-keys/{id}", "/api-keys/"+rotated.ID, `{}`, "tenant-a", "admin"); w.Code != http.StatusBadRequest {
		t.Errorf("PATCH empty = %d, want 400", w.Code)
	}
}
