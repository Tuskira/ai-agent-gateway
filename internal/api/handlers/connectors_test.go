package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/ops"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const connectorCreateBody = `{
	"name":"Okta MCP!",
	"endpoint":"https://okta.example.com/mcp",
	"metadata":{
		"headers":{
			"X-Static":{"type":"static","value":"top-secret"},
			"X-Client-Secret":{"type":"external","provider":"secret_store","config":{"credential":"okta-prod","field":"client_secret"}}
		},
		"retry":{"max_attempts":3}
	}
}`

// connectors.default_timeout_ms applies to a connector created without
// timeout_ms; an explicit timeout_ms still wins.
func TestConnectors_Create_UsesConfiguredDefaultTimeout(t *testing.T) {
	d := newTestDeps()
	d.DefaultTimeoutMS = 45000
	h := Connectors{Deps: d}
	for body, want := range map[string]int{
		`{"name":"a","endpoint":"https://a.example.com/mcp"}`:                   45000,
		`{"name":"b","endpoint":"https://b.example.com/mcp","timeout_ms":1234}`: 1234,
	} {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(body)), "tenant-a", "admin")
		w := serve(http.MethodPost, "/connectors", h.Create, req)
		var got connectorView
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		if got.TimeoutMS != want {
			t.Errorf("%s: TimeoutMS = %d, want %d", body, got.TimeoutMS, want)
		}
	}
}

func TestConnectors_Create_SlugifiesNameAndMasksSecrets(t *testing.T) {
	h := Connectors{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "top-secret") {
		t.Fatalf("response leaked the static header value: %s", w.Body.String())
	}

	var got connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Slug != "okta-mcp" {
		t.Errorf("Slug = %q, want %q", got.Slug, "okta-mcp")
	}
	if got.TimeoutMS != defaultConnectorTimeoutMS {
		t.Errorf("TimeoutMS = %d, want default %d", got.TimeoutMS, defaultConnectorTimeoutMS)
	}

	headers, _ := got.Metadata["headers"].(map[string]any)
	static, _ := headers["X-Static"].(map[string]any)
	if static["value"] != "***" {
		t.Errorf("static header value not masked: %+v", static)
	}
	ext, _ := headers["X-Client-Secret"].(map[string]any)
	cfg, _ := ext["config"].(map[string]any)
	if cfg["credential"] != "***" || cfg["field"] != "***" {
		t.Errorf("external header config not masked: %+v", cfg)
	}
	retry, _ := got.Metadata["retry"].(map[string]any)
	if retry["max_attempts"] != float64(3) {
		t.Errorf("non-header metadata was altered: %+v", got.Metadata["retry"])
	}
}

func TestConnectors_Create_DuplicateSlugConflict(t *testing.T) {
	deps := newTestDeps()
	h := Connectors{Deps: deps}

	mk := func() *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), "tenant-a", "admin")
		return serve(http.MethodPost, "/connectors", h.Create, req)
	}
	if w := mk(); w.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, body = %s", w.Code, w.Body.String())
	}
	w := mk()
	if w.Code != http.StatusConflict {
		t.Fatalf("second create status = %d, want 409", w.Code)
	}
}

func TestConnectors_Create_ValidationErrors(t *testing.T) {
	h := Connectors{Deps: newTestDeps()}

	cases := map[string]string{
		"empty name":        `{"name":"","endpoint":"https://x.example.com"}`,
		"relative endpoint": `{"name":"x","endpoint":"/not-absolute"}`,
		"bad scheme":        `{"name":"x","endpoint":"ftp://x.example.com"}`,
		"timeout too low":   `{"name":"x","endpoint":"https://x.example.com","timeout_ms":1}`,
		"timeout too high":  `{"name":"x","endpoint":"https://x.example.com","timeout_ms":9999999}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(body)), "tenant-a", "admin")
			w := serve(http.MethodPost, "/connectors", h.Create, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
			}
			assertErrorType(t, w, "validation_error")
		})
	}
}

