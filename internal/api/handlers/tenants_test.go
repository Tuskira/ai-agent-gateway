package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTenants_Create_HappyPath(t *testing.T) {
	h := Tenants{Deps: newTestDeps()}

	body := strings.NewReader(`{"slug":"acme","name":"Acme Corp"}`)
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/tenants", body), "caller-tenant", "admin")
	w := serve(http.MethodPost, "/tenants", h.Create, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got tenantView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Slug != "acme" || got.Name != "Acme Corp" || got.ID == "" {
		t.Errorf("got = %+v", got)
	}
}

func TestTenants_Create_ValidationError(t *testing.T) {
	h := Tenants{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/tenants", strings.NewReader(`{"slug":"acme","name":""}`)), "t", "admin")
	w := serve(http.MethodPost, "/tenants", h.Create, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	assertErrorType(t, w, "validation_error")
}

func TestTenants_Create_DuplicateSlugConflict(t *testing.T) {
	deps := newTestDeps()
	h := Tenants{Deps: deps}

	mk := func() *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/tenants", strings.NewReader(`{"slug":"acme","name":"Acme"}`)), "t", "admin")
		return serve(http.MethodPost, "/tenants", h.Create, req)
	}
	if w := mk(); w.Code != http.StatusCreated {
		t.Fatalf("first create status = %d", w.Code)
	}
	w := mk()
	if w.Code != http.StatusConflict {
		t.Fatalf("second create status = %d, want 409, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "conflict")
}

func TestTenants_List_Pagination(t *testing.T) {
	deps := newTestDeps()
	h := Tenants{Deps: deps}

	for _, slug := range []string{"a", "b", "c"} {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/tenants", strings.NewReader(`{"slug":"`+slug+`","name":"`+slug+`"}`)), "t", "admin")
		if w := serve(http.MethodPost, "/tenants", h.Create, req); w.Code != http.StatusCreated {
			t.Fatalf("seed create %q: status %d, body %s", slug, w.Code, w.Body.String())
		}
	}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/tenants?limit=2&offset=1", nil), "t", "admin")
	w := serve(http.MethodGet, "/tenants", h.List, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var page struct {
		Items []tenantView `json:"items"`
		Total int          `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 3 {
		t.Errorf("total = %d, want 3", page.Total)
	}
	if len(page.Items) != 2 {
		t.Errorf("items = %d, want 2 (limit=2)", len(page.Items))
	}
}

// assertErrorType checks the response body matches the shared envelope
// {"error":{"type":wantType,...}}.
func assertErrorType(t *testing.T, w *httptest.ResponseRecorder, wantType string) {
	t.Helper()
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, w.Body.String())
	}
	if body.Error.Type != wantType {
		t.Errorf("error.type = %q, want %q (message=%q)", body.Error.Type, wantType, body.Error.Message)
	}
	if body.Error.Message == "" {
		t.Error("error.message is empty")
	}
}

func TestTenants_Create_WithID(t *testing.T) {
	h := Tenants{Deps: newTestDeps()}
	create := func(body string) (int, tenantView) {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/tenants", strings.NewReader(body)), "caller-tenant", "admin")
		w := serve(http.MethodPost, "/tenants", h.Create, req)
		var v tenantView
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v
	}

	const id = "0b6f3f0e-5a5e-4d8e-9c35-0d4c3b1f7a21"
	if code, v := create(`{"id":"` + id + `","slug":"ext","name":"External"}`); code != http.StatusCreated || v.ID != id {
		t.Fatalf("with id = %d %+v, want 201 with id %s", code, v, id)
	}
	// Same id under another slug, and same slug under another id: 409.
	if code, _ := create(`{"id":"` + id + `","slug":"other","name":"x"}`); code != http.StatusConflict {
		t.Errorf("duplicate id = %d, want 409", code)
	}
	if code, _ := create(`{"id":"1b6f3f0e-5a5e-4d8e-9c35-0d4c3b1f7a21","slug":"ext","name":"x"}`); code != http.StatusConflict {
		t.Errorf("duplicate slug = %d, want 409", code)
	}
	// Absent id: generated, unchanged behavior.
	if code, v := create(`{"slug":"gen","name":"Generated"}`); code != http.StatusCreated || v.ID == "" || v.ID == id {
		t.Errorf("without id = %d %+v", code, v)
	}
	// Uppercase is accepted and stored lowercase.
	if code, v := create(`{"id":"0B6F3F0E-5A5E-4D8E-9C35-0D4C3B1F7A22","slug":"upper","name":"x"}`); code != http.StatusCreated || v.ID != "0b6f3f0e-5a5e-4d8e-9c35-0d4c3b1f7a22" {
		t.Errorf("uppercase id = %d %+v, want 201 with lowercase id", code, v)
	}
	// Anything that is not a plain 8-4-4-4-12 UUID is rejected.
	for _, bad := range []string{
		"not-a-uuid", "00000000-0000-0000-0000-000000000000",
		"{0b6f3f0e-5a5e-4d8e-9c35-0d4c3b1f7a23}",
		"urn:uuid:0b6f3f0e-5a5e-4d8e-9c35-0d4c3b1f7a24", "0b6f3f0e5a5e4d8e9c350d4c3b1f7a25",
	} {
		if code, _ := create(`{"id":"` + bad + `","slug":"bad-` + bad[:4] + `","name":"x"}`); code != http.StatusBadRequest {
			t.Errorf("id %q = %d, want 400", bad, code)
		}
	}
}
