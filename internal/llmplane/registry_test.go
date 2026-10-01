package llmplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// memCredentials is the smallest store.CredentialStore that lets a REAL
// secrets.Service (AES-256-GCM, key ring, field parsing) run in a unit
// test without Postgres: it keeps the ciphertext rows in a map. Only the
// storage is in memory; encryption, decryption and field lookup are the
// production code paths.
type memCredentials struct {
	mu   sync.Mutex
	rows map[string]*store.Credential // tenant|name
}

func (m *memCredentials) Create(_ context.Context, c *store.Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]*store.Credential{}
	}
	key := c.TenantID + "|" + c.Name
	if _, ok := m.rows[key]; ok {
		return store.ErrConflict
	}
	c.ID = key
	m.rows[key] = c
	return nil
}

func (m *memCredentials) Get(_ context.Context, tenantID, name string) (*store.Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.rows[tenantID+"|"+name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return c, nil
}

func (m *memCredentials) List(context.Context, string) ([]*store.Credential, error) { return nil, nil }
func (m *memCredentials) Rotate(context.Context, string, string, []byte, []byte, string, []string) error {
	return nil
}
func (m *memCredentials) Delete(context.Context, string, string) error { return nil }

// registryFixture is a registry over an in-memory model store and a real
// secrets.Service, plus a recording capture sink.
type registryFixture struct {
	models   store.ModelStore
	secrets  *secrets.Service
	registry *Registry
	rec      *lastRec
	tenant   string
}

func newRegistryFixture(t *testing.T) *registryFixture {
	t.Helper()
	st := dptest.New()
	ring := &secrets.KeyRing{Keys: map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)}, ActiveKeyID: "k1"}
	svc := secrets.NewService(&memCredentials{}, ring)
	return &registryFixture{
		models:   st.Models(),
		secrets:  svc,
		registry: NewRegistry(st.Models(), svc, RegistryOptions{}),
		rec:      &lastRec{},
		tenant:   "t",
	}
}

func (f *registryFixture) credential(t *testing.T, name string, fields map[string]string) {
	t.Helper()
	if _, err := f.secrets.Create(context.Background(), f.tenant, name, "api_key", fields, "test"); err != nil {
		t.Fatal(err)
	}
}

func (f *registryFixture) model(t *testing.T, name string, price *store.ModelPrice, targets ...store.ModelTarget) {
	t.Helper()
	if err := f.models.Create(context.Background(), &store.Model{TenantID: f.tenant, Name: name, Enabled: true, Targets: targets, Price: price}); err != nil {
		t.Fatal(err)
	}
}

// gateway serves the plane with this fixture's registry, the way
// cmd/gateway does (Config.Registry), behind an agent Principal of the
// fixture's tenant.
func (f *registryFixture) gateway(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	cfg.Registry = f.registry
	if cfg.Authorizer == nil {
		cfg.Authorizer = pkgauth.NewRoleAuthorizer()
	}
	if cfg.MaxRequestBytes == 0 {
		cfg.MaxRequestBytes = 1 << 20
	}
	h, err := Handler(cfg, f.rec)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(withPrincipal(h, &pkgauth.Principal{TenantID: f.tenant, Roles: []string{"agent"}}))
	t.Cleanup(gw.Close)
	return gw
}

// anthropicUpstream is an httptest server speaking Anthropic's Messages
// wire format: it records what it received and answers a fixed message.
type anthropicUpstream struct {
	*httptest.Server
	hits   atomic.Int32
	mu     sync.Mutex
	body   []byte
	header http.Header
	path   string
}