func TestConnectors_Get_And_List_IncludeCapabilities(t *testing.T) {
	deps := newTestDeps()
	h := Connectors{Deps: deps}

	conn := &store.Connector{
		TenantID: "tenant-a",
		Name:     "probed",
		Slug:     "probed",
		Endpoint: "https://probed.example.com/mcp",
		Status:   "healthy",
		Capabilities: map[string]any{
			"tools":            true,
			"protocol_version": "2025-06-18",
			"server_info":      map[string]any{"name": "backend", "version": "1.0"},
		},
	}
	if err := deps.Store.Connectors().Create(context.Background(), conn); err != nil {
		t.Fatalf("create connector: %v", err)
	}

	getReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/connectors/"+conn.ID, nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/connectors/{id}", h.Get, getReq)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", w.Code, w.Body.String())
	}
	var got connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Capabilities["tools"] != true || got.Capabilities["protocol_version"] != "2025-06-18" {
		t.Errorf("Get Capabilities = %+v", got.Capabilities)
	}
	si, _ := got.Capabilities["server_info"].(map[string]any)
	if si["name"] != "backend" || si["version"] != "1.0" {
		t.Errorf("Get Capabilities.server_info = %+v", si)
	}

	listReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/connectors", nil), "tenant-a", "admin")
	w = serve(http.MethodGet, "/connectors", h.List, listReq)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", w.Code, w.Body.String())
	}
	var page struct {
		Items []connectorView `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Capabilities["tools"] != true {
		t.Errorf("List item Capabilities = %+v", page.Items)
	}
}

func TestConnectors_Create_DescriptionAndSlugOverride(t *testing.T) {
	h := Connectors{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(
		`{"name":"Okta MCP","endpoint":"https://okta.example.com/mcp","description":"prod okta","slug":"custom-slug"}`)),
		"tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Slug != "custom-slug" {
		t.Errorf("Slug = %q, want %q", got.Slug, "custom-slug")
	}
	if got.Description != "prod okta" {
		t.Errorf("Description = %q, want %q", got.Description, "prod okta")
	}
	if got.Metadata["description"] != "prod okta" {
		t.Errorf("Metadata.description = %v, want %q", got.Metadata["description"], "prod okta")
	}
}

func TestConnectors_Create_InvalidSlugRejected(t *testing.T) {
	h := Connectors{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(
		`{"name":"x","endpoint":"https://x.example.com","slug":"Not Valid!"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "validation_error")
}

// TestConnectors_Create_GatewayNameAndSlugAreReserved asserts that
// neither a connector named "gateway" (any casing) nor one explicitly
// slugged "gateway" can be created: Phase 2's native gateway__skill tool
// and native command prompts (internal/dataplane/orchestrator) would
// become ambiguous with a "gateway__*"-qualified connector tool.
func TestConnectors_Create_GatewayNameAndSlugAreReserved(t *testing.T) {
	h := Connectors{Deps: newTestDeps()}

	for _, body := range []string{
		`{"name":"gateway","endpoint":"https://x.example.com"}`,
		`{"name":"Gateway","endpoint":"https://x.example.com"}`,
		`{"name":"My Connector","endpoint":"https://x.example.com","slug":"gateway"}`,
	} {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(body)), "tenant-a", "admin")
		w := serve(http.MethodPost, "/connectors", h.Create, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400, body = %s", body, w.Code, w.Body.String())
		}
	}

	// A name that merely contains "gateway" as a substring is fine --
	// only an exact (case-insensitive) match is reserved.
	okReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(
		`{"name":"My Gateway Connector","endpoint":"https://x.example.com"}`)), "tenant-a", "admin")
	if w := serve(http.MethodPost, "/connectors", h.Create, okReq); w.Code != http.StatusCreated {
		t.Errorf("a name merely containing \"gateway\" status = %d, want 201, body = %s", w.Code, w.Body.String())
	}
}

func TestConnectors_Update_GatewayNameIsReserved(t *testing.T) {
	deps := newTestDeps()
	h := Connectors{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, createReq)
	var created connectorView
	json.Unmarshal(w.Body.Bytes(), &created)

	updReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/connectors/"+created.ID, strings.NewReader(
		`{"name":"gateway","endpoint":"https://x.example.com"}`)), "tenant-a", "admin")
	w = serve(http.MethodPut, "/connectors/{id}", h.Update, updReq)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

