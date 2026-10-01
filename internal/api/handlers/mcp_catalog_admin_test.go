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

// Roles used below: "tadmin" holds "*" (a tenant admin: no platform.*),
// "padmin" additionally holds platform.admin, "agent" only reads.
func adminDeps(t *testing.T) (Deps, map[string]*store.MCPCatalogEntry) {
	t.Helper()
	d, entries := catalogFixture(t)
	d.Authorizer = &pkgauth.RoleAuthorizer{Rules: map[string][]string{
		"tadmin": {"*"},
		"padmin": {"*", pkgauth.PermPlatformAdmin},
		"agent":  {"*.read"},
	}}
	return d, entries
}

const validEntryBody = `{"slug":"mine","name":"Mine","description":"My server","url":"https://mine.example.com/mcp",
	"auth":{"kind":"bearer","fields":[{"name":"token","label":"Token","secret":true,"required":true}]}}`

func catalogCall(d Deps, method, pattern, target, tenant, role, body string, fn http.HandlerFunc) *httptest.ResponseRecorder {
	req := withPrincipal(httptest.NewRequest(method, target, strings.NewReader(body)), tenant, role)
	return serve(method, pattern, fn, req)
}

func decodeEntry(t *testing.T, w *httptest.ResponseRecorder) catalogEntryView {
	t.Helper()
	var v catalogEntryView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return v
}

func TestMCPCatalog_Create_TenantEntryIsPrivateAndAddable(t *testing.T) {
	d, _ := adminDeps(t)
	h := MCPCatalog{Deps: d}

	w := catalogCall(d, http.MethodPost, "/mcp-catalog", "/mcp-catalog", "tenant-a", "tadmin", validEntryBody, h.Create)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	v := decodeEntry(t, w)
	if v.Scope != "tenant" || v.Slug != "mine" || !v.Enabled || !v.Supported || v.ID == "" {
		t.Errorf("created view = %+v", v)
	}

	// Duplicate slug in the same tenant.
	if w := catalogCall(d, http.MethodPost, "/mcp-catalog", "/mcp-catalog", "tenant-a", "tadmin", validEntryBody, h.Create); w.Code != http.StatusConflict {
		t.Errorf("duplicate create = %d, want 409", w.Code)
	}
	// Another tenant can use the same slug, and cannot see tenant-a's row.
	if w := catalogCall(d, http.MethodGet, "/mcp-catalog/{slug}", "/mcp-catalog/mine", "tenant-b", "tadmin", "", h.Get); w.Code != http.StatusNotFound {
		t.Errorf("tenant-b sees tenant-a's entry: %d", w.Code)
	}
	if w := catalogCall(d, http.MethodPost, "/mcp-catalog", "/mcp-catalog", "tenant-b", "tadmin", validEntryBody, h.Create); w.Code != http.StatusCreated {
		t.Errorf("tenant-b create same slug = %d, want 201", w.Code)
	}

	// The entry is addable by its owner and produces a bearer credential.
	add := doAdd(d, "tenant-a", "mine", `{"fields":{"token":"tok-123"}}`)
	got := decodeAdd(t, add)
	if got.Connector.CatalogID != v.ID {
		t.Errorf("connector catalog_id = %q, want %q", got.Connector.CatalogID, v.ID)
	}
	stored, err := d.Secrets.Get(context.Background(), "tenant-a", "mcp-mine")
	if err != nil || stored["authorization"] != "Bearer tok-123" {
		t.Errorf("credential = %v (%v)", stored, err)
	}
	// And not by another tenant that has no such entry... tenant-c:
	if w := doAdd(d, "tenant-c", "mine", `{"fields":{"token":"x"}}`); w.Code != http.StatusNotFound {
		t.Errorf("tenant-c add of tenant-a's entry = %d, want 404", w.Code)
	}
}

