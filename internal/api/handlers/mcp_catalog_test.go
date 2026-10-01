package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/ops"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// catalogFixture seeds one entry per auth kind (plus a disabled one) into a
// fresh fake store and returns the handler deps and the entries by slug.
func catalogFixture(t *testing.T) (Deps, map[string]*store.MCPCatalogEntry) {
	t.Helper()
	d := newTestDeps()
	ctx := context.Background()
	seed := []*store.MCPCatalogEntry{
		{Slug: "open", Name: "Open", Description: "No auth", URL: "https://open.example.com/mcp", Enabled: true,
			Auth: store.MCPCatalogAuth{Kind: "none"}, DefaultHeaders: map[string]string{"X-Client": "gw"}},
		{Slug: "tok", Name: "Tok", URL: "https://tok.example.com/mcp", Enabled: true,
			Auth: store.MCPCatalogAuth{Kind: "bearer", Fields: []store.MCPCatalogField{
				{Name: "token", Label: "Token", Secret: true, Required: true},
				{Name: "project", Label: "Project", Query: "project_ref"},
			}}},
		{Slug: "lf", Name: "LF", URL: "https://us.lf.example.com/mcp", URLOverridable: true, Enabled: true,
			Auth: store.MCPCatalogAuth{Kind: "basic", Fields: []store.MCPCatalogField{
				{Name: "pk", Label: "Public", Required: true},
				{Name: "sk", Label: "Secret", Secret: true, Required: true},
			}}},
		{Slug: "hdr", Name: "Hdr", URL: "https://hdr.example.com/mcp", Enabled: true,
			Auth: store.MCPCatalogAuth{Kind: "header", HeaderTemplate: &store.MCPCatalogHeader{Name: "X-Api-Key"},
				Fields: []store.MCPCatalogField{{Name: "key", Label: "Key", Secret: true, Required: true}}}},
		{Slug: "goog", Name: "Goog", URL: "https://goog.example.com/mcp", Enabled: true, Auth: store.MCPCatalogAuth{Kind: "oauth"}},
		{Slug: "off", Name: "Off", URL: "https://off.example.com/mcp", Enabled: false, Auth: store.MCPCatalogAuth{Kind: "none"}},
	}
	out := map[string]*store.MCPCatalogEntry{}
	for _, e := range seed {
		if err := d.Store.MCPCatalog().Upsert(ctx, e); err != nil {
			t.Fatalf("seed %s: %v", e.Slug, err)
		}
		out[e.Slug] = e
	}
	return d, out
}

func addReq(tenant, slug, body string, roles ...string) (*http.Request, string) {
	if len(roles) == 0 {
		roles = []string{"admin"}
	}
	pattern := "/mcp-catalog/{slug}/add"
	return withPrincipal(httptest.NewRequest(http.MethodPost, "/mcp-catalog/"+slug+"/add", strings.NewReader(body)), tenant, roles...), pattern
}

func doAdd(d Deps, tenant, slug, body string) *httptest.ResponseRecorder {
	req, pattern := addReq(tenant, slug, body)
	return serve(http.MethodPost, pattern, MCPCatalog{Deps: d}.Add, req)
}

func errType(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return e.Error.Type
}

