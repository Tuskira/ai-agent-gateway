package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const (
	platKeys    = "/tenants/{tenant_id}/api-keys"
	platKeyByID = "/tenants/{tenant_id}/api-keys/{id}"
	platRotate  = "/tenants/{tenant_id}/api-keys/{id}/rotate"
)

func seedTenant(t *testing.T, d Deps, slug string) string {
	t.Helper()
	tn := &store.Tenant{Slug: slug, Name: slug}
	if err := d.Store.Tenants().Create(context.Background(), tn); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return tn.ID
}

// The platform route mints a key that lives in the TARGET tenant, not the
// caller's, with the same limits/role/expiry/profile rules, a one-time
// plaintext, and the acting principal in created_by.
func TestAPIKeys_PlatformCreate_LandsInTargetTenant(t *testing.T) {
	d := keyDeps()
	h := APIKeys{Deps: d}
	home := seedTenant(t, d, "home")
	target := seedTenant(t, d, "target")
	profile := seedProfile(t, d, target, "alpha")

	body := `{"name":"per-tenant","role":"agent","expires_at":"2099-01-01T00:00:00Z","limits":{"rpm":30},"profile_id":"` + profile + `"}`
	w := keyCall(h.PlatformCreate, http.MethodPost, platKeys, "/tenants/"+target+"/api-keys", body, home, pkgauth.RolesForKey(pkgauth.RolePlatformAdmin)...)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "key_hash") {
		t.Fatalf("response leaked key_hash: %s", w.Body.String())
	}
	var got apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Key, apikey.Prefix) || got.Role != "agent" || got.ProfileID != profile ||
		got.ExpiresAt != "2099-01-01T00:00:00Z" || got.Limits == nil || *got.Limits.RPM != 30 {
		t.Fatalf("view = %+v", got)
	}

	stored, err := d.Store.APIKeys().GetByID(context.Background(), target, got.ID)
	if err != nil {
		t.Fatalf("key not in target tenant: %v", err)
	}
	if stored.TenantID != target || stored.CreatedBy != "test-principal" || stored.KeyHash != apikey.Hash(got.Key) {
		t.Errorf("stored = %+v", stored)
	}
	if _, err := d.Store.APIKeys().GetByID(context.Background(), home, got.ID); err == nil {
		t.Error("key also visible in the caller's own tenant")
	}

	// The caller's own-tenant list does not show it; the target's does.
	if keys, _ := d.Store.APIKeys().List(context.Background(), home); len(keys) != 0 {
		t.Errorf("home tenant has %d keys, want 0", len(keys))
	}
	w = keyCall(h.PlatformList, http.MethodGet, platKeys, "/tenants/"+target+"/api-keys", "", home, pkgauth.RolesForKey(pkgauth.RolePlatformAdmin)...)
	var page struct {
		Items []apiKeyView `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page)
	if w.Code != http.StatusOK || len(page.Items) != 1 || page.Items[0].ID != got.ID || strings.Contains(w.Body.String(), got.Key) {
		t.Errorf("list = %d %s", w.Code, w.Body.String())
	}
}

// Validation is the same as the own-tenant route, and a profile of another
// tenant is refused: the profile lookup is scoped to the target tenant.
func TestAPIKeys_PlatformCreate_ValidationUsesTargetTenant(t *testing.T) {
	d := keyDeps()
	h := APIKeys{Deps: d}
	home := seedTenant(t, d, "home")
	target := seedTenant(t, d, "target")
	homeProfile := seedProfile(t, d, home, "mine")
	roles := pkgauth.RolesForKey(pkgauth.RolePlatformAdmin)

	for name, body := range map[string]string{
		"bad role":         `{"name":"x","role":"superuser"}`,
		"empty limits":     `{"name":"x","role":"agent","limits":{}}`,
		"past expires_at":  `{"name":"x","role":"agent","expires_at":"2000-01-01T00:00:00Z"}`,
		"caller's profile": `{"name":"x","role":"agent","profile_id":"` + homeProfile + `"}`,
	} {
		w := keyCall(h.PlatformCreate, http.MethodPost, platKeys, "/tenants/"+target+"/api-keys", body, home, roles...)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400: %s", name, w.Code, w.Body.String())
		}
	}
	if keys, _ := d.Store.APIKeys().List(context.Background(), target); len(keys) != 0 {
		t.Errorf("rejected requests created %d keys", len(keys))
	}
}

func TestAPIKeys_PlatformRoutes_UnknownTenant404(t *testing.T) {
	d := keyDeps()
	h := APIKeys{Deps: d}
	roles := pkgauth.RolesForKey(pkgauth.RolePlatformAdmin)
	for _, tc := range []struct {
		name    string
		h       http.HandlerFunc
		method  string
		pattern string
		target  string
		body    string
	}{
		{"create", h.PlatformCreate, http.MethodPost, platKeys, "/tenants/nope/api-keys", `{"name":"x","role":"agent"}`},
		{"list", h.PlatformList, http.MethodGet, platKeys, "/tenants/nope/api-keys", ""},
		{"revoke", h.PlatformRevoke, http.MethodDelete, platKeyByID, "/tenants/nope/api-keys/k1", ""},
		{"rotate", h.PlatformRotate, http.MethodPost, platRotate, "/tenants/nope/api-keys/k1/rotate", ""},
	} {
		w := keyCall(tc.h, tc.method, tc.pattern, tc.target, tc.body, "home", roles...)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404: %s", tc.name, w.Code, w.Body.String())
		}
		assertErrorType(t, w, "not_found")
	}
}

// Revoke and rotate only reach keys of the named tenant: a key id from
// another tenant is a 404 and is left untouched.
func TestAPIKeys_PlatformRevokeRotate_ScopedToNamedTenant(t *testing.T) {
	d := keyDeps()
	h := APIKeys{Deps: d}
	a := seedTenant(t, d, "a")
	b := seedTenant(t, d, "b")
	roles := pkgauth.RolesForKey(pkgauth.RolePlatformAdmin)

	create := func(tenant string) apiKeyCreateView {
		w := keyCall(h.PlatformCreate, http.MethodPost, platKeys, "/tenants/"+tenant+"/api-keys", `{"name":"k","role":"agent"}`, "home", roles...)
		var v apiKeyCreateView
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d: %s", w.Code, w.Body.String())
		}
		return v
	}
	ka, kb := create(a), create(b)

	if w := keyCall(h.PlatformRevoke, http.MethodDelete, platKeyByID, "/tenants/"+b+"/api-keys/"+ka.ID, "", "home", roles...); w.Code != http.StatusNotFound {
		t.Errorf("revoke a's key via b = %d, want 404", w.Code)
	}
	if w := keyCall(h.PlatformRotate, http.MethodPost, platRotate, "/tenants/"+b+"/api-keys/"+ka.ID+"/rotate", "", "home", roles...); w.Code != http.StatusNotFound {
		t.Errorf("rotate a's key via b = %d, want 404", w.Code)
	}
	if k, _ := d.Store.APIKeys().GetByID(context.Background(), a, ka.ID); k == nil || k.RevokedAt != nil {
		t.Error("a's key was touched by a request naming b")
	}

	w := keyCall(h.PlatformRotate, http.MethodPost, platRotate, "/tenants/"+b+"/api-keys/"+kb.ID+"/rotate", "", "home", roles...)
	var rotated apiKeyCreateView
	_ = json.Unmarshal(w.Body.Bytes(), &rotated)
	if w.Code != http.StatusOK || rotated.Key == "" || rotated.Key == kb.Key {
		t.Fatalf("rotate = %d: %s", w.Code, w.Body.String())
	}
	if k, err := d.Store.APIKeys().GetByID(context.Background(), b, rotated.ID); err != nil || k.TenantID != b {
		t.Errorf("rotated key not in b: %v", err)
	}
	if w := keyCall(h.PlatformRevoke, http.MethodDelete, platKeyByID, "/tenants/"+b+"/api-keys/"+rotated.ID, "", "home", roles...); w.Code != http.StatusNoContent {
		t.Errorf("revoke = %d, want 204", w.Code)
	}
	if k, _ := d.Store.APIKeys().GetByID(context.Background(), b, rotated.ID); k == nil || k.RevokedAt == nil {
		t.Error("key not revoked")
	}
}

// Every successful platform action is logged with the acting principal
// and the target tenant.
func TestAPIKeys_PlatformCreate_AuditLog(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	d := keyDeps()
	h := APIKeys{Deps: d}
	target := seedTenant(t, d, "target")
	w := keyCall(h.PlatformCreate, http.MethodPost, platKeys, "/tenants/"+target+"/api-keys", `{"name":"k","role":"agent"}`, "home", pkgauth.RolesForKey(pkgauth.RolePlatformAdmin)...)
	var v apiKeyCreateView
	_ = json.Unmarshal(w.Body.Bytes(), &v)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("no audit line: %v (%q)", err, buf.String())
	}
	if rec["action"] != "create" || rec["actor"] != "test-principal" || rec["actor_tenant_id"] != "home" ||
		rec["target_tenant_id"] != target || rec["key_id"] != v.ID {
		t.Errorf("audit = %v", rec)
	}
	if strings.Contains(buf.String(), v.Key) {
		t.Error("audit line contains the plaintext key")
	}
}

func TestTenants_List_SlugFilter(t *testing.T) {
	d := newTestDeps()
	h := Tenants{Deps: d}
	seedTenant(t, d, "one")
	two := seedTenant(t, d, "two")

	list := func(q string) (int, []tenantView) {
		w := keyCall(h.List, http.MethodGet, "/tenants", "/tenants"+q, "", "t", "admin")
		var page struct {
			Items []tenantView `json:"items"`
			Total int          `json:"total"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &page)
		return w.Code, page.Items
	}
	if code, items := list("?slug=two"); code != 200 || len(items) != 1 || items[0].ID != two {
		t.Errorf("slug=two = %d %+v", code, items)
	}
	if code, items := list("?slug=missing"); code != 200 || len(items) != 0 {
		t.Errorf("slug=missing = %d %+v", code, items)
	}
	if _, items := list(""); len(items) != 2 {
		t.Errorf("no filter = %d items, want 2", len(items))
	}
}
