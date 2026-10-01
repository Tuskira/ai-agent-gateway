//go:build e2e

package e2e

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/capture"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/llmplane"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/pricing"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink/stdout"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// ---------------------------------------------------------------------------
// LLM-plane harness
// ---------------------------------------------------------------------------

// llmHarness is the LLM plane on a real socket, authenticating real Postgres
// API keys in front of a stub provider. The unit tests in internal/llmplane put
// the Principal on the context by hand; this one goes through the whole chain —
// an `gk_` key hashed in Postgres, internal/auth.Middleware resolving it, and
// the plane's own llm.access check — which is what catches the authorizer being
// unwired between cmd/gateway and the plane.
type llmHarness struct {
	baseURL string

	upstreamHits *atomic.Int32
	agentKey     string // role "agent": holds llm.* -> llm.access
	noLLMKey     string // custom role from auth.roles with no llm permission
}

// newLLMHarness builds the LLM plane the way cmd/gateway/main.go does. It is a
// mirror, not a call: main.go's wiring lives in run() and the unexported
// llmConfig, neither reachable from a test binary. Kept in step with
// cmd/gateway/main.go lines 175-182 (authorizer = the built-in roles with
// cfg.Auth.Roles merged over them, then internal/auth.Middleware), 289-323 (the
// llm_proxy block: capture Recorder, llmplane.Handler, the mux with an
// unauthenticated /health beside authMiddleware(core)), and 503-524 (llmConfig,
// mapped below).
func newLLMHarness(t *testing.T) *llmHarness {
	t.Helper()

	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping the LLM end-to-end test")
	}
	ctx := context.Background()

	// A stub provider so the test never dials a real one: any 200 proves the
	// request got past auth, which is all these assertions are about.
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_e2e","stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7}}`)
	}))
	t.Cleanup(upstream.Close)

	st := newIsolatedStore(t, rawURL)

	tenant := &store.Tenant{Slug: "e2e-llm-" + uuid.NewString()[:8], Name: "E2E LLM Tenant"}
	if err := st.Tenants().Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// cfg.Auth.Roles as an operator would write it: a custom role that grants
	// real permissions but none in the llm namespace. NewRoleAuthorizer's
	// built-ins (admin, agent) are seeded first and the custom roles merged
	// over them -- cmd/gateway/main.go lines 175-181.
	cfg := config.Default()
	cfg.Auth.Roles = map[string][]string{"reporting": {"mcp.*", "*.read"}}
	authorizer := pkgauth.NewRoleAuthorizer()
	for name, patterns := range cfg.Auth.Roles {
		authorizer.Rules[name] = patterns
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authenticator := apikey.New(st.APIKeys(), apikey.Options{})
	authMiddleware := internalauth.Middleware(authenticator, logger)

	// Durable capture is the Postgres sink in main.go (buildBatchSink); this
	// test asserts nothing about the stored row, so a discarding sink keeps it
	// to the auth path. pkg/sink/postgres has its own coverage.
	logSink := stdout.New(io.Discard)
	t.Cleanup(func() { _ = logSink.Close() })
	recorder := capture.NewRecorder(logSink, nil, nil, 0)

	core, err := llmplane.Handler(llmplane.Config{
		UpstreamBaseURL:         upstream.URL,
		MaxRequestBytes:         cfg.LLMProxy.Limits.MaxRequestBytes,
		MaxStreamDuration:       cfg.LLMProxy.Limits.MaxStreamDuration,
		MaxConcurrentPerTenant:  cfg.LLMProxy.Limits.MaxConcurrentPerTenant,
		BedrockEnabled:          cfg.LLMProxy.Bedrock.Enabled,
		BedrockRegion:           cfg.LLMProxy.Bedrock.Region,
		BedrockCredentialMode:   cfg.LLMProxy.Bedrock.CredentialMode,
		BedrockRoleAccounts:     cfg.LLMProxy.Bedrock.AllowedRoleAccounts,
		Authorizer:              authorizer,
		OpenAIEnabled:           cfg.LLMProxy.Providers.OpenAI.Enabled,
		OpenAIBaseURL:           cfg.LLMProxy.Providers.OpenAI.BaseURL,
		OpenAIStreamUsage:       cfg.LLMProxy.Providers.OpenAI.InjectStreamUsage,
		GeminiEnabled:           cfg.LLMProxy.Providers.Gemini.Enabled,
		GeminiBaseURL:           cfg.LLMProxy.Providers.Gemini.BaseURL,
		StoreBodies:             cfg.LLMProxy.Capture.StoreBodies,
		MaxCaptureRequestBytes:  cfg.LLMProxy.Capture.MaxRequestBytes,
		MaxCaptureResponseBytes: cfg.LLMProxy.Capture.MaxResponseBytes,
		Pricing:                 pricing.Default,
	}, recorder)
	if err != nil {
		t.Fatalf("build llm plane: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok","plane":"llm"}`)
	})
	mux.Handle("/", authMiddleware(core))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})

	return &llmHarness{
		baseURL:      "http://" + ln.Addr().String(),
		upstreamHits: &hits,
		agentKey:     newAPIKey(t, st, tenant.ID, "e2e-agent", "agent"),
		noLLMKey:     newAPIKey(t, st, tenant.ID, "e2e-reporting", "reporting"),
	}
}