func decodeAdd(t *testing.T, w *httptest.ResponseRecorder) addCatalogResponse {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got addCatalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func TestMCPCatalog_List_AddedFlagsAndHiddenDisabled(t *testing.T) {
	d, entries := catalogFixture(t)
	h := MCPCatalog{Deps: d}

	list := func(tenant string) map[string]catalogEntryView {
		t.Helper()
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/mcp-catalog", nil), tenant, "agent")
		w := serve(http.MethodGet, "/mcp-catalog", h.List, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var page struct {
			Items []catalogEntryView `json:"items"`
			Total int                `json:"total"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		out := map[string]catalogEntryView{}
		for _, v := range page.Items {
			out[v.Slug] = v
		}
		if page.Total != len(out) {
			t.Errorf("total = %d, items = %d", page.Total, len(out))
		}
		return out
	}

	before := list("tenant-a")
	if len(before) != 5 {
		t.Fatalf("got %d entries, want 5 (disabled one hidden): %v", len(before), before)
	}
	if _, ok := before["off"]; ok {
		t.Error("disabled entry listed")
	}
	if g := before["goog"]; g.Supported || g.UnsupportedReason == "" {
		t.Errorf("oauth entry = %+v, want unsupported with a reason", g)
	}
	if o := before["open"]; !o.Supported || o.Added || o.ConnectorID != "" {
		t.Errorf("open entry = %+v, want supported and not added", o)
	}

	w := doAdd(d, "tenant-a", "open", `{}`)
	added := decodeAdd(t, w)

	after := list("tenant-a")
	if o := after["open"]; !o.Added || o.ConnectorID != added.Connector.ID {
		t.Errorf("after add, open = %+v, want added with connector id %q", o, added.Connector.ID)
	}
	if after["tok"].Added {
		t.Error("tok should not be added")
	}
	// Another tenant is unaffected.
	if list("tenant-b")["open"].Added {
		t.Error("tenant-b sees tenant-a's add")
	}

	// Removing the connector returns the entry to Available.
	if err := d.Store.Connectors().SoftDelete(context.Background(), "tenant-a", added.Connector.ID); err != nil {
		t.Fatal(err)
	}
	if list("tenant-a")["open"].Added {
		t.Error("entry still added after the connector was deleted")
	}

	// Get by slug.
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/mcp-catalog/lf", nil), "tenant-a", "agent")
	gw := serve(http.MethodGet, "/mcp-catalog/{slug}", h.Get, req)
	var one catalogEntryView
	if gw.Code != http.StatusOK || json.Unmarshal(gw.Body.Bytes(), &one) != nil || one.ID != entries["lf"].ID || len(one.Auth.Fields) != 2 {
		t.Errorf("Get lf = %d %s", gw.Code, gw.Body.String())
	}
	for _, slug := range []string{"off", "missing"} {
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/mcp-catalog/"+slug, nil), "tenant-a", "agent")
		if w := serve(http.MethodGet, "/mcp-catalog/{slug}", h.Get, req); w.Code != http.StatusNotFound {
			t.Errorf("Get %s = %d, want 404", slug, w.Code)
		}
	}
}

func TestMCPCatalog_Add_NoAuth(t *testing.T) {
	d, entries := catalogFixture(t)
	got := decodeAdd(t, doAdd(d, "tenant-a", "open", `{}`))

	c := got.Connector
	if c.Slug != "open" || c.Name != "Open" || c.Endpoint != "https://open.example.com/mcp" || c.CatalogID != entries["open"].ID {
		t.Errorf("connector = %+v", c)
	}
	headers, _ := c.Metadata["headers"].(map[string]any)
	if len(headers) != 1 {
		t.Errorf("headers = %+v, want only the default header", headers)
	}
	creds, _ := d.Secrets.List(context.Background(), "tenant-a")
	if len(creds) != 0 {
		t.Errorf("a no-auth add created %d credentials", len(creds))
	}
}

func TestMCPCatalog_Add_Bearer_StoresCredentialAndScopesURL(t *testing.T) {
	d, _ := catalogFixture(t)
	w := doAdd(d, "tenant-a", "tok", `{"fields":{"token":"sbp_supersecret","project":"abc123"}}`)
	got := decodeAdd(t, w)
	if strings.Contains(w.Body.String(), "sbp_supersecret") {
		t.Fatalf("response leaked the token: %s", w.Body.String())
	}
	if got.Connector.Endpoint != "https://tok.example.com/mcp?project_ref=abc123" {
		t.Errorf("endpoint = %q", got.Connector.Endpoint)
	}

	ctx := context.Background()
	stored, err := d.Secrets.Get(ctx, "tenant-a", "mcp-tok")
	if err != nil {
		t.Fatalf("credential mcp-tok: %v", err)
	}
	if stored["authorization"] != "Bearer sbp_supersecret" {
		t.Errorf("stored payload = %v", stored)
	}
	raw, _ := d.Store.Connectors().Get(ctx, "tenant-a", got.Connector.ID)
	hdr := raw.Metadata["headers"].(map[string]any)["Authorization"].(map[string]any)
	cfg := hdr["config"].(map[string]any)
	if hdr["type"] != "external" || hdr["provider"] != "secret_store" || cfg["credential"] != "mcp-tok" || cfg["field"] != "authorization" {
		t.Errorf("Authorization header = %+v", hdr)
	}
	if strings.Contains(toJSON(t, raw.Metadata), "sbp_supersecret") {
		t.Error("connector metadata contains the token")
	}
	// Credential metadata is masked: only the field name is exposed.
	meta, _ := d.Secrets.List(ctx, "tenant-a")
	if len(meta) != 1 || meta[0].Type != "mcp_catalog" || len(meta[0].FieldNames) != 1 {
		t.Errorf("credential metadata = %+v", meta)
	}
}

func TestMCPCatalog_Add_Basic_And_Header(t *testing.T) {
	d, _ := catalogFixture(t)
	decodeAdd(t, doAdd(d, "tenant-a", "lf", `{"fields":{"pk":"pk-lf-1","sk":"sk-lf-2"},"name":"Langfuse US"}`))
	ctx := context.Background()
	stored, err := d.Secrets.Get(ctx, "tenant-a", "mcp-lf")
	if err != nil {
		t.Fatal(err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-lf-1:sk-lf-2"))
	if stored["authorization"] != want {
		t.Errorf("basic header = %q, want %q", stored["authorization"], want)
	}

	decodeAdd(t, doAdd(d, "tenant-a", "hdr", `{"fields":{"key":"k-123"}}`))
	stored, err = d.Secrets.Get(ctx, "tenant-a", "mcp-hdr")
	if err != nil || stored["x-api-key"] != "k-123" {
		t.Errorf("header credential = %v (err %v)", stored, err)
	}
	c, _ := d.Store.Connectors().GetBySlug(ctx, "tenant-a", "hdr")
	if _, ok := c.Metadata["headers"].(map[string]any)["X-Api-Key"]; !ok {
		t.Errorf("headers = %+v, want X-Api-Key", c.Metadata["headers"])
	}
}

func TestMCPCatalog_Add_URLOverride(t *testing.T) {
	d, _ := catalogFixture(t)
	got := decodeAdd(t, doAdd(d, "tenant-a", "lf", `{"fields":{"pk":"a","sk":"b"},"url":"https://eu.lf.example.com/mcp"}`))
	if got.Connector.Endpoint != "https://eu.lf.example.com/mcp" {
		t.Errorf("endpoint = %q", got.Connector.Endpoint)
	}
	// Not overridable, or not https.
	if w := doAdd(d, "tenant-a", "tok", `{"fields":{"token":"t"},"url":"https://evil.example.com"}`); w.Code != http.StatusBadRequest {
		t.Errorf("override on a fixed-URL entry = %d, want 400", w.Code)
	}
	if w := doAdd(d, "tenant-a", "lf", `{"fields":{"pk":"a","sk":"b"},"slug":"lf-2","url":"http://plain.example.com"}`); w.Code != http.StatusBadRequest {
		t.Errorf("http override = %d, want 400", w.Code)
	}
}

func TestMCPCatalog_Add_Validation(t *testing.T) {
	d, _ := catalogFixture(t)
	cases := map[string]string{
		`{"fields":{"pk":"a"}}`:                                 "missing required secret",
		`{"fields":{"pk":"a","sk":"b","zzz":"c"}}`:              "unknown field",
		`{"fields":{"pk":"a","sk":"b\nInjected: 1"}}`:           "control character",
		`{"fields":{"pk":"   ","sk":"b"}}`:                      "blank required",
		`{"fields":{"pk":"a","sk":"b"},"slug":"Bad!"}`:          "bad slug",
		`{"fields":{"pk":"a","sk":"b"},"slug":"GATEWAY"}`:       "reserved slug",
		`{"fields":{"pk":"a","sk":"b"},"tool_allowlist":["x"]}`: "unknown body field",
	}
	for body, why := range cases {
		w := doAdd(d, "tenant-a", "lf", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", why, w.Code, w.Body.String())
		}
	}
	conns, _ := d.Store.Connectors().List(context.Background(), "tenant-a")
	creds, _ := d.Secrets.List(context.Background(), "tenant-a")
	if len(conns) != 0 || len(creds) != 0 {
		t.Errorf("rejected adds left %d connectors and %d credentials", len(conns), len(creds))
	}
	if w := doAdd(d, "tenant-a", "missing", `{}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown slug = %d, want 404", w.Code)
	}
	if w := doAdd(d, "tenant-a", "off", `{}`); w.Code != http.StatusNotFound {
		t.Errorf("disabled entry = %d, want 404", w.Code)
	}
}

func TestMCPCatalog_Add_OAuthUnsupported(t *testing.T) {
	d, _ := catalogFixture(t)
	w := doAdd(d, "tenant-a", "goog", `{}`)
	if w.Code != http.StatusUnprocessableEntity || errType(t, w) != "oauth_not_supported" {
		t.Fatalf("status = %d, type = %q, body = %s", w.Code, errType(t, w), w.Body.String())
	}
	if conns, _ := d.Store.Connectors().List(context.Background(), "tenant-a"); len(conns) != 0 {
		t.Error("oauth add created a connector")
	}
}

func TestMCPCatalog_Add_Duplicate(t *testing.T) {
	d, _ := catalogFixture(t)
	decodeAdd(t, doAdd(d, "tenant-a", "open", `{}`))

	w := doAdd(d, "tenant-a", "open", `{}`)
	if w.Code != http.StatusConflict || errType(t, w) != "already_added" {
		t.Fatalf("second add = %d %q, want 409 already_added", w.Code, errType(t, w))
	}
	// The same slug explicitly is still already_added.
	if w := doAdd(d, "tenant-a", "open", `{"slug":"open"}`); w.Code != http.StatusConflict || errType(t, w) != "already_added" {
		t.Errorf("explicit same slug = %d %q", w.Code, errType(t, w))
	}
	// A different slug adds it again.
	again := decodeAdd(t, doAdd(d, "tenant-a", "open", `{"slug":"open-2","name":"Open second"}`))
	if again.Connector.Slug != "open-2" {
		t.Errorf("slug = %q", again.Connector.Slug)
	}
	// Renaming the first connector's slug away does not let a plain add through.
	first, _ := d.Store.Connectors().GetBySlug(context.Background(), "tenant-a", "open")
	first.Slug = "renamed"
	if err := d.Store.Connectors().Update(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if w := doAdd(d, "tenant-a", "open", `{}`); w.Code != http.StatusConflict || errType(t, w) != "already_added" {
		t.Errorf("add after slug rename = %d %q", w.Code, errType(t, w))
	}
	// A slug held by an unrelated custom connector is a plain conflict.
	custom := &store.Connector{TenantID: "tenant-a", Name: "Mine", Slug: "tok", Endpoint: "https://mine.example.com"}
	if err := d.Store.Connectors().Create(context.Background(), custom); err != nil {
		t.Fatal(err)
	}
	if w := doAdd(d, "tenant-a", "tok", `{"fields":{"token":"t"}}`); w.Code != http.StatusConflict || errType(t, w) != "conflict" {
		t.Errorf("slug held by custom connector = %d %q, want 409 conflict", w.Code, errType(t, w))
	}
	if creds, _ := d.Secrets.List(context.Background(), "tenant-a"); len(creds) != 0 {
		t.Errorf("a rejected add left %d credentials", len(creds))
	}
}

func TestMCPCatalog_Add_TenantIsolation(t *testing.T) {
	d, _ := catalogFixture(t)
	decodeAdd(t, doAdd(d, "tenant-a", "tok", `{"fields":{"token":"a-secret"}}`))
	// tenant-b can add the same entry: an add in tenant-a is not theirs.
	decodeAdd(t, doAdd(d, "tenant-b", "tok", `{"fields":{"token":"b-secret"}}`))

	ctx := context.Background()
	a, _ := d.Secrets.Get(ctx, "tenant-a", "mcp-tok")
	b, _ := d.Secrets.Get(ctx, "tenant-b", "mcp-tok")
	if a["authorization"] != "Bearer a-secret" || b["authorization"] != "Bearer b-secret" {
		t.Errorf("credentials crossed tenants: a=%v b=%v", a, b)
	}
	ac, _ := d.Store.Connectors().List(ctx, "tenant-a")
	bc, _ := d.Store.Connectors().List(ctx, "tenant-b")
	if len(ac) != 1 || len(bc) != 1 || ac[0].ID == bc[0].ID {
		t.Errorf("connectors: a=%d b=%d", len(ac), len(bc))
	}
}

func TestMCPCatalog_Add_ReusesOnlyItsOwnUnusedCredential(t *testing.T) {
	d, _ := catalogFixture(t)
	ctx := context.Background()

	// A hand-made credential with the default name is never overwritten.
	if _, err := d.Secrets.Create(ctx, "tenant-a", "mcp-tok", "pat", map[string]string{"token": "hand-made"}, "x"); err != nil {
		t.Fatal(err)
	}
	got := decodeAdd(t, doAdd(d, "tenant-a", "tok", `{"fields":{"token":"new"}}`))
	hand, _ := d.Secrets.Get(ctx, "tenant-a", "mcp-tok")
	if hand["token"] != "hand-made" {
		t.Errorf("hand-made credential was overwritten: %v", hand)
	}
	next, err := d.Secrets.Get(ctx, "tenant-a", "mcp-tok-2")
	if err != nil || next["authorization"] != "Bearer new" {
		t.Errorf("mcp-tok-2 = %v (err %v)", next, err)
	}

	// After the connector is deleted its credential is an unused leftover:
	// a fresh add rotates it in place instead of piling up new ones.
	if err := d.Store.Connectors().SoftDelete(ctx, "tenant-a", got.Connector.ID); err != nil {
		t.Fatal(err)
	}
	decodeAdd(t, doAdd(d, "tenant-a", "tok", `{"fields":{"token":"newer"}}`))
	next, _ = d.Secrets.Get(ctx, "tenant-a", "mcp-tok-2")
	if next["authorization"] != "Bearer newer" {
		t.Errorf("leftover not rotated: %v", next)
	}
	if creds, _ := d.Secrets.List(ctx, "tenant-a"); len(creds) != 2 {
		t.Errorf("credentials = %d, want 2", len(creds))
	}
}

func TestMCPCatalog_Add_ReportsProbeAndDiscovery(t *testing.T) {
	d, _ := catalogFixture(t)
	fs := d.Store.(*fakeStore)
	n := 0
	add := func(o *fakeConnectorOps) discoveryView {
		t.Helper()
		d.ConnectorOps = o
		n++
		body := fmt.Sprintf(`{"slug":"open-%d"}`, n)
		return decodeAdd(t, doAdd(d, "tenant-a", "open", body)).Discovery
	}

	// Healthy: tools are discovered and counted.
	got := add(&fakeConnectorOps{store: fs.connectors, probeResult: ops.ProbeResult{Status: "healthy"},
		discoverTools: []store.CachedTool{{ToolName: "a"}, {ToolName: "b"}}})
	if got.Status != "healthy" || got.ToolsDiscovered != 2 || got.DiscoveryError != "" {
		t.Errorf("healthy = %+v", got)
	}

	// Unhealthy (bad credentials): reported honestly, discovery skipped.
	got = add(&fakeConnectorOps{store: fs.connectors, probeResult: ops.ProbeResult{Status: "unhealthy", Error: "401 Unauthorized"},
		discoverTools: []store.CachedTool{{ToolName: "a"}}})
	if got.Status != "unhealthy" || got.Error != "401 Unauthorized" || got.ToolsDiscovered != 0 {
		t.Errorf("unhealthy = %+v", got)
	}

	// Probe infrastructure failure does not fail the add.
	got = add(&fakeConnectorOps{store: fs.connectors, probeErr: errors.New("boom")})
	if got.Status != "unknown" || got.Error == "" {
		t.Errorf("probe error = %+v", got)
	}

	// Healthy probe, failing discovery.
	got = add(&fakeConnectorOps{store: fs.connectors, probeResult: ops.ProbeResult{Status: "healthy"}, discoverErr: errors.New("boom")})
	if got.Status != "healthy" || got.DiscoveryError == "" || got.ToolsDiscovered != 0 {
		t.Errorf("discover error = %+v", got)
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