func TestConnectors_Create_DuplicateSlugOverrideConflict(t *testing.T) {
	deps := newTestDeps()
	h := Connectors{Deps: deps}

	mk := func(name string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(
			`{"name":"`+name+`","endpoint":"https://x.example.com","slug":"shared-slug"}`)), "tenant-a", "admin")
		return serve(http.MethodPost, "/connectors", h.Create, req)
	}
	if w := mk("first"); w.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, body = %s", w.Code, w.Body.String())
	}
	if w := mk("second"); w.Code != http.StatusConflict {
		t.Fatalf("second create status = %d, want 409", w.Code)
	}
}

func TestConnectors_Update_DescriptionAndSlugOverride(t *testing.T) {
	deps := newTestDeps()
	h := Connectors{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, createReq)
	var created connectorView
	json.Unmarshal(w.Body.Bytes(), &created)

	updateReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/connectors/"+created.ID,
		strings.NewReader(`{"name":"Okta MCP","endpoint":"https://okta.example.com/mcp","description":"updated desc","slug":"renamed-slug"}`)),
		"tenant-a", "admin")
	w = serve(http.MethodPut, "/connectors/{id}", h.Update, updateReq)
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", w.Code, w.Body.String())
	}
	var updated connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.Slug != "renamed-slug" {
		t.Errorf("Slug = %q, want %q", updated.Slug, "renamed-slug")
	}
	if updated.Description != "updated desc" {
		t.Errorf("Description = %q, want %q", updated.Description, "updated desc")
	}
	// The original metadata (headers, retry) must survive the description
	// patch: description-only intent doesn't imply metadata was sent.
	if _, ok := updated.Metadata["headers"]; !ok {
		t.Error("metadata was wiped by a description-only-style update")
	}
}

func TestConnectors_Update_SlugConflict(t *testing.T) {
	deps := newTestDeps()
	h := Connectors{Deps: deps}

	idA := createTestConnector(t, deps, "tenant-a")
	takenReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(
		`{"name":"taken","endpoint":"https://taken.example.com","slug":"taken-slug"}`)), "tenant-a", "admin")
	if w := serve(http.MethodPost, "/connectors", h.Create, takenReq); w.Code != http.StatusCreated {
		t.Fatalf("seed create status = %d", w.Code)
	}

	updateReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/connectors/"+idA,
		strings.NewReader(`{"name":"Okta MCP!","endpoint":"https://okta.example.com/mcp","slug":"taken-slug"}`)), "tenant-a", "admin")
	w := serve(http.MethodPut, "/connectors/{id}", h.Update, updateReq)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", w.Code, w.Body.String())
	}
}

func TestConnectors_Get_TenantIsolation(t *testing.T) {
	deps := newTestDeps()
	h := Connectors{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, createReq)
	var created connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	getReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/connectors/"+created.ID, nil), "tenant-b", "admin")
	w = serve(http.MethodGet, "/connectors/{id}", h.Get, getReq)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get status = %d, want 404", w.Code)
	}
}

func TestConnectors_Update_PreservesSlugAndOmittedMetadata(t *testing.T) {
	deps := newTestDeps()
	h := Connectors{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, createReq)
	var created connectorView
	json.Unmarshal(w.Body.Bytes(), &created)

	updateReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/connectors/"+created.ID,
		strings.NewReader(`{"name":"Okta MCP Renamed","endpoint":"https://okta2.example.com/mcp"}`)), "tenant-a", "admin")
	w = serve(http.MethodPut, "/connectors/{id}", h.Update, updateReq)
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", w.Code, w.Body.String())
	}
	var updated connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.Slug != created.Slug {
		t.Errorf("Slug changed on update: %q -> %q, want immutable", created.Slug, updated.Slug)
	}
	if updated.Name != "Okta MCP Renamed" || updated.Endpoint != "https://okta2.example.com/mcp" {
		t.Errorf("update did not apply: %+v", updated)
	}
	if _, ok := updated.Metadata["headers"]; !ok {
		t.Error("metadata was wiped even though the update request omitted it")
	}
}

