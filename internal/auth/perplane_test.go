package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// oneKeyStore is an API-key store holding exactly one key; every other
// hash is unknown. The embedded interface is never called beyond these.
type oneKeyStore struct {
	store.APIKeyStore
	hash string
	key  *store.APIKey
}

func (s *oneKeyStore) GetByHash(_ context.Context, hash string) (*store.APIKey, error) {
	if hash == s.hash {
		cp := *s.key
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (s *oneKeyStore) TouchLastUsed(context.Context, string, time.Time) error { return nil }

func bearerFrom(ip, key string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = ip + ":1"
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

// planes builds the three per-plane authenticators the way main.go does:
// ONE shared apikey.Authenticator, one RateLimiter per plane.
func planes(t *testing.T, mutate func(*RateLimiterConfig)) (key string, api, mcp, llm pkgauth.Authenticator) {
	t.Helper()
	plaintext, hash, prefix, err := apikey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	st := &oneKeyStore{hash: hash, key: &store.APIKey{
		ID: "k1", TenantID: "t1", Role: "admin", KeyHash: hash, KeyPrefix: prefix, CreatedAt: time.Now(),
	}}
	shared := apikey.New(st, apikey.Options{})
	clock := &manualClock{t: time.Now()}
	mk := func() *RateLimiter {
		return newTestRateLimiter(t, clock, func(c *RateLimiterConfig) {
			c.MaxFailures = 3
			c.CredMaxFailures = 100
			if mutate != nil {
				mutate(c)
			}
		})
	}
	return plaintext,
		RateLimitedAuthenticator(shared, mk()),
		PlaneRateLimitedAuthenticator(shared, mk()),
		PlaneRateLimitedAuthenticator(shared, mk())
}

func TestPerPlane_IPLockoutIsIndependent(t *testing.T) {
	good, api, mcp, llm := planes(t, nil)
	ctx := context.Background()
	const ip = "203.0.113.5"
	const bad = "gk_not_a_real_key"

	for i := 0; i < 3; i++ {
		if _, err := mcp.Authenticate(ctx, bearerFrom(ip, bad)); !errors.Is(err, pkgauth.ErrInvalid) {
			t.Fatalf("mcp bad attempt %d: %v", i, err)
		}
	}
	// MCP is locked for this IP, even with a good key.
	if _, err := mcp.Authenticate(ctx, bearerFrom(ip, good)); !errors.Is(err, pkgauth.ErrRateLimited) {
		t.Fatalf("mcp good key after lockout: err = %v, want ErrRateLimited", err)
	}
	// The same IP still authenticates on the LLM and API planes.
	if _, err := llm.Authenticate(ctx, bearerFrom(ip, good)); err != nil {
		t.Errorf("llm good key from MCP-locked IP: %v", err)
	}
	if _, err := api.Authenticate(ctx, bearerFrom(ip, good)); err != nil {
		t.Errorf("api good key from MCP-locked IP: %v", err)
	}

	// Failures do not add up across planes: 2 on llm + 2 on api never lock either.
	const ip3 = "198.51.100.7"
	for i := 0; i < 2; i++ {
		_, _ = llm.Authenticate(ctx, bearerFrom(ip3, bad))
		_, _ = api.Authenticate(ctx, bearerFrom(ip3, bad))
	}
	for name, a := range map[string]pkgauth.Authenticator{"llm": llm, "api": api} {
		if _, err := a.Authenticate(ctx, bearerFrom(ip3, good)); err != nil {
			t.Errorf("%s: 2 failures per plane locked the IP: %v", name, err)
		}
	}

	// And the reverse: locking LLM leaves MCP usable for that IP.
	const ip2 = "192.0.2.44"
	for i := 0; i < 3; i++ {
		_, _ = llm.Authenticate(ctx, bearerFrom(ip2, bad))
	}
	if _, err := llm.Authenticate(ctx, bearerFrom(ip2, good)); !errors.Is(err, pkgauth.ErrRateLimited) {
		t.Fatalf("llm good key after lockout: err = %v, want ErrRateLimited", err)
	}
	if _, err := mcp.Authenticate(ctx, bearerFrom(ip2, good)); err != nil {
		t.Errorf("mcp good key from LLM-locked IP: %v", err)
	}
}

func TestPerPlane_CredentialPrefixLockoutIsIndependent(t *testing.T) {
	good, _, mcp, llm := planes(t, func(c *RateLimiterConfig) {
		c.MaxFailures = 1000 // keep the per-IP lockout out of this test
		c.CredMaxFailures = 2
	})
	ctx := context.Background()
	// Wrong keys sharing the good key's prefix (only the last char differs).
	for _, n := range []string{"X", "Y"} {
		_, _ = mcp.Authenticate(ctx, bearerFrom("203.0.113.5", good[:len(good)-1]+n))
	}
	// The prefix is now locked on MCP: a cold-cache good key is refused there...
	if _, err := mcp.Authenticate(ctx, bearerFrom("198.51.100.9", good)); !errors.Is(err, pkgauth.ErrRateLimited) {
		t.Fatalf("mcp good key under prefix lock: err = %v, want ErrRateLimited", err)
	}
	// ...but the LLM plane's prefix counter is untouched.
	if _, err := llm.Authenticate(ctx, bearerFrom("198.51.100.9", good)); err != nil {
		t.Errorf("llm good key with MCP-locked prefix: %v", err)
	}
}
