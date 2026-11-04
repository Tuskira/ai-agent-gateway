package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// restrictExemptDeps is newTestDeps with the built-in roles plus "editor",
// a role that may write connectors but does not hold admin.manage.
func restrictExemptDeps() Deps {
	d := newTestDeps()
	ra := pkgauth.NewRoleAuthorizer()
	ra.Rules["editor"] = []string{"connector.*"}
	d.Authorizer = ra
	return d
}

func connectorReq(method, path, body, role string) *http.Request {
	return withPrincipal(httptest.NewRequest(method, path, strings.NewReader(body)), "tenant-a", role)
}

// metadata.restrict_exempt must be a boolean; any other value is a 400.
func TestConnectors_RestrictExempt_MustBeBoolean(t *testing.T) {
	h := Connectors{Deps: restrictExemptDeps()}
	for _, v := range []string{`"true"`, `1`, `null`, `{}`} {
		body := `{"name":"help","endpoint":"https://help.example.com/mcp","metadata":{"restrict_exempt":` + v + `}}`
		w := serve(http.MethodPost, "/connectors", h.Create, connectorReq(http.MethodPost, "/connectors", body, "admin"))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "metadata.restrict_exempt must be a boolean") {
			t.Errorf("%s: status %d body %s; want 400", v, w.Code, w.Body.String())
		}
		assertErrorType(t, w, "validation_error")
	}
}

// Creating with restrict_exempt true needs admin.manage on top of
// connector.create; false or absent does not. The value comes back
// unmasked.
func TestConnectors_RestrictExempt_CreateNeedsAdmin(t *testing.T) {
	h := Connectors{Deps: restrictExemptDeps()}
	const exempt = `{"name":"help","endpoint":"https://help.example.com/mcp","metadata":{"restrict_exempt":true}}`

	w := serve(http.MethodPost, "/connectors", h.Create, connectorReq(http.MethodPost, "/connectors", exempt, "editor"))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), restrictExemptMessage) {
		t.Fatalf("editor create: status %d body %s; want 403", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "permission_error")

	for _, body := range []string{
		`{"name":"plain","endpoint":"https://plain.example.com/mcp"}`,
		`{"name":"off","endpoint":"https://off.example.com/mcp","metadata":{"restrict_exempt":false}}`,
	} {
		if w := serve(http.MethodPost, "/connectors", h.Create, connectorReq(http.MethodPost, "/connectors", body, "editor")); w.Code != http.StatusCreated {
			t.Errorf("editor create %s: status %d body %s", body, w.Code, w.Body.String())
		}
	}

	w = serve(http.MethodPost, "/connectors", h.Create, connectorReq(http.MethodPost, "/connectors", exempt, "admin"))
	if w.Code != http.StatusCreated {
		t.Fatalf("admin create: status %d body %s", w.Code, w.Body.String())
	}
	var got connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Metadata["restrict_exempt"] != true {
		t.Errorf("metadata = %v; want restrict_exempt true", got.Metadata)
	}
}

// Changing the value on PUT (setting it, or clearing it, including by
// leaving it out of a replaced metadata) needs admin.manage; an edit that
// keeps it as it is does not.
func TestConnectors_RestrictExempt_UpdateNeedsAdminToChange(t *testing.T) {
	h := Connectors{Deps: restrictExemptDeps()}
	w := serve(http.MethodPost, "/connectors", h.Create, connectorReq(http.MethodPost, "/connectors",
		`{"name":"help","endpoint":"https://help.example.com/mcp","metadata":{"restrict_exempt":true}}`, "admin"))
	var c connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	put := func(meta, role string) *httptest.ResponseRecorder {
		body := `{"name":"help","endpoint":"https://help.example.com/mcp"`
		if meta != "" {
			body += `,"metadata":` + meta
		}
		return serve(http.MethodPut, "/connectors/{id}", h.Update, connectorReq(http.MethodPut, "/connectors/"+c.ID, body+"}", role))
	}

	for name, meta := range map[string]string{
		"clear":        `{"restrict_exempt":false}`,
		"leave it out": `{"retry":{"max_attempts":2}}`,
	} {
		if w := put(meta, "editor"); w.Code != http.StatusForbidden {
			t.Errorf("editor %s: status %d body %s; want 403", name, w.Code, w.Body.String())
		}
	}
	// Unchanged: the same value sent back, or metadata omitted (kept).
	for _, meta := range []string{`{"restrict_exempt":true,"retry":{"max_attempts":2}}`, ""} {
		if w := put(meta, "editor"); w.Code != http.StatusOK {
			t.Errorf("editor keeping the value (%q): status %d body %s", meta, w.Code, w.Body.String())
		}
	}
	got, err := h.Store.Connectors().Get(t.Context(), "tenant-a", c.ID)
	if err != nil || !got.RestrictExempt() {
		t.Fatalf("after editor writes: %v, err %v; want still exempt", got, err)
	}
	if w := put(`{"restrict_exempt":false}`, "admin"); w.Code != http.StatusOK {
		t.Fatalf("admin clear: status %d body %s", w.Code, w.Body.String())
	}
	if w := put(`{"restrict_exempt":true}`, "editor"); w.Code != http.StatusForbidden {
		t.Errorf("editor set: status %d; want 403", w.Code)
	}
	if w := put(`{"restrict_exempt":"yes"}`, "admin"); w.Code != http.StatusBadRequest {
		t.Errorf("admin non-bool: status %d; want 400", w.Code)
	}
}