func TestMCPCatalog_Create_Validation(t *testing.T) {
	d, _ := adminDeps(t)
	h := MCPCatalog{Deps: d}
	cases := map[string]string{
		"bad slug":        `{"slug":"Bad Slug","name":"X","url":"https://x.example.com","auth":{"kind":"none"}}`,
		"missing name":    `{"slug":"x","name":"","url":"https://x.example.com","auth":{"kind":"none"}}`,
		"http url":        `{"slug":"x","name":"X","url":"http://x.example.com","auth":{"kind":"none"}}`,
		"unknown kind":    `{"slug":"x","name":"X","url":"https://x.example.com","auth":{"kind":"magic"}}`,
		"wrong field cnt": `{"slug":"x","name":"X","url":"https://x.example.com","auth":{"kind":"basic","fields":[{"name":"a","label":"A"}]}}`,
		"secret query":    `{"slug":"x","name":"X","url":"https://x.example.com","auth":{"kind":"none","fields":[{"name":"p","label":"P","secret":true,"query":"p"}]}}`,
		"bad scope":       `{"slug":"x","scope":"galaxy","name":"X","url":"https://x.example.com","auth":{"kind":"none"}}`,
		"unknown field":   `{"slug":"x","name":"X","url":"https://x.example.com","auth":{"kind":"none"},"bogus":1}`,
	}
	for why, body := range cases {
		if w := catalogCall(d, http.MethodPost, "/mcp-catalog", "/mcp-catalog", "tenant-a", "tadmin", body, h.Create); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", why, w.Code, w.Body.String())
		}
	}
	// Loopback http is allowed.
	ok := `{"slug":"local","name":"Local","url":"http://127.0.0.1:9000/mcp","auth":{"kind":"none"}}`
	if w := catalogCall(d, http.MethodPost, "/mcp-catalog", "/mcp-catalog", "tenant-a", "tadmin", ok, h.Create); w.Code != http.StatusCreated {
		t.Errorf("loopback http = %d %s, want 201", w.Code, w.Body.String())
	}
	if list, _ := d.Store.MCPCatalog().List(context.Background(), "tenant-a"); len(list) != 6+1 {
		t.Errorf("rejected creates left rows behind: %d entries", len(list))
	}
}

func TestMCPCatalog_PlatformWritesNeedPlatformAdmin(t *testing.T) {
	d, entries := adminDeps(t)
	h := MCPCatalog{Deps: d}
	platformBody := strings.Replace(validEntryBody, `"slug":"mine"`, `"slug":"shared","scope":"platform"`, 1)

	// A tenant admin cannot create a platform entry, nor edit or delete one.
	if w := catalogCall(d, http.MethodPost, "/mcp-catalog", "/mcp-catalog", "tenant-a", "tadmin", platformBody, h.Create); w.Code != http.StatusForbidden {
		t.Errorf("tenant admin creating a platform entry = %d, want 403", w.Code)
	}
	put := `{"name":"Hijack","url":"https://evil.example.com","auth":{"kind":"none"}}`
	if w := catalogCall(d, http.MethodPut, "/mcp-catalog/{slug}", "/mcp-catalog/open", "tenant-a", "tadmin", put, h.Update); w.Code != http.StatusForbidden {
		t.Errorf("tenant admin PUT platform entry = %d, want 403", w.Code)
	}
	if w := catalogCall(d, http.MethodDelete, "/mcp-catalog/{slug}", "/mcp-catalog/open", "tenant-a", "tadmin", "", h.Delete); w.Code != http.StatusForbidden {
		t.Errorf("tenant admin DELETE platform entry = %d, want 403", w.Code)
	}
	if g, _ := d.Store.MCPCatalog().GetBySlug(context.Background(), "", "open"); g.Name != "Open" {
		t.Errorf("platform entry changed: %+v", g)
	}

	// A platform admin can create one, which every tenant then sees.
	if w := catalogCall(d, http.MethodPost, "/mcp-catalog", "/mcp-catalog", "tenant-a", "padmin", platformBody, h.Create); w.Code != http.StatusCreated {
		t.Fatalf("platform create = %d %s", w.Code, w.Body.String())
	} else if v := decodeEntry(t, w); v.Scope != "platform" {
		t.Errorf("scope = %q", v.Scope)
	}
	if w := catalogCall(d, http.MethodGet, "/mcp-catalog/{slug}", "/mcp-catalog/shared", "tenant-z", "agent", "", h.Get); w.Code != http.StatusOK {
		t.Errorf("another tenant reads the platform entry = %d", w.Code)
	}

	// ...edit it (default resolution reaches the platform row when the tenant has none)...
	w := catalogCall(d, http.MethodPut, "/mcp-catalog/{slug}", "/mcp-catalog/open", "tenant-a", "padmin", put, h.Update)
	if w.Code != http.StatusOK {
		t.Fatalf("platform PUT = %d %s", w.Code, w.Body.String())
	}
	if v := decodeEntry(t, w); v.Scope != "platform" || v.Name != "Hijack" || v.ID != entries["open"].ID {
		t.Errorf("updated = %+v", v)
	}
	// ...and delete it.
	if w := catalogCall(d, http.MethodDelete, "/mcp-catalog/{slug}", "/mcp-catalog/shared", "tenant-a", "padmin", "", h.Delete); w.Code != http.StatusNoContent {
		t.Errorf("platform DELETE = %d", w.Code)
	}
	if _, err := d.Store.MCPCatalog().GetBySlug(context.Background(), "tenant-b", "shared"); err == nil {
		t.Error("deleted platform entry still visible")
	}
}

