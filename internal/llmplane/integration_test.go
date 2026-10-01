package llmplane_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/clientip"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/llmplane"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	pkgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// captureRec is a test Recorder that records the last LLMCall for assertions.
// The router calls Record (with a fresh background context) before it returns.
type captureRec struct {
	mu   sync.Mutex
	last *pkgsink.LLMCall
}

func (c *captureRec) Record(_ context.Context, l *pkgsink.LLMCall) error {
	c.mu.Lock()
	c.last = l
	c.mu.Unlock()
	return nil
}
func (c *captureRec) get() *pkgsink.LLMCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// principalMW stands in for the gateway's real auth middleware: it puts a
// resolved Principal on the request context ahead of the plane.
func principalMW(next http.Handler, p *pkgauth.Principal) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(pkgauth.WithPrincipal(r.Context(), p)))
	})
}

// TestIntegration_IdentityAndCapture verifies the integration seams onto
// Avinash's foundation: (1) the caller identity for capture comes from the
// auth-layer Principal on the context, (2) the client's own key rides along to
// the upstream (BYOK), (3) usage + provider request-id are captured into
// pkg/sink.LLMCall, (4) the session tag is read and stripped, and (5) the
// client secret is masked in the captured headers.
func TestIntegration_IdentityAndCapture(t *testing.T) {
	var gotAPIKey, gotSession string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotSession = r.Header.Get("X-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req_upstream_123")
		_, _ = io.WriteString(w, `{"id":"msg_1","stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":6234}}`)
	}))
	defer upstream.Close()

	cap := &captureRec{}
	core, err := llmplane.Handler(llmplane.Config{
		UpstreamBaseURL:         upstream.URL,
		MaxRequestBytes:         1 << 20,
		MaxConcurrentPerTenant:  8,
		StoreBodies:             true,
		MaxCaptureRequestBytes:  4096,
		MaxCaptureResponseBytes: 4096,
		Authorizer:              pkgauth.NewRoleAuthorizer(),
		// 127.0.0.1 (the test client) is the trusted proxy: the right-most
		// untrusted X-Forwarded-For entry is the caller, not the spoofable
		// left one.
		ClientIP: clientip.MustNew("127.0.0.0/8").IP,
	}, cap)
	if err != nil {
		t.Fatalf("build llm plane: %v", err)
	}

	handler := principalMW(core, &pkgauth.Principal{
		Subject: "user@acme", TenantID: "tenant-x", KeyID: "key-abc", Roles: []string{"agent"},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Session-Id", "sess-777")
	req.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.9")
	req.Header.Set("X-Bedrock-Region", "us-east-1")
	req.Header.Set("x-api-key", "sk-ant-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, respBody)
	}

	// BYOK: the client's own key rode along; the session tag was stripped.
	if gotAPIKey != "sk-ant-secret" {
		t.Errorf("upstream x-api-key = %q, want it to ride along", gotAPIKey)
	}
	if gotSession != "" {
		t.Errorf("session tag leaked upstream: %q", gotSession)
	}

	call := cap.get()
	if call == nil {
		t.Fatal("no LLMCall captured")
	}
	// Identity comes from the auth-layer Principal, not from client headers.
	if call.TenantID != "tenant-x" || call.Principal != "user@acme" || call.KeyID != "key-abc" {
		t.Errorf("identity = {%q,%q,%q}, want {tenant-x,user@acme,key-abc}", call.TenantID, call.Principal, call.KeyID)
	}
	if call.SessionID != "sess-777" {
		t.Errorf("session = %q, want sess-777", call.SessionID)
	}
	if call.ClientIP != "203.0.113.9" {
		t.Errorf("client ip = %q, want 203.0.113.9", call.ClientIP)
	}
	if call.Provider != "anthropic" || call.Model != "claude-haiku-4-5" {
		t.Errorf("provider/model = %q/%q", call.Provider, call.Model)
	}
	if call.InputTokens != 11 || call.OutputTokens != 7 || call.CacheReadTokens != 6234 {
		t.Errorf("usage = in%d out%d cr%d, want 11/7/6234", call.InputTokens, call.OutputTokens, call.CacheReadTokens)
	}
	if call.ProviderRequestID != "req_upstream_123" {
		t.Errorf("provider_request_id = %q", call.ProviderRequestID)
	}
	// The client secret must be masked in the captured headers (keys are in
	// http.Header canonical form).
	if v := call.Headers["X-Api-Key"]; v != "[masked]" {
		t.Errorf("captured X-Api-Key = %q, want [masked]", v)
	}
	if v := call.Headers["X-Bedrock-Region"]; v != "us-east-1" {
		t.Errorf("captured X-Bedrock-Region = %q, want the real value", v)
	}
	if len(call.Messages) == 0 {
		t.Error("messages not captured with store_bodies=true")
	}
}