func newAnthropicUpstream(t *testing.T, status int) *anthropicUpstream {
	t.Helper()
	u := &anthropicUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.body, u.header, u.path = b, r.Header.Clone(), r.URL.RequestURI()
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req_up")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":10}}`)
		} else {
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *anthropicUpstream) got() ([]byte, http.Header, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.body, u.header, u.path
}

const aliasBody = `{"model":"sonnet","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`

// A registered Anthropic alias swaps the host, the credential and the model
// id; the rest of the body is the client's own bytes. The row's price
// overrides the rate card, and capture records requested vs resolved.
func TestRegistry_AnthropicAliasRewritesHostCredentialAndModel(t *testing.T) {
	f := newRegistryFixture(t)
	target := newAnthropicUpstream(t, http.StatusOK)
	passthrough := newAnthropicUpstream(t, http.StatusOK)
	f.credential(t, "anthropic-prod", map[string]string{"api_key": "sk-ant-registry"})
	f.model(t, "sonnet", &store.ModelPrice{Input: 2, Output: 10},
		store.ModelTarget{Vendor: "anthropic", Model: "claude-sonnet-4-5", BaseURL: target.URL, Credential: "anthropic-prod"})

	gw := f.gateway(t, Config{UpstreamBaseURL: passthrough.URL})
	resp, body := do(t, "POST", gw.URL+"/v1/messages?beta=true", map[string]string{"x-api-key": "sk-client", "anthropic-version": "2023-06-01"}, aliasBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if passthrough.hits.Load() != 0 {
		t.Error("the configured upstream was hit; the alias should have gone to its target")
	}
	got, hdr, path := target.got()
	if path != "/v1/messages?beta=true" {
		t.Errorf("target path = %q", path)
	}
	var up map[string]json.RawMessage
	if err := json.Unmarshal(got, &up); err != nil {
		t.Fatal(err)
	}
	if string(up["model"]) != `"claude-sonnet-4-5"` {
		t.Errorf("model = %s, want the target's id", up["model"])
	}
	var orig map[string]json.RawMessage
	_ = json.Unmarshal([]byte(aliasBody), &orig)
	if !bytes.Equal(up["messages"], orig["messages"]) || !bytes.Equal(up["max_tokens"], orig["max_tokens"]) {
		t.Errorf("non-model members were altered:\n got %s\nwant %s", up["messages"], orig["messages"])
	}
	if hdr.Get("x-api-key") != "sk-ant-registry" {
		t.Errorf("target x-api-key = %q, want the registry credential", hdr.Get("x-api-key"))
	}
	if hdr.Get("anthropic-version") != "2023-06-01" {
		t.Error("client headers other than the credential must ride along")
	}

	c := f.rec.wait(t)
	if c.RequestedModel != "sonnet" || c.ResolvedModel != "claude-sonnet-4-5" || c.ResolvedVendor != "anthropic" || c.Model != "claude-sonnet-4-5" || c.FallbackIndex != 0 || c.Translated {
		t.Errorf("capture = requested %q resolved %s/%q model %q fallback %d translated %v", c.RequestedModel, c.ResolvedVendor, c.ResolvedModel, c.Model, c.FallbackIndex, c.Translated)
	}
	if c.Provider != "anthropic" || !strings.Contains(target.URL, c.UpstreamHost) {
		t.Errorf("provider/upstream = %q/%q", c.Provider, c.UpstreamHost)
	}
	// 100 in * $2/M + 10 out * $10/M = $0.0003
	if c.CostUSD == nil || !near(*c.CostUSD, 0.0003) {
		t.Errorf("cost = %v, want the registry price 0.0003", c.CostUSD)
	}
	if c.Headers["X-Api-Key"] != "[masked]" {
		t.Errorf("captured x-api-key = %q", c.Headers["X-Api-Key"])
	}
	if strings.Contains(string(c.RequestBody), "claude-sonnet-4-5") && c.RequestBody != nil {
		t.Error("capture must keep the client's body, not the rewritten one")
	}
}

// An unregistered name is the byte-for-byte passthrough: same body, same
// headers (BYOK rides along), requested_model set, nothing resolved.
func TestRegistry_UnregisteredIsByteIdentical(t *testing.T) {
	f := newRegistryFixture(t)
	up := newAnthropicUpstream(t, http.StatusOK)
	f.model(t, "sonnet", nil, store.ModelTarget{Vendor: "anthropic", Model: "x", BaseURL: "https://other.example.com"})

	gw := f.gateway(t, Config{UpstreamBaseURL: up.URL})
	body := `{"model":"claude-haiku-4-5",   "max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	resp, _ := do(t, "POST", gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk-client"}, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got, hdr, _ := up.got()
	if string(got) != body {
		t.Errorf("upstream body differs from the client's:\n got %s\nwant %s", got, body)
	}
	if hdr.Get("x-api-key") != "sk-client" {
		t.Errorf("BYOK key = %q, want the client's", hdr.Get("x-api-key"))
	}
	c := f.rec.wait(t)
	if c.RequestedModel != "claude-haiku-4-5" || c.ResolvedModel != "" || c.ResolvedVendor != "" || c.Model != "claude-haiku-4-5" || c.FallbackIndex != 0 {
		t.Errorf("capture = %+v", c)
	}
}

// A disabled row behaves as unregistered.
func TestRegistry_DisabledRowIsPassthrough(t *testing.T) {
	f := newRegistryFixture(t)
	up := newAnthropicUpstream(t, http.StatusOK)
	other := newAnthropicUpstream(t, http.StatusOK)
	if err := f.models.Create(context.Background(), &store.Model{TenantID: f.tenant, Name: "sonnet", Enabled: false,
		Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "x", BaseURL: other.URL}}}); err != nil {
		t.Fatal(err)
	}
	gw := f.gateway(t, Config{UpstreamBaseURL: up.URL})
	do(t, "POST", gw.URL+"/v1/messages", nil, aliasBody)
	if up.hits.Load() != 1 || other.hits.Load() != 0 {
		t.Errorf("disabled row routed: upstream %d, target %d", up.hits.Load(), other.hits.Load())
	}
}

// Whose key reaches the vendor. A target with a credential gets exactly
// that credential. A target WITHOUT one gets the caller's own key only
// when it is on the gateway's configured default host for the vendor, or
// when it opted in with allow_caller_key; otherwise it is unusable -- it
// is never called, and with no usable target the call is refused.
func TestRegistry_WhoseKeyReachesTheVendor(t *testing.T) {
	f := newRegistryFixture(t)
	def := newAnthropicUpstream(t, http.StatusOK)    // the gateway's configured upstream
	custom := newAnthropicUpstream(t, http.StatusOK) // some other host
	optedIn := newAnthropicUpstream(t, http.StatusOK)
	withCred := newAnthropicUpstream(t, http.StatusOK)
	f.credential(t, "anthropic-prod", map[string]string{"api_key": "sk-ant-registry"})

	f.model(t, "default-host", nil, store.ModelTarget{Vendor: "anthropic", Model: "m", BaseURL: def.URL})
	f.model(t, "no-base-url", nil, store.ModelTarget{Vendor: "anthropic", Model: "m"})
	f.model(t, "custom-host", nil, store.ModelTarget{Vendor: "anthropic", Model: "m", BaseURL: custom.URL})
	f.model(t, "custom-opted-in", nil, store.ModelTarget{Vendor: "anthropic", Model: "m", BaseURL: optedIn.URL, AllowCallerKey: true})
	f.model(t, "with-cred", nil, store.ModelTarget{Vendor: "anthropic", Model: "m", BaseURL: withCred.URL, Credential: "anthropic-prod"})
	f.model(t, "skips-unusable", nil,
		store.ModelTarget{Vendor: "anthropic", Model: "first", BaseURL: custom.URL},
		store.ModelTarget{Vendor: "anthropic", Model: "second", BaseURL: withCred.URL, Credential: "anthropic-prod"})

	gw := f.gateway(t, Config{UpstreamBaseURL: def.URL})
	hdrs := map[string]string{"x-api-key": "sk-client", "x-goog-api-key": "AIza-other-vendor", "api-key": "azure-other-vendor"}
	call := func(model string) (*http.Response, string) {
		f.rec.last = nil
		return do(t, "POST", gw.URL+"/v1/messages", hdrs, strings.Replace(aliasBody, "sonnet", model, 1))
	}

	t.Run("default host: caller's key forwarded", func(t *testing.T) {
		for _, model := range []string{"default-host", "no-base-url"} {
			before := def.hits.Load()
			if resp, body := call(model); resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: status %d body %s", model, resp.StatusCode, body)
			}
			_, h, _ := def.got()
			if def.hits.Load() != before+1 || h.Get("x-api-key") != "sk-client" {
				t.Errorf("%s: hits %d->%d x-api-key=%q; want the caller's key at the default host", model, before, def.hits.Load(), h.Get("x-api-key"))
			}
			if h.Get("x-goog-api-key") != "" || h.Get("api-key") != "" {
				t.Errorf("%s: another vendor's key was forwarded: x-goog-api-key=%q api-key=%q", model, h.Get("x-goog-api-key"), h.Get("api-key"))
			}
		}
	})

	t.Run("custom host without the flag: never called, 400", func(t *testing.T) {
		resp, body := call("custom-host")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d body %s", resp.StatusCode, body)
		}
		var env struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil || env.Type != "error" || env.Error.Type != "invalid_request_error" ||
			env.Error.Message != "model custom-host: no credential for target 0 (set a credential or allow_caller_key)" {
			t.Errorf("envelope = %s", body)
		}
		if custom.hits.Load() != 0 {
			t.Errorf("the custom host was called %d times; it must never see the request", custom.hits.Load())
		}
		if c := f.rec.wait(t); c.StatusCode != 400 || c.Error != "no_credential" || c.FallbackIndex != -1 || c.RequestedModel != "custom-host" {
			t.Errorf("capture = status %d error %q fallback %d requested %q", c.StatusCode, c.Error, c.FallbackIndex, c.RequestedModel)
		}
	})

	t.Run("custom host with allow_caller_key: caller's key forwarded", func(t *testing.T) {
		if resp, body := call("custom-opted-in"); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d body %s", resp.StatusCode, body)
		}
		_, h, _ := optedIn.got()
		if h.Get("x-api-key") != "sk-client" {
			t.Errorf("x-api-key = %q, want the caller's", h.Get("x-api-key"))
		}
		if h.Get("x-goog-api-key") != "" || h.Get("api-key") != "" {
			t.Errorf("another vendor's key was forwarded: %v", h)
		}
	})

	t.Run("credential wins and replaces the caller's headers", func(t *testing.T) {
		f.rec.last = nil
		resp, body := do(t, "POST", gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk-client", "Authorization": "Bearer sk-oauth"},
			strings.Replace(aliasBody, "sonnet", "with-cred", 1))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d body %s", resp.StatusCode, body)
		}
		if _, h, _ := withCred.got(); h.Get("x-api-key") != "sk-ant-registry" || h.Get("Authorization") != "" {
			t.Errorf("x-api-key=%q authorization=%q; want only the registry credential", h.Get("x-api-key"), h.Get("Authorization"))
		}
	})

	t.Run("an unusable target is skipped for the next", func(t *testing.T) {
		before := withCred.hits.Load()
		if resp, body := call("skips-unusable"); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d body %s", resp.StatusCode, body)
		}
		if custom.hits.Load() != 0 || withCred.hits.Load() != before+1 {
			t.Errorf("hits custom=%d withCred=%d->%d", custom.hits.Load(), before, withCred.hits.Load())
		}
		if c := f.rec.wait(t); c.FallbackIndex != 1 || c.ResolvedModel != "second" {
			t.Errorf("capture fallback=%d resolved=%q", c.FallbackIndex, c.ResolvedModel)
		}
	})

	t.Run("the gateway key is never forwarded", func(t *testing.T) {
		f.rec.last = nil
		do(t, "POST", gw.URL+"/v1/messages", map[string]string{"Authorization": "Bearer gk_gatewaykey", "x-api-key": "gk_repeated"},
			strings.Replace(aliasBody, "sonnet", "custom-opted-in", 1))
		if _, h, _ := optedIn.got(); h.Get("Authorization") != "" || h.Get("x-api-key") != "" {
			t.Errorf("gateway key leaked upstream: authorization=%q x-api-key=%q", h.Get("Authorization"), h.Get("x-api-key"))
		}
	})

	t.Run("a credential without a usable field is a 400 that never shows the value", func(t *testing.T) {
		f.credential(t, "odd", map[string]string{"user": "u", "pass": "p-secret"})
		f.model(t, "odd-cred", nil, store.ModelTarget{Vendor: "anthropic", Model: "m", BaseURL: custom.URL, Credential: "odd"})
		resp, body := call("odd-cred")
		if resp.StatusCode != http.StatusBadRequest || strings.Contains(body, "p-secret") || !strings.Contains(body, "api_key") {
			t.Errorf("status %d body %s", resp.StatusCode, body)
		}
		if custom.hits.Load() != 0 {
			t.Error("the target was called without a usable credential")
		}
	})
}

// The refusal is written in the CLIENT dialect's envelope: an OpenAI
// client gets OpenAI's error shape.
func TestRegistry_NoCredentialRefusalUsesClientDialectEnvelope(t *testing.T) {
	f := newRegistryFixture(t)
	var hits atomic.Int32
	custom := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(custom.Close)
	f.model(t, "fast", nil, store.ModelTarget{Vendor: "openai_compat", Model: "llama", BaseURL: custom.URL})

	gw := f.gateway(t, Config{UpstreamBaseURL: "https://unused.invalid", OpenAIEnabled: true, OpenAIBaseURL: "https://api.openai.com"})
	resp, body := do(t, "POST", gw.URL+"/openai/v1/chat/completions", map[string]string{"Authorization": "Bearer sk-client"},
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || resp.StatusCode != http.StatusBadRequest || env.Type != "" ||
		env.Error.Type != "invalid_request_error" || env.Error.Message != "model fast: no credential for target 0 (set a credential or allow_caller_key)" {
		t.Errorf("status %d body %s", resp.StatusCode, body)
	}
	if hits.Load() != 0 {
		t.Error("the custom host was called")
	}
}

// setCredential itself never lets a caller's key through when it is not
// allowed, even if a caller of it forgot to check (defence in depth), and
// never another vendor's key in any branch.
func TestSetCredential(t *testing.T) {
	mk := func() *http.Request {
		r := httptest.NewRequest("POST", "https://target.example.com/v1/messages", nil)
		r.Header.Set("X-Api-Key", "sk-client")
		r.Header.Set("Authorization", "Bearer sk-oauth")
		r.Header.Set("X-Goog-Api-Key", "AIza")
		r.Header.Set("Api-Key", "azure")
		r.Header.Set("Anthropic-Version", "2023-06-01")
		return r
	}
	id := func(k string) string { return k }

	r := mk()
	if err := setCredential(r, "anthropic", nil, false, "X-Api-Key", id); err != nil {
		t.Fatal(err)
	}
	for _, h := range credentialHeaders {
		if r.Header.Get(h) != "" {
			t.Errorf("not allowed: %s = %q still present", h, r.Header.Get(h))
		}
	}
	if r.Header.Get("Anthropic-Version") == "" {
		t.Error("non-credential headers must be kept")
	}

	r = mk()
	_ = setCredential(r, "anthropic", nil, true, "X-Api-Key", id)
	if r.Header.Get("X-Api-Key") != "sk-client" || r.Header.Get("Authorization") != "Bearer sk-oauth" || r.Header.Get("X-Goog-Api-Key") != "" || r.Header.Get("Api-Key") != "" {
		t.Errorf("allowed (anthropic): %v", r.Header)
	}

	r = mk()
	_ = setCredential(r, "openai", nil, true, "Authorization", id)
	if r.Header.Get("Authorization") != "Bearer sk-oauth" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Goog-Api-Key") != "" {
		t.Errorf("allowed (openai): %v", r.Header)
	}

	r = mk()
	_ = setCredential(r, "openai", map[string]string{"api_key": "sk-registry"}, true, "Authorization", func(k string) string { return "Bearer " + k })
	if r.Header.Get("Authorization") != "Bearer sk-registry" || r.Header.Get("X-Api-Key") != "" {
		t.Errorf("credential: %v", r.Header)
	}
}

// Fallback: a 429 from the first target moves on to the second before
// anything reached the client; the second's answer is relayed and
// fallback_index names it.
func TestRegistry_FallbackOn429(t *testing.T) {
	f := newRegistryFixture(t)
	first := newAnthropicUpstream(t, http.StatusTooManyRequests)
	second := newAnthropicUpstream(t, http.StatusOK)
	f.model(t, "sonnet", nil,
		store.ModelTarget{Vendor: "anthropic", Model: "primary", BaseURL: first.URL, AllowCallerKey: true},
		store.ModelTarget{Vendor: "anthropic", Model: "backup", BaseURL: second.URL, AllowCallerKey: true})

	gw := f.gateway(t, Config{UpstreamBaseURL: "https://unused.invalid"})
	resp, body := do(t, "POST", gw.URL+"/v1/messages", nil, aliasBody)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_1") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if first.hits.Load() != 1 || second.hits.Load() != 1 {
		t.Errorf("hits first=%d second=%d, want 1/1", first.hits.Load(), second.hits.Load())
	}
	if got, _, _ := second.got(); !strings.Contains(string(got), `"model":"backup"`) {
		t.Errorf("second target got model %s", got)
	}
	c := f.rec.wait(t)
	if c.FallbackIndex != 1 || c.ResolvedModel != "backup" || c.StatusCode != 200 {
		t.Errorf("capture fallback=%d resolved=%q status=%d", c.FallbackIndex, c.ResolvedModel, c.StatusCode)
	}
}

// A dial failure (target down) also falls through; when every target
// fails the LAST outcome is what the client gets: a relayed 429 from a
// last target that answered, or a 502 envelope when it was unreachable.
func TestRegistry_FallbackOnDialErrorAndLastErrorWins(t *testing.T) {
	f := newRegistryFixture(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	throttled := newAnthropicUpstream(t, http.StatusTooManyRequests)
	ok := newAnthropicUpstream(t, http.StatusOK)
	f.model(t, "recovers", nil,
		store.ModelTarget{Vendor: "anthropic", Model: "a", BaseURL: deadURL, AllowCallerKey: true},
		store.ModelTarget{Vendor: "anthropic", Model: "b", BaseURL: ok.URL, AllowCallerKey: true})
	f.model(t, "all-throttled", nil,
		store.ModelTarget{Vendor: "anthropic", Model: "a", BaseURL: deadURL, AllowCallerKey: true},
		store.ModelTarget{Vendor: "anthropic", Model: "b", BaseURL: throttled.URL, AllowCallerKey: true})
	f.model(t, "all-dead", nil,
		store.ModelTarget{Vendor: "anthropic", Model: "a", BaseURL: throttled.URL, AllowCallerKey: true},
		store.ModelTarget{Vendor: "anthropic", Model: "b", BaseURL: deadURL, AllowCallerKey: true})

	gw := f.gateway(t, Config{UpstreamBaseURL: "https://unused.invalid"})

	resp, _ := do(t, "POST", gw.URL+"/v1/messages", nil, strings.Replace(aliasBody, "sonnet", "recovers", 1))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("dead-then-ok: status %d, want 200", resp.StatusCode)
	}
	if c := f.rec.wait(t); c.FallbackIndex != 1 {
		t.Errorf("dead-then-ok: fallback_index %d, want 1", c.FallbackIndex)
	}

	f.rec.last = nil
	resp, body := do(t, "POST", gw.URL+"/v1/messages", nil, strings.Replace(aliasBody, "sonnet", "all-throttled", 1))
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "slow down") {
		t.Errorf("dead-then-429: status %d body %s; want the last target's 429 relayed", resp.StatusCode, body)
	}
	if c := f.rec.wait(t); c.FallbackIndex != 1 || c.StatusCode != 429 {
		t.Errorf("dead-then-429: capture fallback=%d status=%d", c.FallbackIndex, c.StatusCode)
	}

	f.rec.last = nil
	resp, body = do(t, "POST", gw.URL+"/v1/messages", nil, strings.Replace(aliasBody, "sonnet", "all-dead", 1))
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, `"type":"api_error"`) {
		t.Errorf("429-then-dead: status %d body %s; want 502 in the Anthropic envelope", resp.StatusCode, body)
	}
	if c := f.rec.wait(t); c.FallbackIndex != 1 || c.StatusCode != 502 || !strings.HasPrefix(c.Error, "upstream_error:") {
		t.Errorf("429-then-dead: capture fallback=%d status=%d error=%q", c.FallbackIndex, c.StatusCode, c.Error)
	}
}

// Once the first target's headers have been relayed there is no fallback,
// even if its body then fails: the second target is never called.
func TestRegistry_NoFallbackAfterFirstByte(t *testing.T) {
	f := newRegistryFixture(t)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // drop the connection mid-stream
	}))
	t.Cleanup(broken.Close)
	second := newAnthropicUpstream(t, http.StatusOK)
	f.model(t, "sonnet", nil,
		store.ModelTarget{Vendor: "anthropic", Model: "a", BaseURL: broken.URL, AllowCallerKey: true},
		store.ModelTarget{Vendor: "anthropic", Model: "b", BaseURL: second.URL, AllowCallerKey: true})

	gw := f.gateway(t, Config{UpstreamBaseURL: "https://unused.invalid"})
	resp, body := do(t, "POST", gw.URL+"/v1/messages", nil, strings.Replace(aliasBody, `"max_tokens"`, `"stream":true,"max_tokens"`, 1))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "message_start") {
		t.Errorf("status %d body %q; want the partial stream relayed", resp.StatusCode, body)
	}
	if second.hits.Load() != 0 {
		t.Error("second target was tried after bytes had reached the client")
	}
	if c := f.rec.wait(t); c.FallbackIndex != 0 || !strings.HasPrefix(c.Error, "upstream_error") {
		t.Errorf("capture fallback=%d error=%q", c.FallbackIndex, c.Error)
	}
}

// A registered model none of whose targets can be reached from the client's
// dialect -- natively or through a pkg/llm adapter pair -- is the "requires
// translation" 400. (Anthropic -> openai_compat IS translated: see
// translated_registry_test.go.)
func TestRegistry_RequiresTranslation400(t *testing.T) {
	f := newRegistryFixture(t)
	up := newAnthropicUpstream(t, http.StatusOK)
	f.model(t, "gpt", nil, store.ModelTarget{Vendor: "gemini", Model: "gemini-2.5-pro"})
	f.model(t, "mixed", nil,
		store.ModelTarget{Vendor: "gemini", Model: "gemini-2.5-pro"},
		store.ModelTarget{Vendor: "anthropic", Model: "claude", BaseURL: up.URL, AllowCallerKey: true})

	gw := f.gateway(t, Config{UpstreamBaseURL: "https://unused.invalid"})
	resp, body := do(t, "POST", gw.URL+"/v1/messages", nil, strings.Replace(aliasBody, "sonnet", "gpt", 1))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Type != "error" || env.Error.Type != "invalid_request_error" ||
		env.Error.Message != "model gpt requires translation (not available)" {
		t.Errorf("envelope = %s", body)
	}
	if c := f.rec.wait(t); c.StatusCode != 400 || c.Error != "requires_translation" || c.FallbackIndex != -1 || c.RequestedModel != "gpt" {
		t.Errorf("capture = status %d error %q fallback %d requested %q", c.StatusCode, c.Error, c.FallbackIndex, c.RequestedModel)
	}

	// A mixed row skips the foreign-dialect target and uses the one that
	// fits; fallback_index is the position in the row's Targets.
	f.rec.last = nil
	resp, _ = do(t, "POST", gw.URL+"/v1/messages", nil, strings.Replace(aliasBody, "sonnet", "mixed", 1))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("mixed row: status %d", resp.StatusCode)
	}
	if c := f.rec.wait(t); c.FallbackIndex != 1 || c.ResolvedModel != "claude" {
		t.Errorf("mixed row: fallback=%d resolved=%q", c.FallbackIndex, c.ResolvedModel)
	}
}

// OpenAI dialect -> openai_compat target: base_url (which, per the
// registry's convention, already includes the version segment like the
// OpenAI SDKs -- e.g. https://api.groq.com/openai/v1) + Authorization:
// Bearer from the credential, model rewritten, priced as openai. The
// client's own "/v1" is stripped before joining, so the upstream path is
// "/chat/completions", never "/v1/chat/completions" (which would double
// the target's own "/v1").
func TestRegistry_OpenAICompatTarget(t *testing.T) {
	f := newRegistryFixture(t)
	var gotAuth, gotBody, gotPath string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotAuth, gotBody, gotPath = r.Header.Get("Authorization"), string(b), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
	}))
	t.Cleanup(target.Close)
	f.credential(t, "groq", map[string]string{"api_key": "gsk_secret"})
	f.model(t, "fast", nil, store.ModelTarget{Vendor: "openai_compat", Model: "llama-3.3-70b", BaseURL: target.URL + "/openai/v1", Credential: "groq"})

	gw := f.gateway(t, Config{UpstreamBaseURL: "https://unused.invalid", OpenAIEnabled: true, OpenAIBaseURL: "https://api.openai.com"})
	resp, _ := do(t, "POST", gw.URL+"/openai/v1/chat/completions", map[string]string{"Authorization": "Bearer sk-client"},
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if gotAuth != "Bearer gsk_secret" || gotPath != "/openai/v1/chat/completions" || !strings.Contains(gotBody, `"model":"llama-3.3-70b"`) {
		t.Errorf("target saw auth=%q path=%q body=%s", gotAuth, gotPath, gotBody)
	}
	c := f.rec.wait(t)
	if c.Provider != "openai" || c.ResolvedVendor != "openai_compat" || c.ResolvedModel != "llama-3.3-70b" || c.RequestedModel != "fast" || c.InputTokens != 20 {
		t.Errorf("capture = %+v", c)
	}
}

// The "/v1" stripped from upstreamPath is a plain prefix trim, not
// specific to "/chat/completions": any upstream path behind a registry
// openai_compat target joins the same way, e.g. "/openai/v1/models" ->
// "{base}/models" (the contract's own example for this fix).
func TestRegistry_OpenAICompatTarget_StripsV1ForAnyPath(t *testing.T) {
	f := newRegistryFixture(t)
	var gotPath string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	t.Cleanup(target.Close)
	f.credential(t, "groq", map[string]string{"api_key": "gsk_secret"})
	f.model(t, "fast", nil, store.ModelTarget{Vendor: "openai_compat", Model: "llama-3.3-70b", BaseURL: target.URL + "/openai/v1", Credential: "groq"})

	gw := f.gateway(t, Config{UpstreamBaseURL: "https://unused.invalid", OpenAIEnabled: true, OpenAIBaseURL: "https://api.openai.com"})
	// A body naming the registered model, POSTed to /openai/v1/models --
	// contrived (a real listing call carries no body), but it isolates the
	// path-join fix on a path other than /chat/completions.
	resp, _ := do(t, "POST", gw.URL+"/openai/v1/models", map[string]string{"Authorization": "Bearer sk-client"}, `{"model":"fast"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if gotPath != "/openai/v1/models" {
		t.Errorf("target saw path=%q, want /openai/v1/models ({base}/models under base %s)", gotPath, target.URL+"/openai/v1")
	}
}

// The non-registry default OpenAI provider path (openaiProvider's own
// base_url, no explicit registry target) is untouched by the fix: its
// base_url does NOT include a version segment, so appending upstreamPath
// verbatim is still correct.
func TestRegistry_OpenAIDefaultProviderPathUnaffected(t *testing.T) {
	f := newRegistryFixture(t)
	var gotPath string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	t.Cleanup(target.Close)

	gw := f.gateway(t, Config{UpstreamBaseURL: "https://unused.invalid", OpenAIEnabled: true, OpenAIBaseURL: target.URL})
	resp, _ := do(t, "GET", gw.URL+"/openai/v1/models", map[string]string{"Authorization": "Bearer sk-client"}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if gotPath != "/v1/models" {
		t.Errorf("default provider path = %q, want /v1/models unchanged", gotPath)
	}
}

// hostRewriteTransport sends every request to a local server whatever host
// the URL names, so a request signed for bedrock-runtime.<region> can be
// observed by an httptest upstream speaking Bedrock's wire format.
type hostRewriteTransport struct{ to *url.URL }

func (h hostRewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host = h.to.Scheme, h.to.Host
	return http.DefaultTransport.RoundTrip(r2)
}

// Anthropic dialect -> Bedrock target: /v1/messages becomes a SigV4-signed
// InvokeModel(WithResponseStream) for the target's model in the target's
// region, signed with the row's credential; "model"/"stream" leave the body
// and anthropic_version joins it; a streamed answer is re-framed from
// Bedrock's event stream to the SSE the client expects.
func TestRegistry_AnthropicOnBedrock(t *testing.T) {
	f := newRegistryFixture(t)
	var mu sync.Mutex
	var gotPath, gotAuth, gotHost string
	var gotBody []byte
	bedrock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath, gotAuth, gotHost, gotBody = r.URL.Path, r.Header.Get("Authorization"), r.Host, b
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/invoke") {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("x-amzn-requestid", "br-1")
			_, _ = io.WriteString(w, `{"id":"msg_br","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":50,"output_tokens":5}}`)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range []string{
			`{"type":"message_start","message":{"id":"msg_br","usage":{"input_tokens":40,"output_tokens":1}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
			`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":40,"outputTokenCount":7}}`,
		} {
			payload, _ := json.Marshal(map[string]string{"bytes": base64Std(ev)})
			_, _ = w.Write(encodeEventStreamFrame(map[string]string{":message-type": "event", ":event-type": "chunk", ":content-type": "application/json"}, payload))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(bedrock.Close)
	f.credential(t, "aws-bedrock", map[string]string{"access_key_id": "AKIAREGISTRY", "secret_access_key": "secret"})
	f.model(t, "sonnet-br", nil, store.ModelTarget{Vendor: "bedrock", Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0", Region: "us-west-2", Credential: "aws-bedrock"})

	to, _ := url.Parse(bedrock.URL)
	rt := &router{
		byID:     map[string]Provider{"anthropic": newAnthropicProvider("https://api.anthropic.com")},
		recorder: f.rec,
		client:   &http.Client{Transport: hostRewriteTransport{to: to}},
		pricing:  pricing.Default,
		registry: f.registry,
		bedrock:  newBedrockProvider("us-east-1", "", ""),
	}
	gw := httptest.NewServer(withPrincipal(withTags(rt), &pkgauth.Principal{TenantID: f.tenant, Roles: []string{"agent"}}))
	t.Cleanup(gw.Close)

	// Non-streaming.
	body := `{"model":"sonnet-br","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	resp, respBody := do(t, "POST", gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk-client"}, body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(respBody, "msg_br") {
		t.Fatalf("status %d body %s", resp.StatusCode, respBody)
	}
	mu.Lock()
	if gotPath != "/model/us.anthropic.claude-sonnet-4-5-20250929-v1:0/invoke" || gotHost != "bedrock-runtime.us-west-2.amazonaws.com" {
		t.Errorf("path %q host %q", gotPath, gotHost)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIAREGISTRY/") || !strings.Contains(gotAuth, "/us-west-2/bedrock/aws4_request") {
		t.Errorf("authorization = %q; want SigV4 with the registry credential for us-west-2", gotAuth)
	}
	var up map[string]json.RawMessage
	_ = json.Unmarshal(gotBody, &up)
	if _, has := up["model"]; has || string(up["anthropic_version"]) != `"bedrock-2023-05-31"` || string(up["max_tokens"]) != "16" {
		t.Errorf("bedrock body = %s", gotBody)
	}
	mu.Unlock()
	c := f.rec.wait(t)
	if c.ResolvedVendor != "bedrock" || c.Provider != "anthropic" || c.InputTokens != 50 || c.OutputTokens != 5 || c.ProviderRequestID != "br-1" {
		t.Errorf("capture = %+v", c)
	}
	if c.CostUSD == nil {
		t.Error("cost should be priced from the rate card for the Bedrock model")
	}

	// Streaming: the client gets SSE with Anthropic's own events.
	f.rec.last = nil
	resp, respBody = do(t, "POST", gw.URL+"/v1/messages", nil, strings.Replace(body, `"max_tokens"`, `"stream":true,"max_tokens"`, 1))
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream status %d content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	mu.Lock()
	if gotPath != "/model/us.anthropic.claude-sonnet-4-5-20250929-v1:0/invoke-with-response-stream" {
		t.Errorf("stream path %q", gotPath)
	}
	_ = json.Unmarshal(gotBody, &up)
	if _, has := up["stream"]; has {
		t.Errorf("stream flag must not reach Bedrock: %s", gotBody)
	}
	mu.Unlock()
	want := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_br\",\"usage\":{\"input_tokens\":40,\"output_tokens\":1}}}\n\n"
	if !strings.HasPrefix(respBody, want) || !strings.Contains(respBody, "event: content_block_delta\ndata: ") || !strings.Contains(respBody, "event: message_stop\n") {
		t.Errorf("SSE = %q", respBody)
	}
	c = f.rec.wait(t)
	if c.InputTokens != 40 || c.OutputTokens != 7 || c.StopReason != "end_turn" || !c.Stream {
		t.Errorf("stream capture = in %d out %d stop %q stream %v", c.InputTokens, c.OutputTokens, c.StopReason, c.Stream)
	}
}

func base64Std(s string) string { return encodeB64([]byte(s)) }

func encodeB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// Event-stream decoding: a real frame round-trips, an exception frame
// becomes an Anthropic error event, and a corrupted CRC is an error.
func TestEventStreamToSSE(t *testing.T) {
	ev := `{"type":"content_block_delta","delta":{"text":"a"}}`
	payload, _ := json.Marshal(map[string]string{"bytes": encodeB64([]byte(ev))})
	frame := encodeEventStreamFrame(map[string]string{":message-type": "event", ":event-type": "chunk"}, payload)
	exc := encodeEventStreamFrame(map[string]string{":message-type": "exception", ":exception-type": "throttlingException"}, []byte(`{"message":"Too many requests"}`))

	out, err := io.ReadAll(newEventStreamToSSE(bytes.NewReader(append(append([]byte{}, frame...), exc...))))
	if err != nil {
		t.Fatal(err)
	}
	want := "event: content_block_delta\ndata: " + ev + "\n\n" +
		"event: error\ndata: {\"error\":{\"message\":\"Too many requests\",\"type\":\"throttlingException\"},\"type\":\"error\"}\n\n"
	if string(out) != want {
		t.Errorf("sse =\n%q\nwant\n%q", out, want)
	}

	// A frame delivered in small pieces still decodes (io.ReadFull).
	out, err = io.ReadAll(newEventStreamToSSE(io.MultiReader(bytes.NewReader(frame[:5]), bytes.NewReader(frame[5:]))))
	if err != nil || !bytes.HasPrefix(out, []byte("event: content_block_delta")) {
		t.Errorf("split frame: %v %q", err, out)
	}

	bad := append([]byte{}, frame...)
	bad[len(bad)-1] ^= 0xff
	if _, err := io.ReadAll(newEventStreamToSSE(bytes.NewReader(bad))); err == nil || !strings.Contains(err.Error(), "CRC") {
		t.Errorf("corrupted frame error = %v", err)
	}
	if _, err := io.ReadAll(newEventStreamToSSE(bytes.NewReader(frame[:7]))); err == nil {
		t.Error("truncated prelude must be an error")
	}
}

func TestRequiresTranslation(t *testing.T) {
	cases := []struct {
		dialect, path string
		t             store.ModelTarget
		want          bool
	}{
		{"anthropic", "/v1/messages", store.ModelTarget{Vendor: "anthropic", Model: "claude"}, false},
		{"anthropic", "/v1/messages/count_tokens", store.ModelTarget{Vendor: "anthropic", Model: "claude"}, false},
		{"anthropic", "/v1/messages", store.ModelTarget{Vendor: "bedrock", Model: "anthropic.claude-3-haiku-20240307-v1:0"}, false},
		{"anthropic", "/v1/messages", store.ModelTarget{Vendor: "bedrock", Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0"}, false},
		{"anthropic", "/v1/messages", store.ModelTarget{Vendor: "bedrock", Model: "global.anthropic.claude-opus-4-1-v1:0"}, false},
		{"anthropic", "/v1/messages", store.ModelTarget{Vendor: "bedrock", Model: "amazon.nova-lite-v1:0"}, true},
		{"anthropic", "/v1/messages/count_tokens", store.ModelTarget{Vendor: "bedrock", Model: "anthropic.claude"}, true},
		{"anthropic", "/v1/messages", store.ModelTarget{Vendor: "openai_compat", Model: "gpt"}, true},
		{"anthropic", "/v1/messages", store.ModelTarget{Vendor: "gemini", Model: "g"}, true},
		{"openai", "/v1/chat/completions", store.ModelTarget{Vendor: "openai_compat", Model: "gpt"}, false},
		{"openai", "/v1/chat/completions", store.ModelTarget{Vendor: "anthropic", Model: "claude"}, true},
		{"bedrock", "/model/x/converse", store.ModelTarget{Vendor: "bedrock", Model: "y"}, false},
		{"bedrock", "/model/x/converse", store.ModelTarget{Vendor: "anthropic", Model: "y"}, true},
		{"gemini", "/v1beta/models/x:generateContent", store.ModelTarget{Vendor: "gemini", Model: "y"}, false},
		{"gemini", "/v1beta/models/x:generateContent", store.ModelTarget{Vendor: "openai_compat", Model: "y"}, true},
	}
	for _, c := range cases {
		if got := requiresTranslation(c.dialect, c.path, c.t); got != c.want {
			t.Errorf("requiresTranslation(%s, %s, %s/%s) = %v, want %v", c.dialect, c.path, c.t.Vendor, c.t.Model, got, c.want)
		}
	}
}

// The resolver caches hits and misses per tenant for its TTL, Invalidate
// drops one tenant's entries (or every tenant's for a platform change),
// and a nil Registry resolves nothing.
func TestRegistry_CacheAndInvalidate(t *testing.T) {
	st := dptest.New()
	now := time.Unix(1_700_000_000, 0)
	reg := NewRegistry(st.Models(), nil, RegistryOptions{TTL: 30 * time.Second, Now: func() time.Time { return now }})
	ctx := context.Background()

	if m, err := reg.Resolve(ctx, "t1", "sonnet"); m != nil || err != nil {
		t.Fatalf("miss = %v, %v", m, err)
	}
	if err := st.Models().Create(ctx, &store.Model{TenantID: "t1", Name: "sonnet", Enabled: true, Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := reg.Resolve(ctx, "t1", "sonnet"); m != nil {
		t.Error("negative entry should still be cached")
	}
	now = now.Add(31 * time.Second)
	if m, _ := reg.Resolve(ctx, "t1", "sonnet"); m == nil {
		t.Error("expired negative entry should re-query")
	}
	if m, _ := reg.Resolve(ctx, "t2", "sonnet"); m != nil {
		t.Error("another tenant must not see t1's row")
	}

	// Platform row: visible to t2; invalidating t1 alone keeps t2's cache.
	if err := st.Models().Create(ctx, &store.Model{Name: "haiku", Enabled: true, Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "h"}}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := reg.Resolve(ctx, "t2", "haiku"); m == nil || m.TenantID != "" {
		t.Fatalf("platform row not resolved for t2: %v", m)
	}
	if err := st.Models().Create(ctx, &store.Model{TenantID: "t2", Name: "haiku", Enabled: true, Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "override"}}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := reg.Resolve(ctx, "t2", "haiku"); m.TenantID != "" {
		t.Error("cached platform resolution should survive until invalidated")
	}
	reg.Invalidate("t1")
	if m, _ := reg.Resolve(ctx, "t2", "haiku"); m.TenantID != "" {
		t.Error("invalidating t1 must not drop t2's entries")
	}
	reg.Invalidate("t2")
	if m, _ := reg.Resolve(ctx, "t2", "haiku"); m.TenantID != "t2" {
		t.Error("after Invalidate(t2) the tenant override should resolve")
	}
	reg.Invalidate("")
	if len(reg.cache) != 0 {
		t.Error("Invalidate(\"\") should drop every entry")
	}

	var nilReg *Registry
	if m, err := nilReg.Resolve(ctx, "t1", "sonnet"); m != nil || err != nil {
		t.Error("nil registry must resolve nothing")
	}
	nilReg.Invalidate("t1")
}

// SeedModels upserts platform rows by name: created once, updated after,
// never duplicated, and never touching tenant rows or unrelated platform rows.
func TestSeedModels(t *testing.T) {
	st := dptest.New()
	ctx := context.Background()
	if err := st.Models().Create(ctx, &store.Model{Name: "keep", Enabled: true, Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "k"}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Models().Create(ctx, &store.Model{TenantID: "t1", Name: "sonnet", Enabled: true, Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "tenant-own"}}}); err != nil {
		t.Fatal(err)
	}
	seeds := []config.ModelSeed{{
		Name: "sonnet", Description: "default",
		Targets: []config.ModelSeedTarget{{Vendor: "anthropic", Model: "claude-sonnet-4-5", BaseURL: "https://proxy.internal", AllowCallerKey: true}, {Vendor: "bedrock", Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0", Region: "us-east-1"}},
		Price:   &config.ModelSeedPrice{Input: 3, Output: 15},
	}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	created, updated, err := SeedModels(ctx, st.Models(), seeds, logger)
	if err != nil || created != 1 || updated != 0 {
		t.Fatalf("first seed: created %d updated %d err %v", created, updated, err)
	}
	seeds[0].Targets = []config.ModelSeedTarget{seeds[0].Targets[0], {Vendor: "openai_compat", Model: "qwen3", BaseURL: "http://localhost:11434/v1", Label: "ollama", AllowCallerKey: true}}
	created, updated, err = SeedModels(ctx, st.Models(), seeds, logger)
	if err != nil || created != 0 || updated != 1 {
		t.Fatalf("second seed: created %d updated %d err %v", created, updated, err)
	}
	plat, err := st.Models().List(ctx, "", store.ListOptions{})
	if err != nil || len(plat) != 2 {
		t.Fatalf("platform rows = %v, %v; want keep + sonnet", plat, err)
	}
	seeded, _ := st.Models().GetByName(ctx, "", "sonnet")
	if !seeded.Targets[0].AllowCallerKey {
		t.Error("seed did not carry allow_caller_key")
	}
	if t1 := seeded.Targets[1]; t1.Label != "ollama" || t1.Vendor != "openai_compat" || !t1.AllowCallerKey {
		t.Errorf("seed did not carry the label: %+v", t1)
	}
	if seeded.TenantID != "" || len(seeded.Targets) != 2 || seeded.Price == nil || seeded.Price.Output != 15 || seeded.Metadata["seed"] != "config" {
		t.Errorf("seeded row = %+v", seeded)
	}
	own, _ := st.Models().GetByName(ctx, "t1", "sonnet")
	if own.TenantID != "t1" || own.Targets[0].Model != "tenant-own" {
		t.Errorf("tenant row was touched: %+v", own)
	}
}
