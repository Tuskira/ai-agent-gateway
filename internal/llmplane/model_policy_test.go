package llmplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// memTenants is a TenantLookup over a map: tenant id -> strict policy.
// Lookups are counted so a test can see the policy is cached.
type memTenants struct {
	mu      sync.Mutex
	strict  map[string]bool
	err     error
	lookups atomic.Int32
}

func (m *memTenants) GetByID(_ context.Context, id string) (*store.Tenant, error) {
	m.lookups.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	s, ok := m.strict[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &store.Tenant{ID: id, StrictModels: s}, nil
}

func (m *memTenants) set(id string, strict bool) {
	m.mu.Lock()
	m.strict[id] = strict
	m.mu.Unlock()
}

// policyGateway serves the plane with the fixture's model store, a
// registry that reads tenant policy from tenants, and an agent Principal
// of the fixture's tenant carrying the key override keyStrict.
func (f *registryFixture) policyGateway(t *testing.T, cfg Config, tenants *memTenants, keyStrict *bool) *httptest.Server {
	t.Helper()
	cfg.Registry = NewRegistry(f.models, f.secrets, RegistryOptions{Tenants: tenants})
	cfg.Authorizer = pkgauth.NewRoleAuthorizer()
	cfg.MaxRequestBytes = 1 << 20
	h, err := Handler(cfg, f.rec)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(withPrincipal(h, &pkgauth.Principal{
		TenantID: f.tenant, Roles: []string{"agent"}, KeyID: "key-1", StrictModels: keyStrict,
	}))
	t.Cleanup(gw.Close)
	return gw
}

func boolp(b bool) *bool { return &b }

// assertRefused checks a 403 in the Anthropic envelope, the refusal row
// (status 403, the capture error, no target tried) and that nothing was
// sent upstream.
func assertRefused(t *testing.T, f *registryFixture, resp *http.Response, body, wantErr, wantModel string, upstreams ...*anthropicUpstream) {
	t.Helper()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d body %s, want 403", resp.StatusCode, body)
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Type != "error" || env.Error.Type != "permission_error" {
		t.Errorf("body %s, want an Anthropic permission_error envelope", body)
	}
	for i, u := range upstreams {
		if n := u.hits.Load(); n != 0 {
			t.Errorf("upstream %d was called %d times; a refusal must not reach any upstream", i, n)
		}
	}
	c := f.rec.wait(t)
	if c.StatusCode != http.StatusForbidden || c.Error != wantErr || c.FallbackIndex != -1 || c.RequestedModel != wantModel {
		t.Errorf("refusal row = status %d error %q fallback %d requested %q; want 403 %q -1 %q",
			c.StatusCode, c.Error, c.FallbackIndex, c.RequestedModel, wantErr, wantModel)
	}
}

// A registry row that exists but is disabled is refused with 403, logged as
// a refusal, and never forwarded -- not to its targets, and not to the
// passthrough upstream (it used to fall through to the passthrough).
func TestModelPolicy_DisabledRowIsRefused(t *testing.T) {
	f := newRegistryFixture(t)
	passthrough := newAnthropicUpstream(t, http.StatusOK)
	target := newAnthropicUpstream(t, http.StatusOK)
	if err := f.models.Create(context.Background(), &store.Model{TenantID: f.tenant, Name: "sonnet", Enabled: false,
		Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "x", BaseURL: target.URL}}}); err != nil {
		t.Fatal(err)
	}
	// Non-strict tenant: a disabled row is refused whatever the policy.
	gw := f.policyGateway(t, Config{UpstreamBaseURL: passthrough.URL}, &memTenants{strict: map[string]bool{f.tenant: false}}, nil)
	resp, body := do(t, "POST", gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk-client"}, aliasBody)
	assertRefused(t, f, resp, body, "model_disabled", "sonnet", passthrough, target)
	if !strings.Contains(body, "sonnet is disabled") {
		t.Errorf("message = %s, want it to say the model is disabled", body)
	}
}

