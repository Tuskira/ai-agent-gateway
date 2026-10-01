package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func TestCredentials_Create_HappyPath_NeverLeaksPayload(t *testing.T) {
	h := Credentials{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/credentials",
		strings.NewReader(`{"name":"okta-prod","type":"secret_store","payload":{"client_id":"abc","client_secret":"s3cr3t"}}`)),
		"tenant-a", "admin")
	w := serve(http.MethodPost, "/credentials", h.Create, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "s3cr3t") {
		t.Fatalf("response leaked the payload: %s", w.Body.String())
	}

	var got credentialView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "okta-prod" || got.Type != "secret_store" || got.KeyID != "k1" {
		t.Errorf("got = %+v", got)
	}
	if len(got.FieldNames) != 2 || got.FieldNames[0] != "client_id" || got.FieldNames[1] != "client_secret" {
		t.Errorf("FieldNames = %v, want sorted [client_id client_secret]", got.FieldNames)
	}
}

func TestCredentials_Create_ValidationError_EmptyPayload(t *testing.T) {
	h := Credentials{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/credentials",
		strings.NewReader(`{"name":"x","type":"t","payload":{}}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/credentials", h.Create, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "validation_error")
}

func TestCredentials_Get_TenantIsolation(t *testing.T) {
	deps := newTestDeps()
	h := Credentials{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/credentials",
		strings.NewReader(`{"name":"okta","type":"t","payload":{"a":"1"}}`)), "tenant-a", "admin")
	if w := serve(http.MethodPost, "/credentials", h.Create, createReq); w.Code != http.StatusCreated {
		t.Fatalf("seed create status = %d", w.Code)
	}

	getReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/credentials/okta", nil), "tenant-b", "admin")
	w := serve(http.MethodGet, "/credentials/{name}", h.Get, getReq)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get status = %d, want 404", w.Code)
	}
	assertErrorType(t, w, "not_found")
}

func TestCredentials_Rotate_ChangesFieldNames(t *testing.T) {
	deps := newTestDeps()
	h := Credentials{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/credentials",
		strings.NewReader(`{"name":"okta","type":"t","payload":{"a":"1"}}`)), "tenant-a", "admin")
	serve(http.MethodPost, "/credentials", h.Create, createReq)

	rotateReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/credentials/okta",
		strings.NewReader(`{"payload":{"b":"2","c":"3"}}`)), "tenant-a", "admin")
	w := serve(http.MethodPut, "/credentials/{name}", h.Rotate, rotateReq)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, body = %s", w.Code, w.Body.String())
	}

	var got credentialView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.FieldNames) != 2 || got.FieldNames[0] != "b" || got.FieldNames[1] != "c" {
		t.Errorf("FieldNames after rotate = %v, want [b c]", got.FieldNames)
	}
	if got.RotatedAt == "" {
		t.Error("RotatedAt not set after rotate")
	}
}

func TestCredentials_List_And_Get_UsedBy(t *testing.T) {
	deps := newTestDeps()
	h := Credentials{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/credentials",
		strings.NewReader(`{"name":"okta-prod","type":"secret_store","payload":{"client_id":"abc","client_secret":"s3cr3t"}}`)), "tenant-a", "admin")
	if w := serve(http.MethodPost, "/credentials", h.Create, createReq); w.Code != http.StatusCreated {
		t.Fatalf("seed create status = %d", w.Code)
	}

	conn := &store.Connector{
		TenantID: "tenant-a",
		Name:     "Okta MCP",
		Slug:     "okta-mcp",
		Endpoint: "https://okta.example.com/mcp",
		Metadata: map[string]any{
			"headers": map[string]any{
				"X-Client-Secret": map[string]any{
					"type": "external", "provider": "secret_store",
					"config": map[string]any{"credential": "okta-prod", "field": "client_secret"},
				},
			},
		},
	}
	if err := deps.Store.Connectors().Create(context.Background(), conn); err != nil {
		t.Fatalf("create connector: %v", err)
	}
	// An unrelated connector, and a header of a different type/provider,
	// must not show up as a user of okta-prod.
	other := &store.Connector{
		TenantID: "tenant-a",
		Name:     "Other",
		Slug:     "other",
		Endpoint: "https://other.example.com/mcp",
		Metadata: map[string]any{
			"headers": map[string]any{
				"X-Static": map[string]any{"type": "static", "value": "v"},
			},
		},
	}
	if err := deps.Store.Connectors().Create(context.Background(), other); err != nil {
		t.Fatalf("create other connector: %v", err)
	}

	listReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/credentials", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/credentials", h.List, listReq)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", w.Code, w.Body.String())
	}
	var page struct {
		Items []credentialView `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %+v", page.Items)
	}
	if len(page.Items[0].UsedBy) != 1 || page.Items[0].UsedBy[0].ID != conn.ID || page.Items[0].UsedBy[0].Name != "Okta MCP" {
		t.Errorf("List UsedBy = %+v, want exactly connector %q", page.Items[0].UsedBy, conn.ID)
	}

	getReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/credentials/okta-prod", nil), "tenant-a", "admin")
	w = serve(http.MethodGet, "/credentials/{name}", h.Get, getReq)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", w.Code, w.Body.String())
	}
	var got credentialView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.UsedBy) != 1 || got.UsedBy[0].ID != conn.ID {
		t.Errorf("Get UsedBy = %+v, want exactly connector %q", got.UsedBy, conn.ID)
	}
}

func TestCredentials_UsedByEmptyWhenUnreferenced(t *testing.T) {
	deps := newTestDeps()
	h := Credentials{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/credentials",
		strings.NewReader(`{"name":"unused","type":"t","payload":{"a":"1"}}`)), "tenant-a", "admin")
	serve(http.MethodPost, "/credentials", h.Create, createReq)

	getReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/credentials/unused", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/credentials/{name}", h.Get, getReq)
	var got credentialView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UsedBy == nil || len(got.UsedBy) != 0 {
		t.Errorf("UsedBy = %+v, want an empty (non-null) slice", got.UsedBy)
	}
}

func TestCredentials_Delete(t *testing.T) {
	deps := newTestDeps()
	h := Credentials{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/credentials",
		strings.NewReader(`{"name":"okta","type":"t","payload":{"a":"1"}}`)), "tenant-a", "admin")
	serve(http.MethodPost, "/credentials", h.Create, createReq)

	delReq := withPrincipal(httptest.NewRequest(http.MethodDelete, "/credentials/okta", nil), "tenant-a", "admin")
	w := serve(http.MethodDelete, "/credentials/{name}", h.Delete, delReq)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", w.Code)
	}

	getReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/credentials/okta", nil), "tenant-a", "admin")
	w = serve(http.MethodGet, "/credentials/{name}", h.Get, getReq)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", w.Code)
	}
}
