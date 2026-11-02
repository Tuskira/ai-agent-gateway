package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dataplaneheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
)

// newSourceTestDeps is newTestDeps plus the external_service provider and
// the registry's source lookup over the same store, as buildSecretsDeps
// wires them, and a real HTTP server speaking the credential-service
// contract (options listing only; it accepts token "gw-token").
func newSourceTestDeps(t *testing.T) (Deps, *httptest.Server) {
	t.Helper()
	d := newTestDeps()
	if err := d.Headers.RegisterExternal(secrets.NewExternalServiceProvider(d.Secrets.Fetcher(), 2*time.Second, time.Minute)); err != nil {
		t.Fatal(err)
	}
	d.Headers.SetSourceLookup(dataplaneheaders.StoreSourceLookup(d.Store.CredentialSources()))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gw-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/credentials/options" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"options":[]}`))
	}))
	t.Cleanup(srv.Close)
	return d, srv
}

func sourceReq(t *testing.T, method, pattern, path string, h http.HandlerFunc, tenant, body string, dst any) int {
	t.Helper()
	req := withPrincipal(httptest.NewRequest(method, path, strings.NewReader(body)), tenant, "admin")
	req.Header.Set("Content-Type", "application/json")
	w := serve(method, pattern, h, req)
	if dst != nil && w.Code < 300 {
		if err := json.Unmarshal(w.Body.Bytes(), dst); err != nil {
			t.Fatalf("decode %s %s: %v (%s)", method, path, err, w.Body.String())
		}
	}
	return w.Code
}

func TestCredentialSources_CRUDAndUsage(t *testing.T) {
	d, srv := newSourceTestDeps(t)
	h := CredentialSources{Deps: d}
	conns := Connectors{Deps: d}

	sourceBody := `{"name":"vault-a","provider":"external_service","config":{"url":"` + srv.URL + `","auth_credential":"vault-auth"}}`

	// Validation.
	for name, body := range map[string]string{
		"unknown provider":   `{"name":"x","provider":"nope","config":{}}`,
		"no-source provider": `{"name":"x","provider":"secret_store","config":{}}`,
		"missing url":        `{"name":"x","provider":"external_service","config":{"auth_credential":"a"}}`,
		"plain http public":  `{"name":"x","provider":"external_service","config":{"url":"http://creds.example.com","auth_credential":"a"}}`,
		"empty name":         `{"name":"","provider":"external_service","config":{"url":"https://creds.example.com","auth_credential":"a"}}`,
	} {
		if code := sourceReq(t, http.MethodPost, "/credential-sources", "/credential-sources", h.Create, "tenant-a", body, nil); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, code)
		}
	}

	var created credentialSourceView
	if code := sourceReq(t, http.MethodPost, "/credential-sources", "/credential-sources", h.Create, "tenant-a", sourceBody, &created); code != http.StatusCreated {
		t.Fatalf("create status = %d", code)
	}
	if !created.Enabled || created.Provider != "external_service" || created.ID == "" {
		t.Errorf("created = %+v", created)
	}
	if code := sourceReq(t, http.MethodPost, "/credential-sources", "/credential-sources", h.Create, "tenant-a", sourceBody, nil); code != http.StatusConflict {
		t.Errorf("duplicate name status = %d, want 409", code)
	}

	// Tenant isolation.
	if code := sourceReq(t, http.MethodGet, "/credential-sources/{id}", "/credential-sources/"+created.ID, h.Get, "tenant-b", "", nil); code != http.StatusNotFound {
		t.Errorf("other tenant Get status = %d, want 404", code)
	}

	// A connector header naming the source: a missing source is refused at
	// save time, the real one is accepted.
	connBody := func(source string) string {
		return `{"name":"splunk","endpoint":"https://splunk.example.com/mcp","metadata":{"headers":{"Authorization":{"type":"external","provider":"external_service","source":"` + source + `","config":{"credential_id":"splunk-prod","field":"token","prefix":"Bearer "}}}}}`
	}
	if code := sourceReq(t, http.MethodPost, "/connectors", "/connectors", conns.Create, "tenant-a", connBody("missing"), nil); code != http.StatusBadRequest {
		t.Errorf("connector with missing source status = %d, want 400", code)
	}
	if code := sourceReq(t, http.MethodPost, "/connectors", "/connectors", conns.Create, "tenant-b", connBody("vault-a"), nil); code != http.StatusBadRequest {
		t.Errorf("connector naming another tenant's source status = %d, want 400", code)
	}
	var conn connectorView
	if code := sourceReq(t, http.MethodPost, "/connectors", "/connectors", conns.Create, "tenant-a", connBody("vault-a"), &conn); code != http.StatusCreated {
		t.Fatalf("connector create status = %d", code)
	}

	var got credentialSourceView
	sourceReq(t, http.MethodGet, "/credential-sources/{id}", "/credential-sources/"+created.ID, h.Get, "tenant-a", "", &got)
	if len(got.UsedBy) != 1 || got.UsedBy[0].ID != conn.ID {
		t.Errorf("used_by = %+v, want the connector", got.UsedBy)
	}

	// Referenced: delete and rename are refused; disable is fine.
	if code := sourceReq(t, http.MethodDelete, "/credential-sources/{id}", "/credential-sources/"+created.ID, h.Delete, "tenant-a", "", nil); code != http.StatusConflict {
		t.Errorf("delete while referenced status = %d, want 409", code)
	}
	if code := sourceReq(t, http.MethodPut, "/credential-sources/{id}", "/credential-sources/"+created.ID, h.Update, "tenant-a", `{"name":"renamed"}`, nil); code != http.StatusConflict {
		t.Errorf("rename while referenced status = %d, want 409", code)
	}
	var updated credentialSourceView
	if code := sourceReq(t, http.MethodPut, "/credential-sources/{id}", "/credential-sources/"+created.ID, h.Update, "tenant-a", `{"enabled":false}`, &updated); code != http.StatusOK || updated.Enabled {
		t.Errorf("disable status = %d, enabled = %v", code, updated.Enabled)
	}
	if code := sourceReq(t, http.MethodPut, "/credential-sources/{id}", "/credential-sources/"+created.ID, h.Update, "tenant-a", `{"config":{"url":"ftp://x","auth_credential":"a"}}`, nil); code != http.StatusBadRequest {
		t.Errorf("bad config update status = %d, want 400", code)
	}

	// Unreferenced: delete works.
	if code := sourceReq(t, http.MethodDelete, "/connectors/{id}", "/connectors/"+conn.ID, conns.Delete, "tenant-a", "", nil); code != http.StatusNoContent {
		t.Fatalf("connector delete status = %d", code)
	}
	if code := sourceReq(t, http.MethodDelete, "/credential-sources/{id}", "/credential-sources/"+created.ID, h.Delete, "tenant-a", "", nil); code != http.StatusNoContent {
		t.Errorf("delete status = %d, want 204", code)
	}
	var list struct {
		Items []credentialSourceView `json:"items"`
		Total int                    `json:"total"`
	}
	sourceReq(t, http.MethodGet, "/credential-sources", "/credential-sources", h.List, "tenant-a", "", &list)
	if list.Total != 0 {
		t.Errorf("list after delete = %+v", list)
	}
}