// An invalid header config is a 400 at save; sending a read's masked values
// back on PUT still saves (they are validated after unmasking).
func TestConnectors_HeadersValidatedAtSave(t *testing.T) {
	h := Connectors{Deps: newTestDeps()}
	bad := `{"name":"deny","endpoint":"https://d.example.com/mcp","metadata":{"headers":{"X-Fwd":{"type":"incoming_field","header":"Authorization"}}}}`
	w := serve(http.MethodPost, "/connectors", h.Create, withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(bad)), "tenant-a", "admin"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "validation_error") {
		t.Fatalf("create with denylisted header = %d %s, want 400 validation_error", w.Code, w.Body.String())
	}

	w = serve(http.MethodPost, "/connectors", h.Create, withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), "tenant-a", "admin"))
	var created connectorView
	json.Unmarshal(w.Body.Bytes(), &created)
	masked, _ := json.Marshal(map[string]any{"name": created.Name, "endpoint": created.Endpoint, "metadata": created.Metadata})
	w = serve(http.MethodPut, "/connectors/{id}", h.Update, withPrincipal(httptest.NewRequest(http.MethodPut, "/connectors/"+created.ID, strings.NewReader(string(masked))), "tenant-a", "admin"))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT with masked values = %d %s, want 200", w.Code, w.Body.String())
	}
}

// A header saved before save-time validation existed must not block editing
// or disabling the connector: the console sends every header back. Only a
// header the request adds or changes is checked.
func TestConnectors_Update_UnchangedLegacyHeaderStillSaves(t *testing.T) {
	deps := newTestDeps()
	h := Connectors{Deps: deps}
	legacy := &store.Connector{TenantID: "tenant-a", Name: "legacy", Slug: "legacy", Endpoint: "https://l.example.com/mcp", TimeoutMS: 30000,
		Metadata: map[string]any{"headers": map[string]any{"X-Fwd": map[string]any{"type": "incoming_field", "header": "Authorization"}}}}
	if err := deps.Store.Connectors().Create(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	put := func(body string) *httptest.ResponseRecorder {
		return serve(http.MethodPut, "/connectors/{id}", h.Update, withPrincipal(httptest.NewRequest(http.MethodPut, "/connectors/"+legacy.ID, strings.NewReader(body)), "tenant-a", "admin"))
	}
	disable := `{"name":"legacy","endpoint":"https://l.example.com/mcp","metadata":{"enabled":false,"headers":{"X-Fwd":{"type":"incoming_field","header":"Authorization"}}}}`
	if w := put(disable); w.Code != http.StatusOK {
		t.Fatalf("disable with the unchanged legacy header = %d %s, want 200", w.Code, w.Body.String())
	}
	added := `{"name":"legacy","endpoint":"https://l.example.com/mcp","metadata":{"headers":{"X-Fwd":{"type":"incoming_field","header":"Authorization"},"X-New":{"type":"incoming_field","header":"Cookie"}}}}`
	if w := put(added); w.Code != http.StatusOK {
		t.Fatalf("adding a valid header next to the legacy one = %d %s, want 200", w.Code, w.Body.String())
	}
	bad := `{"name":"legacy","endpoint":"https://l.example.com/mcp","metadata":{"headers":{"X-Fwd":{"type":"incoming_field","header":"X-Gateway-Key"}}}}`
	if w := put(bad); w.Code != http.StatusBadRequest {
		t.Fatalf("changing the header to another denylisted one = %d, want 400", w.Code)
	}
}

func TestConnectors_Delete_SoftDeleteHidesFromGetAndList(t *testing.T) {
	deps := newTestDeps()
	h := Connectors{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, createReq)
	var created connectorView
	json.Unmarshal(w.Body.Bytes(), &created)

	delReq := withPrincipal(httptest.NewRequest(http.MethodDelete, "/connectors/"+created.ID, nil), "tenant-a", "admin")
	w = serve(http.MethodDelete, "/connectors/{id}", h.Delete, delReq)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", w.Code)
	}

	getReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/connectors/"+created.ID, nil), "tenant-a", "admin")
	w = serve(http.MethodGet, "/connectors/{id}", h.Get, getReq)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", w.Code)
	}
}

