package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// recordingInvalidator is a ModelInvalidator that remembers every tenant
// it was asked to drop.
type recordingInvalidator struct {
	mu      sync.Mutex
	tenants []string
}

func (r *recordingInvalidator) Invalidate(tenantID string) {
	r.mu.Lock()
	r.tenants = append(r.tenants, tenantID)
	r.mu.Unlock()
}

func (r *recordingInvalidator) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tenants)
}

// newModelDeps is newTestDeps plus a credential "anthropic-prod" in
// tenant-a (so a target may reference it), a platform model "haiku"
// every tenant sees, and a recording invalidator.
func newModelDeps(t *testing.T) (Deps, *fakeStore, *recordingInvalidator) {
	t.Helper()
	deps := newTestDeps()
	fs := deps.Store.(*fakeStore)
	if _, err := deps.Secrets.Create(context.Background(), "tenant-a", "anthropic-prod", "api_key", map[string]string{"api_key": "sk-ant-secret"}, "test"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	plat := &store.Model{Name: "haiku", Enabled: true, Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "claude-haiku-4-5"}}}
	if err := fs.Models().Create(context.Background(), plat); err != nil {
		t.Fatalf("seed platform model: %v", err)
	}
	inv := &recordingInvalidator{}
	deps.ModelInvalidator = inv
	return deps, fs, inv
}

const modelCreateBody = `{
	"name":"sonnet",
	"description":"Team default",
	"targets":[
		{"vendor":"anthropic","model":"claude-sonnet-4-5","credential":"anthropic-prod"},
		{"vendor":"bedrock","model":"us.anthropic.claude-sonnet-4-5-20250929-v1:0","region":"us-east-1"}
	],
	"price":{"input":3,"output":15,"cache_read":0.3,"cache_write":3.75},
	"metadata":{"team":"platform"}
}`

func postModel(t *testing.T, h Models, tenant, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/models", strings.NewReader(body)), tenant, "admin")
	return serve(http.MethodPost, "/models", h.Create, req)
}

func decodeModel(t *testing.T, w *httptest.ResponseRecorder) modelView {
	t.Helper()
	var v modelView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return v
}

