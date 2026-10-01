package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// fakeClock is an injectable internalauth.Clock for this test so the
// lockout window/duration can be advanced deterministically instead of
// racing real time.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// keyedAuthenticator fails Authenticate for any bearer credential other
// than validKey -- enough to exercise RateLimitedAuthenticator's
// failure/success reporting without a real apikey store.
type keyedAuthenticator struct{ validKey string }

func (k *keyedAuthenticator) Name() string { return "keyed" }

func (k *keyedAuthenticator) Authenticate(_ context.Context, r *http.Request) (*pkgauth.Principal, error) {
	const prefix = "Bearer "
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, prefix) {
		return nil, pkgauth.ErrNoCredential
	}
	if strings.TrimPrefix(authz, prefix) != k.validKey {
		return nil, pkgauth.ErrInvalid
	}
	return &pkgauth.Principal{Subject: "u1", TenantID: "t1", KeyID: "k1"}, nil
}

// TestNewRouter_RateLimit_LockoutAfterMaxFailures is the end-to-end
// scenario from the task: 10 bad keys from the same IP each get a normal
// 401, the 11th request -- even carrying a VALID key from that same IP --
// gets 429 with Retry-After while the lockout is active, /api/v1/health
// stays reachable throughout, and the valid key succeeds again once the
// lockout expires.
func TestNewRouter_RateLimit_LockoutAfterMaxFailures(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	rl := internalauth.NewRateLimiter(internalauth.RateLimiterConfig{
		Enabled:     true,
		MaxFailures: 10,
		Window:      time.Minute,
		Lockout:     5 * time.Minute,
		Clock:       clock,
		// RequestsPerMinute left at 0 (disabled) so this test isolates
		// the failure lockout from the general per-IP cap.
	})
	t.Cleanup(rl.Close)

	inner := &keyedAuthenticator{validKey: "gk_good"}
	h := NewRouter(Deps{
		ServiceVersion: "1.2.3",
		Authenticator:  internalauth.RateLimitedAuthenticator(inner, rl),
		RateLimiter:    rl,
	})

	authedReq := func(key string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		return r
	}

	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, authedReq("gk_bad"))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("request %d (bad key) = %d, want 401", i+1, w.Code)
		}
	}

	// 11th request: even the VALID key is rejected while locked out.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedReq("gk_good"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("11th request (valid key, should be locked out) = %d, want 429", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Error("429 response missing Retry-After header")
	}

	// A 12th request (another bad key) is also 429, not a fresh 401 --
	// the lockout blocks the whole IP, not just repeats of a bad key.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, authedReq("gk_bad"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("12th request (bad key, still locked out) = %d, want 429", w.Code)
	}

	// /api/v1/health stays reachable during the lockout (exempt from the
	// lockout, though it still counts toward the disabled-here general cap).
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/health during lockout = %d, want 200", w.Code)
	}

	// After the lockout window elapses, the valid key succeeds again.
	clock.Advance(5*time.Minute + time.Second)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, authedReq("gk_good"))
	if w.Code != http.StatusOK {
		t.Fatalf("valid key after lockout expiry = %d, want 200", w.Code)
	}
}
