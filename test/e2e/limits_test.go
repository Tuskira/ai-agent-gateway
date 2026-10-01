//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api"
	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/capture"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/llmplane"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	pkgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	pgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/postgres"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// limitsHarness is the API plane and the LLM plane over one real Postgres
// schema, wired as cmd/gateway wires them: keys are created with limits
// through POST /api/v1/api-keys, calls go through the real api-key
// authenticator and the plane's Limiter, capture goes to the real Postgres
// llm_calls sink (which is also the SpendReader), and /api/v1/health
// reports the Limiter's counters.
type limitsHarness struct {
	upstreamURL    string
	apiURL, llmURL string
	adminKey       string
	sink           *pgsink.Sink
	check          *sql.DB
	tenantID       string
	upstreamHits   *atomic.Int32
}

func newLimitsHarness(t *testing.T) *limitsHarness {
	t.Helper()
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping the limits end-to-end test")
	}
	ctx := context.Background()
	st, dsn := newIsolatedStoreDSN(t, rawURL)

	tenant := &store.Tenant{Slug: "e2e-lim-" + uuid.NewString()[:8], Name: "E2E limits"}
	if err := st.Tenants().Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	sk, err := pgsink.New(ctx, pgsink.Options{DSN: dsn})
	if err != nil {
		t.Fatalf("postgres llm sink: %v", err)
	}
	t.Cleanup(func() { _ = sk.Close() })
	check, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { check.Close() })

	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_e2e","stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7}}`)
	}))
	t.Cleanup(upstream.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authenticator := apikey.New(st.APIKeys(), apikey.Options{})
	authorizer := pkgauth.NewRoleAuthorizer()

	// The same order as cmd/gateway/main.go's llm_proxy block: the limiter
	// over the store's keys and the Postgres capture sink as SpendReader.
	limiter, err := llmplane.NewLimiter(llmplane.LimiterConfig{Keys: st.APIKeys(), Spend: sk})
	if err != nil {
		t.Fatal(err)
	}
	registry := llmplane.NewRegistry(st.Models(), nil, llmplane.RegistryOptions{})
	core, err := llmplane.Handler(llmplane.Config{
		UpstreamBaseURL: upstream.URL,
		MaxRequestBytes: 1 << 20,
		Authorizer:      authorizer,
		Pricing:         pricing.Default,
		Limiter:         limiter,
		Registry:        registry,
	}, capture.NewRecorder(sk, nil, nil, 0))
	if err != nil {
		t.Fatalf("build llm plane: %v", err)
	}
	llmURL := serve(t, internalauth.Middleware(authenticator, logger)(core))

	apiURL := serve(t, api.NewRouter(api.Deps{
		ServiceVersion:   "e2e",
		Authenticator:    authenticator,
		Authorizer:       authorizer,
		Logger:           logger,
		Store:            st,
		LimitsStatus:     limiter.Status,
		ModelInvalidator: registry,
	}))

	return &limitsHarness{
		upstreamURL: upstream.URL,
		apiURL:      apiURL, llmURL: llmURL,
		adminKey: newAPIKey(t, st, tenant.ID, "e2e-admin", "admin"),
		sink:     sk, check: check, tenantID: tenant.ID, upstreamHits: &hits,
	}
}

func serve(t *testing.T, h http.Handler) string {
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
	return "http://" + ln.Addr().String()
}

func (h *limitsHarness) apiCall(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, h.apiURL+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// createKey creates an agent key with limits through the API and returns
// its id and plaintext.
func (h *limitsHarness) createKey(t *testing.T, limits string) (id, plaintext string) {
	t.Helper()
	code, body := h.apiCall(t, http.MethodPost, "/api-keys", `{"name":"budgeted","role":"agent","limits":`+limits+`}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api-keys = %d %s", code, body)
	}
	var out struct {
		ID     string          `json:"id"`
		Key    string          `json:"key"`
		Limits json.RawMessage `json:"limits"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Key == "" || len(out.Limits) == 0 {
		t.Fatalf("POST /api-keys body = %s (%v); want id, key and limits", body, err)
	}
	return out.ID, out.Key
}

func (h *limitsHarness) message(t *testing.T, key string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.llmURL+"/v1/messages",
		strings.NewReader(`{"model":"claude-haiku-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	return resp
}

func readError(t *testing.T, resp *http.Response) (string, string) {
	t.Helper()
	defer resp.Body.Close()
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &env); err != nil || env.Type != "error" {
		t.Fatalf("not an Anthropic error envelope: %s", b)
	}
	return env.Error.Type, env.Error.Message
}