func TestModels_Create_DefaultsAndShape(t *testing.T) {
	deps, _, inv := newModelDeps(t)
	h := Models{Deps: deps}

	w := postModel(t, h, "tenant-a", modelCreateBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-ant-secret") {
		t.Fatalf("response leaked a credential value: %s", w.Body.String())
	}
	got := decodeModel(t, w)
	if got.ID == "" || got.Name != "sonnet" || !got.Enabled || got.Scope != "tenant" || got.Description != "Team default" {
		t.Errorf("view = %+v", got)
	}
	if len(got.Targets) != 2 || got.Targets[0].Credential != "anthropic-prod" || got.Targets[1].Region != "us-east-1" {
		t.Errorf("targets = %+v", got.Targets)
	}
	if got.Price == nil || got.Price.Output != 15 {
		t.Errorf("price = %+v", got.Price)
	}
	if got.Metadata["team"] != "platform" {
		t.Errorf("metadata = %+v", got.Metadata)
	}
	if inv.count() != 1 {
		t.Errorf("invalidator called %d times, want 1", inv.count())
	}

	// enabled:false is honoured; metadata defaults to {} and price to null.
	w = postModel(t, h, "tenant-a", `{"name":"off","enabled":false,"targets":[{"vendor":"openai_compat","model":"gpt-4o-mini","base_url":"https://llm.internal/v1"}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	got = decodeModel(t, w)
	if got.Enabled || got.Price != nil || got.Metadata == nil {
		t.Errorf("view = %+v", got)
	}
	if !strings.Contains(w.Body.String(), `"price":null`) || !strings.Contains(w.Body.String(), `"metadata":{}`) {
		t.Errorf("body should render price null and metadata {}: %s", w.Body.String())
	}
}

func TestModels_Create_Validation(t *testing.T) {
	deps, _, inv := newModelDeps(t)
	h := Models{Deps: deps}

	cases := map[string]string{
		"uppercase name":             `{"name":"Sonnet","targets":[{"vendor":"anthropic","model":"x"}]}`,
		"name too long":              `{"name":"` + strings.Repeat("a", 65) + `","targets":[{"vendor":"anthropic","model":"x"}]}`,
		"no targets":                 `{"name":"sonnet","targets":[]}`,
		"unknown vendor":             `{"name":"sonnet","targets":[{"vendor":"cohere","model":"x"}]}`,
		"empty vendor model":         `{"name":"sonnet","targets":[{"vendor":"anthropic","model":""}]}`,
		"openai_compat w/o base_url": `{"name":"sonnet","targets":[{"vendor":"openai_compat","model":"x"}]}`,
		"bedrock w/o region":         `{"name":"sonnet","targets":[{"vendor":"bedrock","model":"anthropic.x"}]}`,
		"bedrock bad region":         `{"name":"sonnet","targets":[{"vendor":"bedrock","model":"anthropic.x","region":"evil.host/"}]}`,
		"region on anthropic":        `{"name":"sonnet","targets":[{"vendor":"anthropic","model":"x","region":"us-east-1"}]}`,
		"http non-loopback base_url": `{"name":"sonnet","targets":[{"vendor":"openai_compat","model":"x","base_url":"http://llm.internal/v1"}]}`,
		"base_url with query":        `{"name":"sonnet","targets":[{"vendor":"anthropic","model":"x","base_url":"https://a.example.com/?k=1"}]}`,
		"missing credential":         `{"name":"sonnet","targets":[{"vendor":"anthropic","model":"x","credential":"nope"}]}`,
		"negative price":             `{"name":"sonnet","targets":[{"vendor":"anthropic","model":"x"}],"price":{"input":-1}}`,
		"flag with credential":       `{"name":"sonnet","targets":[{"vendor":"anthropic","model":"x","credential":"anthropic-prod","allow_caller_key":true}]}`,
		"too many targets":           `{"name":"sonnet","targets":[` + strings.Repeat(`{"vendor":"anthropic","model":"x"},`, 8) + `{"vendor":"anthropic","model":"x"}]}`,
	}
	for name, body := range cases {
		w := postModel(t, h, "tenant-a", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", name, w.Code, w.Body.String())
			continue
		}
		var e struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Error.Type != httpx.TypeValidation {
			t.Errorf("%s: body = %s", name, w.Body.String())
		}
	}
	if inv.count() != 0 {
		t.Errorf("a rejected write must not invalidate the cache (%d calls)", inv.count())
	}

	// A loopback http base_url is fine (local development upstreams).
	w := postModel(t, h, "tenant-a", `{"name":"local","targets":[{"vendor":"openai_compat","model":"x","base_url":"http://127.0.0.1:11434/v1"}]}`)
	if w.Code != http.StatusCreated {
		t.Errorf("loopback http base_url: status = %d (%s)", w.Code, w.Body.String())
	}

	// A credential that exists in ANOTHER tenant is still "missing" here.
	w = postModel(t, h, "tenant-b", `{"name":"sonnet","targets":[{"vendor":"anthropic","model":"x","credential":"anthropic-prod"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("cross-tenant credential: status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

func TestModels_Create_DuplicateAndPlatformShadow(t *testing.T) {
	deps, _, _ := newModelDeps(t)
	h := Models{Deps: deps}

	if w := postModel(t, h, "tenant-a", modelCreateBody); w.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", w.Code, w.Body.String())
	}
	if w := postModel(t, h, "tenant-a", modelCreateBody); w.Code != http.StatusConflict {
		t.Errorf("duplicate name: status = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	// Same name in another tenant is fine.
	if w := postModel(t, h, "tenant-b", `{"name":"sonnet","targets":[{"vendor":"anthropic","model":"x"}]}`); w.Code != http.StatusCreated {
		t.Errorf("same name, other tenant: status = %d (%s)", w.Code, w.Body.String())
	}
	// Shadowing the platform row "haiku" is allowed and becomes a tenant row.
	w := postModel(t, h, "tenant-a", `{"name":"haiku","targets":[{"vendor":"openai_compat","model":"gpt-4o-mini","base_url":"https://llm.internal/v1"}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("shadow platform row: %d %s", w.Code, w.Body.String())
	}
	if got := decodeModel(t, w); got.Scope != "tenant" {
		t.Errorf("shadow scope = %q, want tenant", got.Scope)
	}
}

func TestModels_List_MergesPlatformRows(t *testing.T) {
	deps, _, _ := newModelDeps(t)
	h := Models{Deps: deps}

	if w := postModel(t, h, "tenant-a", modelCreateBody); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	list := func(tenant string) []modelView {
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/models", nil), tenant, "agent")
		w := serve(http.MethodGet, "/models", h.List, req)
		if w.Code != http.StatusOK {
			t.Fatalf("list(%s): %d %s", tenant, w.Code, w.Body.String())
		}
		var page struct {
			Items []modelView `json:"items"`
			Total int         `json:"total"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if page.Total != len(page.Items) {
			t.Errorf("total = %d, items = %d", page.Total, len(page.Items))
		}
		return page.Items
	}

	a := list("tenant-a")
	if len(a) != 2 || a[0].Name != "haiku" || a[0].Scope != "platform" || a[1].Name != "sonnet" || a[1].Scope != "tenant" {
		t.Errorf("tenant-a list = %+v; want [haiku(platform), sonnet(tenant)] by name", a)
	}
	b := list("tenant-b")
	if len(b) != 1 || b[0].Name != "haiku" || b[0].Scope != "platform" {
		t.Errorf("tenant-b list = %+v; want only the platform row", b)
	}

	// After tenant-a shadows "haiku", its list shows the tenant row once.
	if w := postModel(t, h, "tenant-a", `{"name":"haiku","targets":[{"vendor":"anthropic","model":"claude-haiku-4-5","credential":"anthropic-prod"}]}`); w.Code != http.StatusCreated {
		t.Fatalf("shadow: %d %s", w.Code, w.Body.String())
	}
	a = list("tenant-a")
	if len(a) != 2 || a[0].Name != "haiku" || a[0].Scope != "tenant" {
		t.Errorf("tenant-a list after shadow = %+v; want the tenant's haiku only", a)
	}
	if b = list("tenant-b"); len(b) != 1 || b[0].Scope != "platform" {
		t.Errorf("tenant-b list after tenant-a's shadow = %+v; must still see the platform row", b)
	}
}

func TestModels_GetUpdateDelete(t *testing.T) {
	deps, fs, inv := newModelDeps(t)
	h := Models{Deps: deps}

	created := decodeModel(t, postModel(t, h, "tenant-a", modelCreateBody))
	platform, err := fs.Models().GetByName(context.Background(), "tenant-b", "haiku")
	if err != nil {
		t.Fatal(err)
	}
	inv.tenants = nil

	get := func(tenant, id string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/models/"+id, nil), tenant, "agent")
		return serve(http.MethodGet, "/models/{id}", h.Get, req)
	}
	put := func(tenant, id, body string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodPut, "/models/"+id, strings.NewReader(body)), tenant, "admin")
		return serve(http.MethodPut, "/models/{id}", h.Update, req)
	}
	del := func(tenant, id string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodDelete, "/models/"+id, nil), tenant, "admin")
		return serve(http.MethodDelete, "/models/{id}", h.Delete, req)
	}

	// Get: own row, platform row (any tenant), never another tenant's row.
	if w := get("tenant-a", created.ID); w.Code != http.StatusOK || decodeModel(t, w).Name != "sonnet" {
		t.Errorf("get own: %d %s", w.Code, w.Body.String())
	}
	if w := get("tenant-b", platform.ID); w.Code != http.StatusOK || decodeModel(t, w).Scope != "platform" {
		t.Errorf("get platform: %d %s", w.Code, w.Body.String())
	}
	if w := get("tenant-b", created.ID); w.Code != http.StatusNotFound {
		t.Errorf("get other tenant's row: %d, want 404", w.Code)
	}

	// Update: full replace of name/targets/price; metadata kept when omitted.
	w := put("tenant-a", created.ID, `{"name":"sonnet-v2","description":"d2","enabled":false,"targets":[{"vendor":"anthropic","model":"claude-sonnet-4-5"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	got := decodeModel(t, w)
	if got.Name != "sonnet-v2" || got.Description != "d2" || got.Enabled || len(got.Targets) != 1 || got.Price != nil || got.Metadata["team"] != "platform" {
		t.Errorf("update view = %+v (price %+v)", got, got.Price)
	}
	if inv.count() != 1 {
		t.Errorf("invalidator after update called %d times, want 1", inv.count())
	}
	// Validation applies to updates too; a bad update changes nothing.
	if w := put("tenant-a", created.ID, `{"name":"sonnet-v2","targets":[{"vendor":"bedrock","model":"x"}]}`); w.Code != http.StatusBadRequest {
		t.Errorf("invalid update: %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if w := get("tenant-a", created.ID); decodeModel(t, w).Targets[0].Vendor != "anthropic" {
		t.Error("a rejected update still persisted")
	}
	// Renaming onto the (shadowable) platform name is fine; onto another
	// live tenant row is a 409.
	if w := postModel(t, h, "tenant-a", `{"name":"other","targets":[{"vendor":"anthropic","model":"x"}]}`); w.Code != http.StatusCreated {
		t.Fatal(w.Body.String())
	}
	if w := put("tenant-a", created.ID, `{"name":"other","targets":[{"vendor":"anthropic","model":"x"}]}`); w.Code != http.StatusConflict {
		t.Errorf("rename onto live row: %d, want 409 (%s)", w.Code, w.Body.String())
	}

	// Platform rows are read-only through this API.
	if w := put("tenant-a", platform.ID, `{"name":"haiku","targets":[{"vendor":"anthropic","model":"x"}]}`); w.Code != http.StatusForbidden {
		t.Errorf("update platform row: %d, want 403 (%s)", w.Code, w.Body.String())
	}
	if w := del("tenant-a", platform.ID); w.Code != http.StatusForbidden {
		t.Errorf("delete platform row: %d, want 403 (%s)", w.Code, w.Body.String())
	}
	// Another tenant's row is 404 for both.
	if w := put("tenant-b", created.ID, `{"name":"x","targets":[{"vendor":"anthropic","model":"x"}]}`); w.Code != http.StatusNotFound {
		t.Errorf("update other tenant's row: %d, want 404", w.Code)
	}
	if w := del("tenant-b", created.ID); w.Code != http.StatusNotFound {
		t.Errorf("delete other tenant's row: %d, want 404", w.Code)
	}

	// Delete is soft: gone from Get, name re-usable, second delete 404.
	before := inv.count()
	if w := del("tenant-a", created.ID); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if inv.count() != before+1 {
		t.Error("delete did not invalidate the cache")
	}
	if w := get("tenant-a", created.ID); w.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d, want 404", w.Code)
	}
	if w := del("tenant-a", created.ID); w.Code != http.StatusNotFound {
		t.Errorf("second delete: %d, want 404", w.Code)
	}
	if w := postModel(t, h, "tenant-a", `{"name":"sonnet-v2","targets":[{"vendor":"anthropic","model":"x"}]}`); w.Code != http.StatusCreated {
		t.Errorf("re-create deleted name: %d (%s)", w.Code, w.Body.String())
	}
}

// Without an invalidator wired (an api pod that does not serve the LLM
// plane) writes must still succeed.
func TestModels_NilInvalidatorIsNoop(t *testing.T) {
	deps, _, _ := newModelDeps(t)
	deps.ModelInvalidator = nil
	h := Models{Deps: deps}
	if w := postModel(t, h, "tenant-a", modelCreateBody); w.Code != http.StatusCreated {
		t.Fatalf("create without invalidator: %d %s", w.Code, w.Body.String())
	}
}

// allow_caller_key defaults to false (and is then omitted), round-trips
// when set, and is a plain member of the target object.
func TestModels_AllowCallerKey(t *testing.T) {
	deps, _, _ := newModelDeps(t)
	h := Models{Deps: deps}

	w := postModel(t, h, "tenant-a", `{"name":"byok","targets":[
		{"vendor":"openai_compat","model":"llama","base_url":"https://llm.internal","allow_caller_key":true},
		{"vendor":"anthropic","model":"claude"}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	got := decodeModel(t, w)
	if !got.Targets[0].AllowCallerKey || got.Targets[1].AllowCallerKey {
		t.Errorf("targets = %+v", got.Targets)
	}
	if n := strings.Count(w.Body.String(), `"allow_caller_key"`); n != 1 {
		t.Errorf("allow_caller_key appears %d times, want 1 (omitted when false): %s", n, w.Body.String())
	}
}

// limits on a model: the same shape and validation as an API key's,
// returned on every read ("limits": null when none), replaced by PUT.
func TestModels_Limits(t *testing.T) {
	deps, fs, _ := newModelDeps(t)
	h := Models{Deps: deps}
	const want = `"limits":{"daily_usd":5,"monthly_usd":100.5,"rpm":60,"max_tokens":4096}`

	w := postModel(t, h, "tenant-a", `{"name":"budgeted","targets":[{"vendor":"anthropic","model":"claude-sonnet-4-5"}],
		"limits":{"daily_usd":5,"monthly_usd":100.5,"rpm":60,"max_tokens":4096}}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), want) {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	created := decodeModel(t, w)
	stored, err := fs.Models().Get(context.Background(), "tenant-a", created.ID)
	if err != nil || stored.Limits == nil || stored.Limits.RPM == nil || *stored.Limits.RPM != 60 {
		t.Fatalf("stored limits = %+v, %v", stored, err)
	}

	list := serve(http.MethodGet, "/models", h.List, withPrincipal(httptest.NewRequest(http.MethodGet, "/models", nil), "tenant-a", "agent"))
	if !strings.Contains(list.Body.String(), want) {
		t.Errorf("list lacks the limits: %s", list.Body.String())
	}
	// A row without limits says so explicitly.
	if w := postModel(t, h, "tenant-a", `{"name":"plain","targets":[{"vendor":"anthropic","model":"claude-sonnet-4-5"}]}`); w.Code != http.StatusCreated ||
		!strings.Contains(w.Body.String(), `"limits":null`) {
		t.Errorf("create without limits: %d %s", w.Code, w.Body.String())
	}

	put := func(body string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodPut, "/models/"+created.ID, strings.NewReader(body)), "tenant-a", "admin")
		return serve(http.MethodPut, "/models/{id}", h.Update, req)
	}
	// Replaced wholesale.
	if w := put(`{"name":"budgeted","targets":[{"vendor":"anthropic","model":"claude-sonnet-4-5"}],"limits":{"rpm":5}}`); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"limits":{"rpm":5}`) {
		t.Errorf("update limits: %d %s", w.Code, w.Body.String())
	}
	// An invalid update changes nothing.
	if w := put(`{"name":"budgeted","targets":[{"vendor":"anthropic","model":"claude-sonnet-4-5"}],"limits":{"rpm":-1}}`); w.Code != http.StatusBadRequest {
		t.Errorf("invalid limits update: %d %s", w.Code, w.Body.String())
	}
	if stored, _ := fs.Models().Get(context.Background(), "tenant-a", created.ID); stored.Limits == nil || stored.Limits.RPM == nil || *stored.Limits.RPM != 5 {
		t.Errorf("a rejected update changed the limits: %+v", stored.Limits)
	}
	// Omitted on PUT = cleared (full replace, like price).
	if w := put(`{"name":"budgeted","targets":[{"vendor":"anthropic","model":"claude-sonnet-4-5"}]}`); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"limits":null`) {
		t.Errorf("clear limits: %d %s", w.Code, w.Body.String())
	}
}

func TestModels_InvalidLimits(t *testing.T) {
	for name, limits := range map[string]string{
		"empty":            `{}`,
		"negative daily":   `{"daily_usd":-1}`,
		"negative monthly": `{"monthly_usd":-0.01}`,
		"negative rpm":     `{"rpm":-1}`,
		"negative max":     `{"max_tokens":-5}`,
		"rpm too large":    `{"rpm":100001}`,
		"unknown field":    `{"daily":1}`,
		"fractional rpm":   `{"rpm":1.5}`,
	} {
		t.Run(name, func(t *testing.T) {
			deps, _, inv := newModelDeps(t)
			w := postModel(t, Models{Deps: deps}, "tenant-a", `{"name":"m","targets":[{"vendor":"anthropic","model":"claude-sonnet-4-5"}],"limits":`+limits+`}`)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
			}
			assertErrorType(t, w, "validation_error")
			if inv.count() != 0 {
				t.Error("a rejected create invalidated the registry cache")
			}
		})
	}
}

// A target's label: accepted on an openai_compat target, returned on every
// read, and refused when reserved, malformed or on another vendor.
func TestModels_Label(t *testing.T) {
	deps, fs, _ := newModelDeps(t)
	h := Models{Deps: deps}

	w := postModel(t, h, "tenant-a", `{"name":"fast","targets":[
		{"vendor":"openai_compat","model":"llama-3.3-70b","base_url":"https://api.groq.com/openai/v1","label":"groq","allow_caller_key":true},
		{"vendor":"openai_compat","model":"gpt-4o","base_url":"https://api.openai.com/v1","credential":"anthropic-prod"}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	got := decodeModel(t, w)
	if got.Targets[0].Label != "groq" || got.Targets[1].Label != "" || !strings.Contains(w.Body.String(), `"label":"groq"`) ||
		strings.Count(w.Body.String(), `"label"`) != 1 {
		t.Errorf("view = %s, want label groq on the first target and none on the second", w.Body.String())
	}
	stored, err := fs.Models().Get(context.Background(), "tenant-a", got.ID)
	if err != nil || stored.Targets[0].Label != "groq" {
		t.Fatalf("stored = %+v, %v", stored, err)
	}

	for name, target := range map[string]string{
		"reserved anthropic": `{"vendor":"openai_compat","model":"m","base_url":"https://x.test/v1","label":"anthropic"}`,
		"reserved bedrock":   `{"vendor":"openai_compat","model":"m","base_url":"https://x.test/v1","label":"bedrock"}`,
		"uppercase":          `{"vendor":"openai_compat","model":"m","base_url":"https://x.test/v1","label":"Groq"}`,
		"space":              `{"vendor":"openai_compat","model":"m","base_url":"https://x.test/v1","label":"my vendor"}`,
		"too long":           `{"vendor":"openai_compat","model":"m","base_url":"https://x.test/v1","label":"` + strings.Repeat("a", 33) + `"}`,
		"on anthropic":       `{"vendor":"anthropic","model":"claude-haiku-4-5","label":"groq"}`,
		"on bedrock":         `{"vendor":"bedrock","model":"anthropic.claude","region":"us-east-1","label":"groq"}`,
		"on gemini":          `{"vendor":"gemini","model":"gemini-2.5-pro","label":"groq"}`,
	} {
		w := postModel(t, h, "tenant-a", `{"name":"bad","targets":[`+target+`]}`)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "label") {
			t.Errorf("%s: %d %s, want 400 naming the label", name, w.Code, w.Body.String())
		}
	}
	for _, label := range []string{"openai", "gemini", "deepseek", "z.ai", "my_vendor-2", strings.Repeat("a", 32)} {
		w := postModel(t, h, "tenant-a", `{"name":"ok-`+strings.ReplaceAll(label[:min(len(label), 8)], "_", "-")+`","targets":[{"vendor":"openai_compat","model":"m","base_url":"https://x.test/v1","label":"`+label+`","allow_caller_key":true}]}`)
		if w.Code != http.StatusCreated {
			t.Errorf("label %q: %d %s, want 201", label, w.Code, w.Body.String())
		}
	}
}