// TestConnectors_Delete_InvalidatesCache confirms Delete invalidates the
// connector's tool cache (Deps.CacheOps) with the caller's tenant and the
// deleted connector's id, so a deleted connector's tools don't linger in
// tools/list until the cache's TTL expires.
func TestConnectors_Delete_InvalidatesCache(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeCacheOps{store: deps.Store.Connectors()}
	deps.CacheOps = fake
	h := Connectors{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, createReq)
	var created connectorView
	json.Unmarshal(w.Body.Bytes(), &created)

	delReq := withPrincipal(httptest.NewRequest(http.MethodDelete, "/connectors/"+created.ID, nil), "tenant-a", "admin")
	w = serve(http.MethodDelete, "/connectors/{id}", h.Delete, delReq)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", w.Code)
	}
	if fake.lastInvalidateConnectorID != created.ID {
		t.Errorf("CacheOps.Invalidate connector id = %q, want %q", fake.lastInvalidateConnectorID, created.ID)
	}
}

// TestConnectors_Delete_SucceedsEvenIfCacheInvalidateFails confirms a
// CacheOps.Invalidate failure is logged, not returned: the soft delete
// itself already succeeded and is what the caller is waiting on.
func TestConnectors_Delete_SucceedsEvenIfCacheInvalidateFails(t *testing.T) {
	deps := newTestDeps()
	fake := &fakeCacheOps{store: deps.Store.Connectors(), invalidateErr: errors.New("cache backend unavailable")}
	deps.CacheOps = fake
	h := Connectors{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, createReq)
	var created connectorView
	json.Unmarshal(w.Body.Bytes(), &created)

	delReq := withPrincipal(httptest.NewRequest(http.MethodDelete, "/connectors/"+created.ID, nil), "tenant-a", "admin")
	w = serve(http.MethodDelete, "/connectors/{id}", h.Delete, delReq)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 even though cache invalidation failed", w.Code)
	}
}