func (h *limitsHarness) health(t *testing.T) map[string]any {
	t.Helper()
	code, body := h.apiCall(t, http.MethodGet, "/health", "")
	var out struct {
		Limits map[string]any `json:"limits"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &out) != nil || out.Limits == nil {
		t.Fatalf("GET /health = %d %s; want limits", code, body)
	}
	return out.Limits
}

// TestE2E_Limits_DailyBudget: a key created with daily_usd 0.01 through the
// API, two priced llm_calls rows already captured for it today → the next
// call is 429 rate_limit_error with a Retry-After to the next UTC midnight,
// is never forwarded, lands in llm_calls as a 429 row with its error, and
// /api/v1/health counts it. A second key PATCHed from daily_usd 0 to 5
// before its first call is served (the PATCH lands in the store the plane
// reads; an already-cached key would pick it up within 10 s).
func TestE2E_Limits_DailyBudget(t *testing.T) {
	h := newLimitsHarness(t)
	keyID, key := h.createKey(t, `{"daily_usd":0.01}`)

	for _, cost := range []float64{0.006, 0.0055} {
		cost := cost
		if err := h.sink.WriteBatch(context.Background(), []*pkgsink.LLMCall{{
			Timestamp: time.Now(), RequestID: uuid.NewString(), TenantID: h.tenantID, KeyID: keyID,
			Provider: "anthropic", Model: "claude-haiku-4-5", StatusCode: 200, CostUSD: &cost,
		}}); err != nil {
			t.Fatalf("seed llm_calls: %v", err)
		}
	}

	resp := h.message(t, key)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	now := time.Now().UTC()
	want := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC).Sub(now).Seconds()
	if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || float64(ra) < want-2 || float64(ra) > want+2 {
		t.Fatalf("Retry-After = %q, want ≈ %.0f", resp.Header.Get("Retry-After"), want)
	}
	if typ, msg := readError(t, resp); typ != "rate_limit_error" || msg != "daily budget exceeded for this key" {
		t.Fatalf("error = %s: %s", typ, msg)
	}
	if h.upstreamHits.Load() != 0 {
		t.Fatal("a call over budget reached the upstream")
	}

	var status int
	var errText string
	if err := h.check.QueryRow(`SELECT status_code, error FROM llm_calls WHERE key_id = $1 AND status_code <> 200`, keyID).Scan(&status, &errText); err != nil {
		t.Fatalf("denied call not captured: %v", err)
	}
	if status != 429 || !strings.HasPrefix(errText, "budget_exceeded:") {
		t.Fatalf("captured denial = %d %q", status, errText)
	}
	if got := h.health(t)["budget_denials"]; got != float64(1) {
		t.Fatalf("health budget_denials = %v, want 1", got)
	}

	// A key PATCHed to a larger budget before its first call is served.
	id2, key2 := h.createKey(t, `{"daily_usd":0}`)
	if code, body := h.apiCall(t, http.MethodPatch, "/api-keys/"+id2, `{"limits":{"daily_usd":5}}`); code != http.StatusOK ||
		!strings.Contains(string(body), `"limits":{"daily_usd":5}`) {
		t.Fatalf("PATCH = %d %s", code, body)
	}
	if resp := h.message(t, key2); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("patched key status = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

// TestE2E_Limits_RPM: rpm 2 → two calls served, the third within the same
// second is 429 with Retry-After: 1, counted in /api/v1/health.
func TestE2E_Limits_RPM(t *testing.T) {
	h := newLimitsHarness(t)
	_, key := h.createKey(t, `{"rpm":2}`)

	start := time.Now()
	for i := 0; i < 2; i++ {
		resp := h.message(t, key)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("call %d status = %d, want 200", i+1, resp.StatusCode)
		}
	}
	resp := h.message(t, key)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Logf("note: three calls took %v", elapsed)
	}
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("third call = %d Retry-After %q, want 429 / 1", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if typ, _ := readError(t, resp); typ != "rate_limit_error" {
		t.Fatalf("error type = %q", typ)
	}
	if h.upstreamHits.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2", h.upstreamHits.Load())
	}
	if got := h.health(t)["rpm_denials"]; got != float64(1) {
		t.Fatalf("health rpm_denials = %v, want 1", got)
	}
}

// messageFor is message for a named model with a max_tokens.
func (h *limitsHarness) messageFor(t *testing.T, key, model string, maxTokens int) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.llmURL+"/v1/messages",
		strings.NewReader(`{"model":"`+model+`","max_tokens":`+strconv.Itoa(maxTokens)+`,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", "sk-caller")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	return resp
}

// drain reads a response to its end before closing it, so the client
// reuses the connection. (Closing an unread body drops the connection, and
// the dial that replaces it can leave the server a connection that never
// carries a request, which http.Server.Shutdown waits 5 s on.)
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// TestE2E_ModelLimits: a model registered through POST /api/v1/models with
// limits {rpm: 2, max_tokens: 64}, called by a key that has no limits of
// its own. max_tokens over the cap is a 400; two calls are served and the
// third within the minute is 429 with Retry-After: 1, naming the model;
// the denials land in llm_calls with requested_model and fallback_index -1
// and count in /api/v1/health; an unregistered model on the same plane is
// not limited; a PUT that drops the limits lifts them at once (the
// registry is the API's ModelInvalidator).
func TestE2E_ModelLimits(t *testing.T) {
	h := newLimitsHarness(t)
	code, body := h.apiCall(t, http.MethodPost, "/api-keys", `{"name":"plain","role":"agent"}`)
	var key struct {
		Key string `json:"key"`
	}
	if code != http.StatusCreated || json.Unmarshal(body, &key) != nil || key.Key == "" {
		t.Fatalf("POST /api-keys = %d %s", code, body)
	}
	target := `{"vendor":"anthropic","model":"claude-haiku-4-5","base_url":"` + h.upstreamURL + `","allow_caller_key":true}`
	code, body = h.apiCall(t, http.MethodPost, "/models", `{"name":"team-haiku","targets":[`+target+`],"limits":{"rpm":2,"max_tokens":64}}`)
	var model struct {
		ID     string          `json:"id"`
		Limits json.RawMessage `json:"limits"`
	}
	if code != http.StatusCreated || json.Unmarshal(body, &model) != nil || string(model.Limits) != `{"rpm":2,"max_tokens":64}` {
		t.Fatalf("POST /models = %d %s", code, body)
	}
	if code, body := h.apiCall(t, http.MethodPost, "/models", `{"name":"bad","targets":[`+target+`],"limits":{"rpm":-1}}`); code != http.StatusBadRequest {
		t.Fatalf("POST /models with invalid limits = %d %s, want 400", code, body)
	}

	resp := h.messageFor(t, key.Key, "team-haiku", 65)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("over max_tokens: status = %d, want 400", resp.StatusCode)
	}
	if typ, msg := readError(t, resp); typ != "invalid_request_error" || msg != "max_tokens 65 exceeds model team-haiku's limit of 64" {
		t.Fatalf("error = %s: %s", typ, msg)
	}

	for i := 0; i < 2; i++ {
		resp := h.messageFor(t, key.Key, "team-haiku", 16)
		drain(resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("call %d status = %d, want 200", i+1, resp.StatusCode)
		}
	}
	resp = h.messageFor(t, key.Key, "team-haiku", 16)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("third call = %d Retry-After %q, want 429 / 1", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if typ, msg := readError(t, resp); typ != "rate_limit_error" || msg != "rate limit exceeded for model team-haiku (requests per minute)" {
		t.Fatalf("error = %s: %s", typ, msg)
	}
	if h.upstreamHits.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2", h.upstreamHits.Load())
	}

	var denied int
	if err := h.check.QueryRow(`SELECT count(*) FROM llm_calls WHERE requested_model = 'team-haiku' AND fallback_index = -1
		AND ((status_code = 429 AND error LIKE 'rpm_exceeded:%') OR (status_code = 400 AND error LIKE 'max_tokens_exceeded:%'))`).Scan(&denied); err != nil || denied != 2 {
		t.Fatalf("captured model denials = %d (%v), want 2", denied, err)
	}
	if got := h.health(t)["rpm_denials"]; got != float64(1) {
		t.Fatalf("health rpm_denials = %v, want 1", got)
	}

	// Another (unregistered) model is not limited by this row.
	resp = h.messageFor(t, key.Key, "claude-haiku-4-5", 4096)
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unregistered model status = %d, want 200", resp.StatusCode)
	}

	// Dropping the limits lifts them on the next call.
	if code, body := h.apiCall(t, http.MethodPut, "/models/"+model.ID, `{"name":"team-haiku","targets":[`+target+`]}`); code != http.StatusOK ||
		!strings.Contains(string(body), `"limits":null`) {
		t.Fatalf("PUT /models = %d %s", code, body)
	}
	resp = h.messageFor(t, key.Key, "team-haiku", 4096)
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after clearing the limits: status = %d, want 200", resp.StatusCode)
	}
}
