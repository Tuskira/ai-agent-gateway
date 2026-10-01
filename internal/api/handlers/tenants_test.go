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
