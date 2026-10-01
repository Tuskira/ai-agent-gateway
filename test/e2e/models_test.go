//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api"
	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/capture"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/llmplane"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/secrets"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	pgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/postgres"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// ---------------------------------------------------------------------------
// Model-registry harness: API plane + LLM plane over one Postgres, the
// registry shared between them exactly as cmd/gateway wires it, durable
// capture through the real Postgres sink, and httptest upstreams speaking
// the Anthropic wire format.
// ---------------------------------------------------------------------------

type registryHarness struct {
	apiBaseURL string
	llmBaseURL string
	dsn        string

	store    store.Store
	secrets  *secrets.Service
	tenant   *store.Tenant
	adminKey string
	agentKey string

	passthrough *recordingUpstream
}

// recordingUpstream is an httptest server that answers as Anthropic does
// and remembers the last request it saw.
type recordingUpstream struct {
	*httptest.Server
	hits   atomic.Int32
	status int

	mu     sync.Mutex
	body   []byte
	header http.Header
	path   string
}

func newRecordingUpstream(t *testing.T, status int) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{status: status}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.body, u.header, u.path = b, r.Header.Clone(), r.URL.RequestURI()
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		if u.status == http.StatusOK {
			_, _ = io.WriteString(w, `{"id":"msg_e2e","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7}}`)
		} else {
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"busy"}}`)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *recordingUpstream) last() ([]byte, http.Header, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.body, u.header, u.path
}

func newRegistryHarness(t *testing.T) *registryHarness {
	t.Helper()
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping the model-registry end-to-end test")
	}
	ctx := context.Background()
	st, dsn := newIsolatedStoreDSN(t, rawURL)

	tenant := &store.Tenant{Slug: "e2e-models-" + shortID(), Name: "E2E Models Tenant"}
	if err := st.Tenants().Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	ring := &secrets.KeyRing{Keys: map[string][]byte{"k1": bytes.Repeat([]byte{0x5a}, 32)}, ActiveKeyID: "k1"}
	secretSvc := secrets.NewService(st.Credentials(), ring)
	registry := llmplane.NewRegistry(st.Models(), secretSvc, llmplane.RegistryOptions{})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authenticator := apikey.New(st.APIKeys(), apikey.Options{})
	authorizer := pkgauth.NewRoleAuthorizer()

	// API plane, with the registry as the ModelInvalidator (cmd/gateway).
	apiHandler := api.NewRouter(api.Deps{
		ServiceVersion:   "e2e",
		Authenticator:    authenticator,
		Authorizer:       authorizer,
		Logger:           logger,
		Store:            st,
		Secrets:          secretSvc,
		ModelInvalidator: registry,
	})
	apiAddr := serveOnFreePort(t, apiHandler)

	// LLM plane: durable capture through the real Postgres sink into this
	// test's schema, the registry on Config.Registry.
	batch, err := pgsink.New(ctx, pgsink.Options{DSN: dsn})
	if err != nil {
		t.Fatalf("postgres sink: %v", err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	recorder := capture.NewRecorder(batch, nil, nil, 0)

	passthrough := newRecordingUpstream(t, http.StatusOK)
	cfg := config.Default()
	core, err := llmplane.Handler(llmplane.Config{
		UpstreamBaseURL:         passthrough.URL,
		MaxRequestBytes:         cfg.LLMProxy.Limits.MaxRequestBytes,
		MaxStreamDuration:       cfg.LLMProxy.Limits.MaxStreamDuration,
		MaxConcurrentPerTenant:  cfg.LLMProxy.Limits.MaxConcurrentPerTenant,
		BedrockRegion:           "us-east-1",
		Authorizer:              authorizer,
		StoreBodies:             true,
		MaxCaptureRequestBytes:  1 << 20,
		MaxCaptureResponseBytes: 1 << 20,
		Pricing:                 pricing.Default,
		Registry:                registry,
	}, recorder)
	if err != nil {
		t.Fatalf("build llm plane: %v", err)
	}
	llmAddr := serveOnFreePort(t, internalauth.Middleware(authenticator, logger)(core))

	return &registryHarness{
		apiBaseURL:  "http://" + apiAddr,
		llmBaseURL:  "http://" + llmAddr,
		dsn:         dsn,
		store:       st,
		secrets:     secretSvc,
		tenant:      tenant,
		adminKey:    newAPIKey(t, st, tenant.ID, "e2e-admin", "admin"),
		agentKey:    newAPIKey(t, st, tenant.ID, "e2e-agent", "agent"),
		passthrough: passthrough,
	}
}

func serveOnFreePort(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h, ReadTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return ln.Addr().String()
}

// api issues a control-plane request as the admin key and decodes the
// JSON body into dst when non-nil.
func (h *registryHarness) api(t *testing.T, method, path, body string, dst any) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.apiBaseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if dst != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp
}

// messages posts one Anthropic Messages request as the agent key.
func (h *registryHarness) messages(t *testing.T, body string, extra map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.llmBaseURL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.agentKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// capturedRow reads the durable capture row for the call whose request
// body mentions marker, waiting briefly for the synchronous write to land.
type capturedRow struct {
	Provider, Model, RequestedModel, ResolvedVendor, ResolvedModel string
	FallbackIndex, StatusCode                                      int
	Translated                                                     bool
	InputTokens                                                    int64
	CostUSD                                                        sql.NullFloat64
}

func (h *registryHarness) capturedRow(t *testing.T, marker string) capturedRow {
	t.Helper()
	return h.capturedRowWhere(t, `convert_from(request_body, 'UTF8') LIKE $2`, "%"+marker+"%")
}

// refusedRow reads the capture row of a call the gateway refused before
// any upstream call. Such rows carry no request body (nothing was sent),
// so they are found by the model the client asked for and their status.
func (h *registryHarness) refusedRow(t *testing.T, requestedModel string) capturedRow {
	t.Helper()
	return h.capturedRowWhere(t, `status_code = 400 AND requested_model = $2`, requestedModel)
}

func (h *registryHarness) capturedRowWhere(t *testing.T, where, arg string) capturedRow {
	t.Helper()
	db, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var row capturedRow
	for i := 0; i < 50; i++ {
		err = db.QueryRow(`SELECT provider, model, requested_model, coalesce(resolved_vendor, ''), coalesce(resolved_model, ''), fallback_index, status_code, translated, input_tokens, cost_usd
			FROM llm_calls WHERE tenant_id = $1 AND `+where+` ORDER BY timestamp DESC LIMIT 1`,
			h.tenant.ID, arg).
			Scan(&row.Provider, &row.Model, &row.RequestedModel, &row.ResolvedVendor, &row.ResolvedModel, &row.FallbackIndex, &row.StatusCode, &row.Translated, &row.InputTokens, &row.CostUSD)
		if err == nil {
			return row
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no llm_calls row for %q: %v", arg, err)
	return row
}

func shortID() string { return uuid.NewString()[:8] }

// ---------------------------------------------------------------------------
// the test
// ---------------------------------------------------------------------------

// TestModelRegistryEndToEnd drives the registry the way an operator and a
// client do: rows created through /api/v1/models, an Anthropic-format call
// naming the alias landing on the alias's target host with the alias's
// credential and the target's model id, fallback from a throttled target
// to the next, an unregistered name forwarded byte-for-byte, and the
// durable capture row carrying requested/resolved fields -- all over real
// sockets, real Postgres, the real secret store and the real Postgres sink.
func TestModelRegistryEndToEnd(t *testing.T) {
	h := newRegistryHarness(t)
	ctx := context.Background()

	target := newRecordingUpstream(t, http.StatusOK)
	throttled := newRecordingUpstream(t, http.StatusTooManyRequests)

	if _, err := h.secrets.Create(ctx, h.tenant.ID, "anthropic-prod", "api_key", map[string]string{"api_key": "sk-ant-e2e-registry"}, "e2e"); err != nil {
		t.Fatalf("create credential: %v", err)
	}

	var created struct {
		ID    string `json:"id"`
		Scope string `json:"scope"`
	}
	resp := h.api(t, http.MethodPost, "/api/v1/models", fmt.Sprintf(`{
		"name":"sonnet","description":"e2e",
		"targets":[{"vendor":"anthropic","model":"claude-sonnet-4-5","base_url":%q,"credential":"anthropic-prod"}],
		"price":{"input":2,"output":10}}`, target.URL), &created)
	if resp.StatusCode != http.StatusCreated || created.Scope != "tenant" || created.ID == "" {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /models = %d %s", resp.StatusCode, b)
	}
	resp = h.api(t, http.MethodPost, "/api/v1/models", fmt.Sprintf(`{
		"name":"resilient",
		"targets":[{"vendor":"anthropic","model":"primary","base_url":%q,"allow_caller_key":true},{"vendor":"anthropic","model":"backup","base_url":%q,"allow_caller_key":true}]}`, throttled.URL, target.URL), nil)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /models (resilient) = %d %s", resp.StatusCode, b)
	}
	// A credential that does not exist is refused at registration time.
	if resp = h.api(t, http.MethodPost, "/api/v1/models", `{"name":"broken","targets":[{"vendor":"anthropic","model":"x","credential":"nope"}]}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /models with unknown credential = %d, want 400", resp.StatusCode)
	}

	t.Run("alias swaps host, credential and model", func(t *testing.T) {
		body := `{"model":"sonnet","max_tokens":16,"messages":[{"role":"user","content":"e2e-alias"}]}`
		resp, out := h.messages(t, body, map[string]string{"x-api-key": "sk-client-own"})
		if resp.StatusCode != http.StatusOK || !strings.Contains(out, "msg_e2e") {
			t.Fatalf("status %d body %s", resp.StatusCode, out)
		}
		if h.passthrough.hits.Load() != 0 {
			t.Error("the alias reached the passthrough upstream")
		}
		got, hdr, path := target.last()
		if path != "/v1/messages" || !strings.Contains(string(got), `"model":"claude-sonnet-4-5"`) || strings.Contains(string(got), `"sonnet"`) {
			t.Errorf("target saw path %q body %s", path, got)
		}
		if hdr.Get("x-api-key") != "sk-ant-e2e-registry" {
			t.Errorf("target x-api-key = %q, want the registered credential", hdr.Get("x-api-key"))
		}
		if hdr.Get("Authorization") != "" {
			t.Errorf("the gateway key leaked upstream: %q", hdr.Get("Authorization"))
		}
		row := h.capturedRow(t, "e2e-alias")
		if row.Provider != "anthropic" || row.RequestedModel != "sonnet" || row.ResolvedVendor != "anthropic" || row.ResolvedModel != "claude-sonnet-4-5" ||
			row.Model != "claude-sonnet-4-5" || row.FallbackIndex != 0 || row.Translated || row.StatusCode != 200 || row.InputTokens != 11 {
			t.Errorf("captured row = %+v", row)
		}
		// 11 in * $2/M + 7 out * $10/M = 0.000092
		if !row.CostUSD.Valid || row.CostUSD.Float64 < 0.0000919 || row.CostUSD.Float64 > 0.0000921 {
			t.Errorf("cost = %+v, want the row's price (0.000092)", row.CostUSD)
		}
	})

	t.Run("fallback: throttled first target, second answers", func(t *testing.T) {
		before := target.hits.Load()
		body := `{"model":"resilient","max_tokens":16,"messages":[{"role":"user","content":"e2e-fallback"}]}`
		resp, out := h.messages(t, body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d body %s", resp.StatusCode, out)
		}
		if throttled.hits.Load() != 1 || target.hits.Load() != before+1 {
			t.Errorf("hits throttled=%d target=%d", throttled.hits.Load(), target.hits.Load())
		}
		if got, _, _ := target.last(); !strings.Contains(string(got), `"model":"backup"`) {
			t.Errorf("second target got %s", got)
		}
		if row := h.capturedRow(t, "e2e-fallback"); row.FallbackIndex != 1 || row.ResolvedModel != "backup" {
			t.Errorf("captured row = %+v", row)
		}
	})

	t.Run("unregistered name is byte-identical passthrough", func(t *testing.T) {
		body := `{"model":"claude-haiku-4-5",  "max_tokens":16, "messages":[{"role":"user","content":"e2e-passthrough"}]}`
		resp, _ := h.messages(t, body, map[string]string{"x-api-key": "sk-client-own"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		got, hdr, _ := h.passthrough.last()
		if string(got) != body {
			t.Errorf("upstream body differs:\n got %s\nwant %s", got, body)
		}
		if hdr.Get("x-api-key") != "sk-client-own" {
			t.Errorf("BYOK x-api-key = %q", hdr.Get("x-api-key"))
		}
		if row := h.capturedRow(t, "e2e-passthrough"); row.RequestedModel != "claude-haiku-4-5" || row.ResolvedModel != "" || row.ResolvedVendor != "" || row.FallbackIndex != 0 {
			t.Errorf("captured row = %+v", row)
		}
	})

	t.Run("a registry write takes effect immediately (cache invalidated)", func(t *testing.T) {
		other := newRecordingUpstream(t, http.StatusOK)
		resp := h.api(t, http.MethodPut, "/api/v1/models/"+created.ID, fmt.Sprintf(`{
			"name":"sonnet","targets":[{"vendor":"anthropic","model":"claude-sonnet-4-5-moved","base_url":%q,"allow_caller_key":true}]}`, other.URL), nil)
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("PUT /models = %d %s", resp.StatusCode, b)
		}
		body := `{"model":"sonnet","max_tokens":16,"messages":[{"role":"user","content":"e2e-moved"}]}`
		if resp, out := h.messages(t, body, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d body %s", resp.StatusCode, out)
		}
		if other.hits.Load() != 1 {
			t.Error("the updated target was not used right after the write")
		}
		if got, _, _ := other.last(); !strings.Contains(string(got), `"model":"claude-sonnet-4-5-moved"`) {
			t.Errorf("moved target got %s", got)
		}
	})

	t.Run("a caller's key is not sent to a custom host unless the target opts in", func(t *testing.T) {
		harvester := newRecordingUpstream(t, http.StatusOK)
		var made struct {
			ID string `json:"id"`
		}
		resp := h.api(t, http.MethodPost, "/api/v1/models", fmt.Sprintf(`{
			"name":"claude-opus-4-1","targets":[{"vendor":"anthropic","model":"claude-opus-4-1","base_url":%q}]}`, harvester.URL), &made)
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST /models = %d %s", resp.StatusCode, b)
		}
		body := `{"model":"claude-opus-4-1","max_tokens":16,"messages":[{"role":"user","content":"e2e-harvest"}]}`
		resp2, out := h.messages(t, body, map[string]string{"x-api-key": "sk-client-own"})
		if resp2.StatusCode != http.StatusBadRequest || !strings.Contains(out, "model claude-opus-4-1: no credential for target 0 (set a credential or allow_caller_key)") {
			t.Errorf("status %d body %s", resp2.StatusCode, out)
		}
		if harvester.hits.Load() != 0 {
			t.Errorf("the custom host was called %d times and could have seen the caller's key", harvester.hits.Load())
		}
		if row := h.refusedRow(t, "claude-opus-4-1"); row.FallbackIndex != -1 || row.ResolvedModel != "" {
			t.Errorf("captured row = %+v", row)
		}

		// Opting in forwards the caller's key (and only that).
		resp = h.api(t, http.MethodPut, "/api/v1/models/"+made.ID, fmt.Sprintf(`{
			"name":"claude-opus-4-1","targets":[{"vendor":"anthropic","model":"claude-opus-4-1","base_url":%q,"allow_caller_key":true}]}`, harvester.URL), nil)
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("PUT /models = %d %s", resp.StatusCode, b)
		}
		if resp2, out := h.messages(t, body, map[string]string{"x-api-key": "sk-client-own"}); resp2.StatusCode != http.StatusOK {
			t.Fatalf("status %d body %s", resp2.StatusCode, out)
		}
		if _, hdr, _ := harvester.last(); hdr.Get("x-api-key") != "sk-client-own" || hdr.Get("Authorization") != "" {
			t.Errorf("opted-in target saw x-api-key=%q authorization=%q", hdr.Get("x-api-key"), hdr.Get("Authorization"))
		}
	})

	t.Run("list shows scope and no credential values", func(t *testing.T) {
		var page struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
		}
		resp := h.api(t, http.MethodGet, "/api/v1/models", "", &page)
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || page.Total != 3 {
			t.Fatalf("GET /models = %d %s", resp.StatusCode, raw)
		}
		if strings.Contains(string(raw), "sk-ant-e2e-registry") {
			t.Error("credential value leaked into the list")
		}
		for _, it := range page.Items {
			if it["scope"] != "tenant" {
				t.Errorf("item scope = %v", it["scope"])
			}
		}
	})
}

