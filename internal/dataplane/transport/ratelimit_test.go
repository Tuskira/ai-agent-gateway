package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

type rateLimitedAuthenticator struct{}

func (rateLimitedAuthenticator) Name() string { return "rl" }
func (rateLimitedAuthenticator) Authenticate(context.Context, *http.Request) (*pkgauth.Principal, error) {
	return nil, &pkgauth.RateLimitedError{RetryAfter: 61 * time.Second}
}

func TestAuthMiddleware_RateLimitedIsJSONRPC429(t *testing.T) {
	called := false
	h := AuthMiddleware(rateLimitedAuthenticator{}, nil, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "61" {
		t.Errorf("Retry-After = %q, want 61", got)
	}
	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		Error   struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	if resp.JSONRPC != "2.0" || resp.Error.Code != mcp.ErrorCodeRateLimited {
		t.Errorf("body = %s, want JSON-RPC error %d", rec.Body.String(), mcp.ErrorCodeRateLimited)
	}
	if called {
		t.Error("rate-limited request reached the handler")
	}
}

func TestHandlerClientIP_IgnoresXFFUnlessResolverSet(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.RemoteAddr = "203.0.113.9:4000"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")

	if got := (&Handler{}).clientIP(r); got != "203.0.113.9" {
		t.Errorf("default clientIP = %q, want the TCP peer 203.0.113.9 (XFF must not be trusted as sent)", got)
	}
	h := &Handler{deps: Deps{ClientIP: func(*http.Request) string { return "198.51.100.7" }}}
	if got := h.clientIP(r); got != "198.51.100.7" {
		t.Errorf("resolver clientIP = %q", got)
	}
}