// The refusal is written in the client's own dialect: an OpenAI client gets
// the OpenAI error envelope.
func TestModelPolicy_DisabledRowRefusedInClientDialect(t *testing.T) {
	f := newRegistryFixture(t)
	target := newAnthropicUpstream(t, http.StatusOK)
	if err := f.models.Create(context.Background(), &store.Model{TenantID: f.tenant, Name: "fast", Enabled: false,
		Targets: []store.ModelTarget{{Vendor: "openai_compat", Model: "llama", BaseURL: target.URL}}}); err != nil {
		t.Fatal(err)
	}
	gw := f.policyGateway(t, Config{UpstreamBaseURL: "https://unused.invalid", OpenAIEnabled: true, OpenAIBaseURL: target.URL},
		&memTenants{strict: map[string]bool{}}, nil)
	resp, body := do(t, "POST", gw.URL+"/openai/v1/chat/completions", map[string]string{"Authorization": "Bearer sk-client"},
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d body %s, want 403", resp.StatusCode, body)
	}
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Error.Type != "permission_error" || !strings.Contains(env.Error.Message, "disabled") {
		t.Errorf("body %s, want the OpenAI error envelope", body)
	}
	if target.hits.Load() != 0 {
		t.Error("the disabled row's target was called")
	}
	if c := f.rec.wait(t); c.StatusCode != http.StatusForbidden || c.Error != "model_disabled" || c.Provider != "openai" {
		t.Errorf("refusal row = %+v", c)
	}
}