// TestModelRegistryRealVendors exercises one real call per vendor whose
// credentials the environment provides, through a registered alias; it
// skips clearly otherwise. It never fails silently on a vendor error: a
// non-2xx from the real vendor is a test failure with the vendor's body.
func TestModelRegistryRealVendors(t *testing.T) {
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	awsPresent := os.Getenv("AWS_ACCESS_KEY_ID") != "" || os.Getenv("AWS_PROFILE") != ""
	if anthropicKey == "" && !awsPresent {
		t.Skip("neither ANTHROPIC_API_KEY nor AWS credentials (AWS_ACCESS_KEY_ID / AWS_PROFILE) are set; skipping the real-vendor model-registry calls")
	}
	h := newRegistryHarness(t)
	ctx := context.Background()

	t.Run("anthropic", func(t *testing.T) {
		if anthropicKey == "" {
			t.Skip("ANTHROPIC_API_KEY not set; skipping the real Anthropic call")
		}
		if _, err := h.secrets.Create(ctx, h.tenant.ID, "anthropic-real", "api_key", map[string]string{"api_key": anthropicKey}, "e2e"); err != nil {
			t.Fatal(err)
		}
		resp := h.api(t, http.MethodPost, "/api/v1/models", `{"name":"real-haiku","targets":[{"vendor":"anthropic","model":"claude-haiku-4-5","credential":"anthropic-real"}]}`, nil)
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST /models = %d %s", resp.StatusCode, b)
		}
		body := `{"model":"real-haiku","max_tokens":8,"messages":[{"role":"user","content":"Reply with the single word: pong (e2e-real-anthropic)"}]}`
		resp2, out := h.messages(t, body, nil)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("real Anthropic call through alias: %d %s", resp2.StatusCode, out)
		}
		if row := h.capturedRow(t, "e2e-real-anthropic"); row.ResolvedVendor != "anthropic" || row.InputTokens == 0 || !row.CostUSD.Valid {
			t.Errorf("captured row = %+v", row)
		}
	})

	t.Run("bedrock", func(t *testing.T) {
		if !awsPresent {
			t.Skip("no AWS credentials in the environment; skipping the real Bedrock call")
		}
		region := os.Getenv("AWS_REGION")
		if region == "" {
			region = "us-east-1"
		}
		model := os.Getenv("GATEWAY_E2E_BEDROCK_MODEL")
		if model == "" {
			model = "us.anthropic.claude-haiku-4-5-20251001-v1:0"
		}
		// Gateway identity (no credential on the target): the default AWS
		// chain the process runs with.
		resp := h.api(t, http.MethodPost, "/api/v1/models", fmt.Sprintf(`{"name":"real-bedrock","targets":[{"vendor":"bedrock","model":%q,"region":%q}]}`, model, region), nil)
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST /models = %d %s", resp.StatusCode, b)
		}
		body := `{"model":"real-bedrock","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"Reply with the single word: pong (e2e-real-bedrock)"}]}`
		resp2, out := h.messages(t, body, nil)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("real Bedrock call through alias (%s in %s): %d %s", model, region, resp2.StatusCode, out)
		}
		if !strings.HasPrefix(resp2.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") {
			t.Errorf("Bedrock stream was not re-framed as SSE: %q", out)
		}
		if row := h.capturedRow(t, "e2e-real-bedrock"); row.ResolvedVendor != "bedrock" || row.InputTokens == 0 {
			t.Errorf("captured row = %+v", row)
		}
	})
}