// A caller without admin.manage cannot repoint an exempt connector: a new
// endpoint, a new slug or a changed header (its stored token would go to
// the new place) is a 403. Edits that leave the route alone, and edits of a
// connector that is not exempt, still work.
func TestConnectors_RestrictExempt_RepointNeedsAdmin(t *testing.T) {
	h := Connectors{Deps: restrictExemptDeps()}
	const (
		endpoint = "https://help.example.com/mcp"
		headers  = `"headers":{"Authorization":{"type":"static","value":"Bearer s3cret"}}`
	)
	create := func(name, meta string) connectorView {
		body := `{"name":"` + name + `","endpoint":"` + endpoint + `","metadata":` + meta + `}`
		w := serve(http.MethodPost, "/connectors", h.Create, connectorReq(http.MethodPost, "/connectors", body, "admin"))
		var c connectorView
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil || w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, w.Code, w.Body.String())
		}
		return c
	}
	put := func(c connectorView, ep, slug, meta, role string) *httptest.ResponseRecorder {
		body := `{"name":"` + c.Name + `","endpoint":"` + ep + `"`
		if slug != "" {
			body += `,"slug":"` + slug + `"`
		}
		if meta != "" {
			body += `,"metadata":` + meta
		}
		return serve(http.MethodPut, "/connectors/{id}", h.Update, connectorReq(http.MethodPut, "/connectors/"+c.ID, body+"}", role))
	}

	exempt := create("help", `{"restrict_exempt":true,`+headers+`}`)
	const masked = `"headers":{"Authorization":{"type":"static","value":"***"}}`
	for name, tc := range map[string]struct{ ep, slug, meta string }{
		"new endpoint":                    {"https://evil.example.com/mcp", "", `{"restrict_exempt":true,` + masked + `}`},
		"new endpoint, metadata left out": {"https://evil.example.com/mcp", "", ""},
		"new slug":                        {endpoint, "other", `{"restrict_exempt":true,` + masked + `}`},
		"new header":                      {endpoint, "", `{"restrict_exempt":true,"headers":{"Authorization":{"type":"static","value":"Bearer other"}}}`},
	} {
		w := put(exempt, tc.ep, tc.slug, tc.meta, "editor")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), restrictExemptMessage) {
			t.Errorf("editor, %s: status %d body %s; want 403", name, w.Code, w.Body.String())
		}
	}
	got, err := h.Store.Connectors().Get(t.Context(), "tenant-a", exempt.ID)
	if err != nil || got.Endpoint != endpoint || got.Slug != exempt.Slug {
		t.Fatalf("after refused writes: %+v, err %v; want the connector untouched", got, err)
	}

	// The route is unchanged (headers sent back masked): allowed.
	if w := put(exempt, endpoint, "", `{"restrict_exempt":true,`+masked+`}`, "editor"); w.Code != http.StatusOK {
		t.Errorf("editor, same route: status %d body %s", w.Code, w.Body.String())
	}
	// An admin may repoint it.
	if w := put(exempt, "https://moved.example.com/mcp", "", `{"restrict_exempt":true,`+masked+`}`, "admin"); w.Code != http.StatusOK {
		t.Errorf("admin repoint: status %d body %s", w.Code, w.Body.String())
	}

	// A connector that is not exempt may be repointed by an editor.
	plain := create("plain", `{`+headers+`}`)
	if w := put(plain, "https://other.example.com/mcp", "", `{`+masked+`}`, "editor"); w.Code != http.StatusOK {
		t.Errorf("editor, plain connector: status %d body %s", w.Code, w.Body.String())
	}
}