const unknownModelBody = `{"model":"claude-unregistered-1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

// The effective policy: the key's override when it sets one, else the
// tenant's. Strict refuses a model with no registry row; non-strict
// forwards it byte-for-byte (today's passthrough).
func TestModelPolicy_StrictUnregisteredModel(t *testing.T) {
	cases := []struct {
		name         string
		tenantStrict bool
		keyStrict    *bool
		refused      bool
	}{
		{"strict tenant, key inherits", true, nil, true},
		{"non-strict tenant, key overrides true", false, boolp(true), true},
		{"strict tenant, key overrides false", true, boolp(false), false},
		{"non-strict tenant, key inherits", false, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRegistryFixture(t)
			up := newAnthropicUpstream(t, http.StatusOK)
			gw := f.policyGateway(t, Config{UpstreamBaseURL: up.URL}, &memTenants{strict: map[string]bool{f.tenant: tc.tenantStrict}}, tc.keyStrict)
			resp, body := do(t, "POST", gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk-client"}, unknownModelBody)
			if tc.refused {
				assertRefused(t, f, resp, body, "model_not_registered", "claude-unregistered-1", up)
				if !strings.Contains(body, "not registered") {
					t.Errorf("message = %s, want it to say the model is not registered", body)
				}
				return
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d body %s, want the passthrough's 200", resp.StatusCode, body)
			}
			got, hdr, path := up.got()
			if string(got) != unknownModelBody || path != "/v1/messages" || hdr.Get("x-api-key") != "sk-client" {
				t.Errorf("passthrough changed the request: path %q key %q body %s", path, hdr.Get("x-api-key"), got)
			}
			if c := f.rec.wait(t); c.StatusCode != http.StatusOK || c.Error != "" || c.ResolvedModel != "" {
				t.Errorf("capture = %+v", c)
			}
		})
	}
}

// Strict does not touch an enabled registered model: it is routed to its
// target exactly as before.
func TestModelPolicy_StrictEnabledRegisteredModelUnchanged(t *testing.T) {
	f := newRegistryFixture(t)
	passthrough := newAnthropicUpstream(t, http.StatusOK)
	target := newAnthropicUpstream(t, http.StatusOK)
	f.credential(t, "anthropic-prod", map[string]string{"api_key": "sk-ant-registry"})
	f.model(t, "sonnet", nil, store.ModelTarget{Vendor: "anthropic", Model: "claude-sonnet-4-5", BaseURL: target.URL, Credential: "anthropic-prod"})
	gw := f.policyGateway(t, Config{UpstreamBaseURL: passthrough.URL}, &memTenants{strict: map[string]bool{f.tenant: true}}, nil)
	resp, body := do(t, "POST", gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk-client"}, aliasBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if target.hits.Load() != 1 || passthrough.hits.Load() != 0 {
		t.Errorf("target hits %d, passthrough hits %d; want the registered target only", target.hits.Load(), passthrough.hits.Load())
	}
	if _, hdr, _ := target.got(); hdr.Get("x-api-key") != "sk-ant-registry" {
		t.Errorf("target key = %q", hdr.Get("x-api-key"))
	}
	if c := f.rec.wait(t); c.StatusCode != http.StatusOK || c.ResolvedModel != "claude-sonnet-4-5" {
		t.Errorf("capture = %+v", c)
	}
}

// Under strict, a write that names no model (a batch create) is refused,
// since it can reach models the registry never sees; a read (a batch's
// status) still passes through.
func TestModelPolicy_StrictRequestWithoutModel(t *testing.T) {
	f := newRegistryFixture(t)
	up := newAnthropicUpstream(t, http.StatusOK)
	gw := f.policyGateway(t, Config{UpstreamBaseURL: up.URL}, &memTenants{strict: map[string]bool{f.tenant: true}}, nil)

	resp, body := do(t, "POST", gw.URL+"/v1/messages/batches", map[string]string{"x-api-key": "sk-client"},
		`{"requests":[{"custom_id":"a","params":{"model":"claude-unregistered-1","max_tokens":8,"messages":[]}}]}`)
	assertRefused(t, f, resp, body, "model_not_registered", "", up)

	resp, body = do(t, "GET", gw.URL+"/v1/messages/batches/msgbatch_1", map[string]string{"x-api-key": "sk-client"}, "")
	if resp.StatusCode != http.StatusOK || up.hits.Load() != 1 {
		t.Errorf("GET a batch under strict: status %d (upstream hits %d) body %s, want it passed through", resp.StatusCode, up.hits.Load(), body)
	}
}

// A failing tenant lookup is a 502, never a guess at the policy (and never
// a passthrough).
func TestModelPolicy_TenantLookupFailureIs502(t *testing.T) {
	f := newRegistryFixture(t)
	up := newAnthropicUpstream(t, http.StatusOK)
	tenants := &memTenants{strict: map[string]bool{}, err: errors.New("db down")}
	gw := f.policyGateway(t, Config{UpstreamBaseURL: up.URL}, tenants, nil)
	resp, body := do(t, "POST", gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk-client"}, unknownModelBody)
	if resp.StatusCode != http.StatusBadGateway || up.hits.Load() != 0 {
		t.Errorf("status %d (upstream hits %d) body %s, want 502 and no upstream call", resp.StatusCode, up.hits.Load(), body)
	}
	if c := f.rec.wait(t); c.StatusCode != http.StatusBadGateway || !strings.HasPrefix(c.Error, "registry_error") {
		t.Errorf("capture = %+v", c)
	}
}

// The tenant's policy is cached (one lookup per TTL, not one per call),
// and Invalidate drops it so a policy change applies at once on this
// replica.
func TestRegistry_TenantStrictCachedAndInvalidated(t *testing.T) {
	tenants := &memTenants{strict: map[string]bool{"t": false}}
	r := NewRegistry(nil, nil, RegistryOptions{Tenants: tenants})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if s, err := r.TenantStrict(ctx, "t"); err != nil || s {
			t.Fatalf("TenantStrict = %v, %v; want false", s, err)
		}
	}
	if n := tenants.lookups.Load(); n != 1 {
		t.Errorf("lookups = %d, want 1 (cached)", n)
	}
	tenants.set("t", true)
	r.Invalidate("t")
	if s, _ := r.TenantStrict(ctx, "t"); !s {
		t.Error("after Invalidate the new policy was not read")
	}
	if s, err := r.TenantStrict(ctx, "unknown"); err != nil || s {
		t.Errorf("unknown tenant = %v, %v; want non-strict", s, err)
	}
	var nilReg *Registry
	if s, err := nilReg.TenantStrict(ctx, "t"); err != nil || s {
		t.Errorf("nil registry = %v, %v; want non-strict", s, err)
	}
}