func TestConnectors_Tools_ListsCachedTools(t *testing.T) {
	deps := newTestDeps()
	h := Connectors{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, createReq)
	var created connectorView
	json.Unmarshal(w.Body.Bytes(), &created)

	fs := deps.Store.(*fakeStore)
	if err := fs.toolCache.Upsert(context.Background(), &store.CachedTool{
		TenantID: "tenant-a", ConnectorID: created.ID, ToolName: "list_users", Description: "List users",
	}); err != nil {
		t.Fatalf("seed tool cache: %v", err)
	}

	toolsReq := withPrincipal(httptest.NewRequest(http.MethodGet, "/connectors/"+created.ID+"/tools", nil), "tenant-a", "admin")
	w = serve(http.MethodGet, "/connectors/{id}/tools", h.Tools, toolsReq)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var page struct {
		Items []cachedToolView `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 1 || page.Items[0].ToolName != "list_users" {
		t.Errorf("page = %+v", page)
	}
}

// createTestConnector creates a connector in tenantID via the given Deps'
// store and returns its id.
func createTestConnector(t *testing.T, deps Deps, tenantID string) string {
	t.Helper()
	h := Connectors{Deps: deps}
	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), tenantID, "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, createReq)
	if w.Code != http.StatusCreated {
		t.Fatalf("create connector status = %d, body = %s", w.Code, w.Body.String())
	}
	var created connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return created.ID
}

func TestConnectors_Health_Happy(t *testing.T) {
	deps := newTestDeps()
	id := createTestConnector(t, deps, "tenant-a")

	checkedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	deps.ConnectorOps = &fakeConnectorOps{
		store: deps.Store.Connectors(),
		probeResult: ops.ProbeResult{
			Status:       "healthy",
			LatencyMS:    42,
			Capabilities: map[string]any{"tools": true},
			CheckedAt:    checkedAt,
		},
	}
	h := Connectors{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/connectors/"+id+"/health", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/connectors/{id}/health", h.Health, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var got healthView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != "healthy" || got.LatencyMS != 42 || got.Capabilities["tools"] != true {
		t.Errorf("body = %+v", got)
	}
	if got.CheckedAt != checkedAt.Format(time.RFC3339) {
		t.Errorf("CheckedAt = %q, want %q", got.CheckedAt, checkedAt.Format(time.RFC3339))
	}
}

func TestConnectors_Health_WrongTenant404(t *testing.T) {
	deps := newTestDeps()
	id := createTestConnector(t, deps, "tenant-a")
	deps.ConnectorOps = &fakeConnectorOps{store: deps.Store.Connectors()}
	h := Connectors{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/connectors/"+id+"/health", nil), "tenant-b", "admin")
	w := serve(http.MethodGet, "/connectors/{id}/health", h.Health, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "not_found")
}

func TestConnectors_Health_OpsUnavailable503(t *testing.T) {
	h := Connectors{Deps: newTestDeps()} // Deps.ConnectorOps left nil

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/connectors/x/health", nil), "tenant-a", "admin")
	w := serve(http.MethodGet, "/connectors/{id}/health", h.Health, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "unavailable")
}

func TestConnectors_Discover_Happy(t *testing.T) {
	deps := newTestDeps()
	id := createTestConnector(t, deps, "tenant-a")

	deps.ConnectorOps = &fakeConnectorOps{
		store: deps.Store.Connectors(),
		discoverTools: []store.CachedTool{
			{ConnectorID: id, ToolNamespace: "okta", ToolName: "list_users", Description: "List users"},
			{ConnectorID: id, ToolNamespace: "okta", ToolName: "get_user", Description: "Get a user"},
		},
	}
	h := Connectors{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors/"+id+"/discover", nil), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors/{id}/discover", h.Discover, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var page struct {
		Items []cachedToolView `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("page = %+v", page)
	}
	if page.Items[0].ToolName != "list_users" || page.Items[1].ToolName != "get_user" {
		t.Errorf("items = %+v", page.Items)
	}
}

func TestConnectors_Discover_WrongTenant404(t *testing.T) {
	deps := newTestDeps()
	id := createTestConnector(t, deps, "tenant-a")
	deps.ConnectorOps = &fakeConnectorOps{store: deps.Store.Connectors()}
	h := Connectors{Deps: deps}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors/"+id+"/discover", nil), "tenant-b", "admin")
	w := serve(http.MethodPost, "/connectors/{id}/discover", h.Discover, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "not_found")
}

func TestConnectors_Discover_OpsUnavailable503(t *testing.T) {
	h := Connectors{Deps: newTestDeps()} // Deps.ConnectorOps left nil

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors/x/discover", nil), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors/{id}/discover", h.Discover, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "unavailable")
}

func TestUnmaskHeaderConfigsKeepsStoredSecrets(t *testing.T) {
	stored := map[string]any{"headers": map[string]any{
		"X-Static": map[string]any{"type": "static", "value": "hunter2"},
		"X-Sec":    map[string]any{"type": "external", "provider": "secret_store", "config": map[string]any{"credential": "okta", "field": "secret"}},
	}}
	incoming := map[string]any{"headers": map[string]any{
		"X-Static": map[string]any{"type": "static", "value": "***"},
		"X-Sec":    map[string]any{"type": "external", "provider": "secret_store", "config": map[string]any{"credential": "***", "field": "other"}},
		"X-New":    map[string]any{"type": "static", "value": "***"},
	}}
	got := unmaskHeaderConfigs(incoming, stored)["headers"].(map[string]any)
	if got["X-Static"].(map[string]any)["value"] != "hunter2" {
		t.Errorf("static masked value not restored: %+v", got["X-Static"])
	}
	sec := got["X-Sec"].(map[string]any)["config"].(map[string]any)
	if sec["credential"] != "okta" || sec["field"] != "other" {
		t.Errorf("config merge wrong: %+v", sec)
	}
	if got["X-New"].(map[string]any)["value"] != "***" {
		t.Errorf("new header with no stored value should be left as sent: %+v", got["X-New"])
	}
}