// newAPIKey mints one gateway key exactly as `gateway bootstrap-key` does --
// the plaintext exists only here, the row holds only the hash -- and returns
// the plaintext.
func newAPIKey(t *testing.T, st store.Store, tenantID, name, role string) string {
	t.Helper()
	plaintext, hash, prefix, err := apikey.Generate()
	if err != nil {
		t.Fatalf("generate api key: %v", err)
	}
	key := &store.APIKey{
		TenantID: tenantID, Name: name, Role: role,
		KeyHash: hash, KeyPrefix: prefix, CreatedBy: "e2e",
	}
	if err := st.APIKeys().Create(context.Background(), key); err != nil {
		t.Fatalf("create %q api key: %v", role, err)
	}
	return plaintext
}

// post sends one Messages request as key ("" = no credential at all) and
// returns the status and body.
func (h *llmHarness) post(t *testing.T, key string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.baseURL+"/v1/messages",
		strings.NewReader(`{"model":"claude-haiku-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// ---------------------------------------------------------------------------
// the tests
// ---------------------------------------------------------------------------

// The llm.access permission is enforced by the plane itself, over the real auth
// chain: a key whose role grants no llm permission is 403 and never reaches the
// provider, while the built-in agent role passes. Unwiring Config.Authorizer
// (which used to mean "skip the check") fails this test rather than silently
// opening every provider route to every authenticated key.
func TestLLMPlaneRequiresLLMAccessEndToEnd(t *testing.T) {
	h := newLLMHarness(t)

	t.Run("health needs no credential", func(t *testing.T) {
		resp, err := http.Get(h.baseURL + "/health")
		if err != nil {
			t.Fatalf("GET /health: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /health = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("no credential is 401", func(t *testing.T) {
		if status, body := h.post(t, ""); status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 (%s)", status, body)
		}
	})

	t.Run("a custom role without llm.access is 403", func(t *testing.T) {
		before := h.upstreamHits.Load()
		status, body := h.post(t, h.noLLMKey)
		if status != http.StatusForbidden {
			t.Errorf("status = %d, want 403 (%s)", status, body)
		}
		if !strings.Contains(body, llmplane.PermissionLLMAccess) {
			t.Errorf("403 body does not name the missing permission: %s", body)
		}
		if got := h.upstreamHits.Load(); got != before {
			t.Errorf("the denied request still reached the provider (%d -> %d)", before, got)
		}
	})

	t.Run("the agent role gets past auth", func(t *testing.T) {
		status, body := h.post(t, h.agentKey)
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			t.Fatalf("agent key rejected: %d (%s)", status, body)
		}
		if status != http.StatusOK {
			t.Errorf("status = %d, want 200 from the stub provider (%s)", status, body)
		}
		if h.upstreamHits.Load() == 0 {
			t.Error("the allowed request never reached the provider")
		}
	})
}