func TestMCPCatalog_TenantEntryShadowsPlatform_ListScopeAndDelete(t *testing.T) {
	d, entries := adminDeps(t)
	h := MCPCatalog{Deps: d}

	shadow := `{"slug":"open","name":"Open (ours)","url":"https://ours.example.com/mcp","auth":{"kind":"none"}}`
	if w := catalogCall(d, http.MethodPost, "/mcp-catalog", "/mcp-catalog", "tenant-a", "tadmin", shadow, h.Create); w.Code != http.StatusCreated {
		t.Fatalf("shadow create = %d %s", w.Code, w.Body.String())
	}

	list := func(tenant, role string) map[string]catalogEntryView {
		t.Helper()
		w := catalogCall(d, http.MethodGet, "/mcp-catalog", "/mcp-catalog", tenant, role, "", h.List)
		var page struct {
			Items []catalogEntryView `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		out := map[string]catalogEntryView{}
		for _, v := range page.Items {
			if _, dup := out[v.Slug]; dup {
				t.Errorf("slug %q listed twice", v.Slug)
			}
			out[v.Slug] = v
		}
		return out
	}

	a := list("tenant-a", "agent")
	if a["open"].Scope != "tenant" || a["open"].Name != "Open (ours)" {
		t.Errorf("tenant-a sees %+v for open, want its own row", a["open"])
	}
	if a["tok"].Scope != "platform" {
		t.Errorf("tok scope = %q, want platform", a["tok"].Scope)
	}
	if b := list("tenant-b", "agent"); b["open"].Scope != "platform" || b["open"].ID != entries["open"].ID {
		t.Errorf("tenant-b sees %+v for open, want the platform row", b["open"])
	}

	// Adding "open" uses the tenant's row.
	got := decodeAdd(t, doAdd(d, "tenant-a", "open", `{}`))
	if got.Connector.Endpoint != "https://ours.example.com/mcp" {
		t.Errorf("endpoint = %q, want the tenant entry's URL", got.Connector.Endpoint)
	}

	// Deleting the shadow (default resolution) removes only the tenant row.
	if w := catalogCall(d, http.MethodDelete, "/mcp-catalog/{slug}", "/mcp-catalog/open", "tenant-a", "tadmin", "", h.Delete); w.Code != http.StatusNoContent {
		t.Fatalf("delete shadow = %d", w.Code)
	}
	if a := list("tenant-a", "agent"); a["open"].Scope != "platform" {
		t.Errorf("after deleting the shadow tenant-a sees %+v, want the platform row", a["open"])
	}
	// A tenant-scoped delete of a slug that only exists on the platform is 404, not 403.
	if w := catalogCall(d, http.MethodDelete, "/mcp-catalog/{slug}", "/mcp-catalog/tok?scope=tenant", "tenant-a", "tadmin", "", h.Delete); w.Code != http.StatusNotFound {
		t.Errorf("scope=tenant delete of a platform-only slug = %d, want 404", w.Code)
	}
}

func TestMCPCatalog_Update_FullReplaceKeepsSlugAndOwner(t *testing.T) {
	d, _ := adminDeps(t)
	h := MCPCatalog{Deps: d}
	catalogCall(d, http.MethodPost, "/mcp-catalog", "/mcp-catalog", "tenant-a", "tadmin", validEntryBody, h.Create)

	put := `{"name":"Mine v2","description":"new","url":"https://mine.example.com/v2","url_overridable":true,
		"auth":{"kind":"basic","fields":[{"name":"u","label":"U","required":true},{"name":"p","label":"P","secret":true,"required":true}]},
		"enabled":false}`
	w := catalogCall(d, http.MethodPut, "/mcp-catalog/{slug}", "/mcp-catalog/mine", "tenant-a", "tadmin", put, h.Update)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
	}
	v := decodeEntry(t, w)
	if v.Slug != "mine" || v.Scope != "tenant" || v.Name != "Mine v2" || v.Auth.Kind != "basic" || v.Enabled || !v.URLOverridable {
		t.Errorf("updated = %+v", v)
	}
	// A disabled entry disappears for readers but stays visible to admins, and cannot be added.
	if w := catalogCall(d, http.MethodGet, "/mcp-catalog/{slug}", "/mcp-catalog/mine", "tenant-a", "agent", "", h.Get); w.Code != http.StatusNotFound {
		t.Errorf("agent reads a disabled entry = %d, want 404", w.Code)
	}
	if w := catalogCall(d, http.MethodGet, "/mcp-catalog/{slug}", "/mcp-catalog/mine", "tenant-a", "tadmin", "", h.Get); w.Code != http.StatusOK {
		t.Errorf("admin reads a disabled entry = %d, want 200", w.Code)
	}
	if w := doAdd(d, "tenant-a", "mine", `{"fields":{"u":"a","p":"b"}}`); w.Code != http.StatusNotFound {
		t.Errorf("add of a disabled entry = %d, want 404", w.Code)
	}

	// Slug and scope cannot be changed through PUT; an invalid body changes nothing.
	for why, body := range map[string]string{
		"slug change": `{"slug":"other","name":"X","url":"https://x.example.com","auth":{"kind":"none"}}`,
		"scope":       `{"scope":"platform","name":"X","url":"https://x.example.com","auth":{"kind":"none"}}`,
		"invalid":     `{"name":"X","url":"ftp://x","auth":{"kind":"none"}}`,
	} {
		if w := catalogCall(d, http.MethodPut, "/mcp-catalog/{slug}", "/mcp-catalog/mine", "tenant-a", "tadmin", body, h.Update); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", why, w.Code)
		}
	}
	if w := catalogCall(d, http.MethodPut, "/mcp-catalog/{slug}", "/mcp-catalog/missing", "tenant-a", "tadmin", put, h.Update); w.Code != http.StatusNotFound {
		t.Errorf("PUT missing = %d, want 404", w.Code)
	}
	// Another tenant cannot edit tenant-a's entry.
	if w := catalogCall(d, http.MethodPut, "/mcp-catalog/{slug}", "/mcp-catalog/mine", "tenant-b", "tadmin", put, h.Update); w.Code != http.StatusNotFound {
		t.Errorf("tenant-b PUT tenant-a's entry = %d, want 404", w.Code)
	}
}

func TestMCPCatalog_OptionalCredentialSendsNoHeaderWhenBlank(t *testing.T) {
	d, _ := adminDeps(t)
	h := MCPCatalog{Deps: d}
	body := `{"slug":"opt","name":"Opt","url":"https://opt.example.com/mcp",
		"auth":{"kind":"bearer","fields":[{"name":"key","label":"API key","secret":true}]}}`
	if w := catalogCall(d, http.MethodPost, "/mcp-catalog", "/mcp-catalog", "tenant-a", "tadmin", body, h.Create); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	blank := decodeAdd(t, doAdd(d, "tenant-a", "opt", `{}`))
	if hdrs, _ := blank.Connector.Metadata["headers"].(map[string]any); len(hdrs) != 0 {
		t.Errorf("blank optional key still produced headers: %v", hdrs)
	}
	if creds, _ := d.Secrets.List(context.Background(), "tenant-a"); len(creds) != 0 {
		t.Errorf("blank optional key created %d credentials", len(creds))
	}
	withKey := decodeAdd(t, doAdd(d, "tenant-a", "opt", `{"slug":"opt-2","fields":{"key":"k1"}}`))
	if _, ok := withKey.Connector.Metadata["headers"].(map[string]any)["Authorization"]; !ok {
		t.Errorf("provided key produced no Authorization header: %v", withKey.Connector.Metadata)
	}
}