func TestConnectors_Create_ServerRequestsRoundTripsAndIsNotMasked(t *testing.T) {
	h := Connectors{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(
		`{"name":"Okta MCP","endpoint":"https://okta.example.com/mcp","metadata":{"server_requests":{"sampling":true,"elicitation":false,"roots":true}}}`)),
		"tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", w.Code, w.Body.String())
	}
	var got connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sr, ok := got.Metadata["server_requests"].(map[string]any)
	if !ok {
		t.Fatalf("metadata.server_requests missing or wrong type: %+v", got.Metadata)
	}
	if sr["sampling"] != true || sr["elicitation"] != false || sr["roots"] != true {
		t.Errorf("server_requests = %+v, want {sampling:true elicitation:false roots:true}", sr)
	}
}

// TestConnectors_Create_ServerRequestsOmittedHasNoKeyOnGet documents the
// chosen "omitted" behavior: like every other optional metadata field
// (tool_arg_overrides, tls, retry), a server_requests the caller never
// sent is not synthesized into the stored/returned metadata -- it's
// simply absent. internal/dataplane/client.ServerRequests is what
// applies the all-false default, for the data plane's own use.
func TestConnectors_Create_ServerRequestsOmittedHasNoKeyOnGet(t *testing.T) {
	h := Connectors{Deps: newTestDeps()}

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(
		`{"name":"Okta MCP","endpoint":"https://okta.example.com/mcp"}`)),
		"tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", w.Code, w.Body.String())
	}
	var got connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := got.Metadata["server_requests"]; present {
		t.Errorf("server_requests should be absent when never sent, got: %+v", got.Metadata["server_requests"])
	}
}

func TestConnectors_Create_ServerRequestsValidationErrors(t *testing.T) {
	h := Connectors{Deps: newTestDeps()}

	cases := map[string]string{
		"unknown key":                   `{"name":"x","endpoint":"https://x.example.com","metadata":{"server_requests":{"sampling":true,"prompts":true}}}`,
		"non-bool value":                `{"name":"x","endpoint":"https://x.example.com","metadata":{"server_requests":{"sampling":"yes"}}}`,
		"server_requests not an object": `{"name":"x","endpoint":"https://x.example.com","metadata":{"server_requests":true}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			req := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(body)), "tenant-a", "admin")
			w := serve(http.MethodPost, "/connectors", h.Create, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
			}
			assertErrorType(t, w, "validation_error")
		})
	}
}

func TestConnectors_Update_ServerRequestsRoundTripsAndValidates(t *testing.T) {
	deps := newTestDeps()
	h := Connectors{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/connectors", strings.NewReader(connectorCreateBody)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/connectors", h.Create, createReq)
	var created connectorView
	json.Unmarshal(w.Body.Bytes(), &created)

	// Valid update: sets sampling on, leaves elicitation/roots at their
	// JSON-explicit false.
	updateReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/connectors/"+created.ID,
		strings.NewReader(`{"name":"Okta MCP","endpoint":"https://okta.example.com/mcp","metadata":{"server_requests":{"sampling":true,"elicitation":false,"roots":false}}}`)),
		"tenant-a", "admin")
	w = serve(http.MethodPut, "/connectors/{id}", h.Update, updateReq)
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", w.Code, w.Body.String())
	}
	var updated connectorView
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sr, ok := updated.Metadata["server_requests"].(map[string]any)
	if !ok || sr["sampling"] != true {
		t.Fatalf("server_requests not applied: %+v", updated.Metadata["server_requests"])
	}

	// Invalid update (unknown key) is rejected and the stored policy is
	// left untouched.
	badReq := withPrincipal(httptest.NewRequest(http.MethodPut, "/connectors/"+created.ID,
		strings.NewReader(`{"name":"Okta MCP","endpoint":"https://okta.example.com/mcp","metadata":{"server_requests":{"sampling":true,"weird":true}}}`)),
		"tenant-a", "admin")
	w = serve(http.MethodPut, "/connectors/{id}", h.Update, badReq)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	assertErrorType(t, w, "validation_error")
}