func TestCredentialSources_Test(t *testing.T) {
	d, srv := newSourceTestDeps(t)
	h := CredentialSources{Deps: d}
	body := `{"name":"vault-a","provider":"external_service","config":{"url":"` + srv.URL + `","auth_credential":"vault-auth"}}`
	var src credentialSourceView
	if code := sourceReq(t, http.MethodPost, "/credential-sources", "/credential-sources", h.Create, "tenant-a", body, &src); code != http.StatusCreated {
		t.Fatalf("create status = %d", code)
	}

	test := func() credentialSourceTestResult {
		var res credentialSourceTestResult
		if code := sourceReq(t, http.MethodPost, "/credential-sources/{id}/test", "/credential-sources/"+src.ID+"/test", h.Test, "tenant-a", "", &res); code != http.StatusOK {
			t.Fatalf("test status = %d", code)
		}
		return res
	}

	// No auth credential yet.
	if res := test(); res.OK || !strings.Contains(res.Error, "vault-auth") {
		t.Errorf("test without auth credential = %+v", res)
	}
	if _, err := d.Secrets.Create(context.Background(), "tenant-a", "vault-auth", "generic", map[string]string{"token": "wrong"}, "u"); err != nil {
		t.Fatal(err)
	}
	if res := test(); res.OK || !strings.Contains(res.Error, "401") {
		t.Errorf("test with a rejected token = %+v", res)
	}
	if err := d.Secrets.Rotate(context.Background(), "tenant-a", "vault-auth", map[string]string{"token": "gw-token"}); err != nil {
		t.Fatal(err)
	}
	if res := test(); !res.OK || res.Error != "" {
		t.Errorf("test with the right token = %+v", res)
	}
}

func TestHeaders_List_SourceSchemaAndTenantSources(t *testing.T) {
	d, srv := newSourceTestDeps(t)
	h := CredentialSources{Deps: d}
	mk := func(tenant, name string, enabled bool) {
		body := `{"name":"` + name + `","provider":"external_service","enabled":` + map[bool]string{true: "true", false: "false"}[enabled] +
			`,"config":{"url":"` + srv.URL + `","auth_credential":"a"}}`
		if code := sourceReq(t, http.MethodPost, "/credential-sources", "/credential-sources", h.Create, tenant, body, nil); code != http.StatusCreated {
			t.Fatalf("create %s status = %d", name, code)
		}
	}
	mk("tenant-a", "zeta", true)
	mk("tenant-a", "alpha", true)
	mk("tenant-a", "off", false)
	mk("tenant-b", "b-only", true)

	var got providersResponse
	sourceReq(t, http.MethodGet, "/headers/providers", "/headers/providers", Headers{Deps: d}.List, "tenant-a", "", &got)
	var ext, ss *providerView
	for i := range got.Providers {
		switch got.Providers[i].ID {
		case "external_service":
			ext = &got.Providers[i]
		case "secret_store":
			ss = &got.Providers[i]
		}
	}
	if ext == nil || ss == nil {
		t.Fatalf("providers = %+v", got.Providers)
	}
	if ext.SourceSchema == nil {
		t.Error("external_service has no source_schema")
	}
	if strings.Join(ext.Sources, ",") != "alpha,zeta" {
		t.Errorf("sources = %v, want [alpha zeta]", ext.Sources)
	}
	if ss.SourceSchema != nil || ss.Sources != nil {
		t.Errorf("secret_store gained source fields: %+v", ss)
	}
}
