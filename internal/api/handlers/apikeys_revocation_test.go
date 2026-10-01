package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

// TestAPIKeys_Revoke_InvalidatesAuthenticatorCacheImmediately pins the
// revocation-lag fix: APIKeys.Revoke must evict the just-revoked key's
// cached lookup from a live apikey.Authenticator (via Deps.KeyInvalidator)
// in the same request, so the very next Authenticate for that key fails
// even though CacheTTL (set here to a full minute, well past any test
// timeout) hasn't elapsed. Without the fix this test hangs on nothing --
// it fails immediately, because the stale cache entry keeps authenticating.
func TestAPIKeys_Revoke_InvalidatesAuthenticatorCacheImmediately(t *testing.T) {
	deps := newTestDeps()
	authn := apikey.New(deps.Store.APIKeys(), apikey.Options{CacheTTL: 60 * time.Second})
	deps.KeyInvalidator = authn
	h := APIKeys{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"ci-bot","role":"agent"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/api-keys", h.Create, createReq)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", w.Code, w.Body.String())
	}
	var created apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	authRequest := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+created.Key)
		return r
	}

	// Populate the Authenticator's cache (and prove the key is good).
	if _, err := authn.Authenticate(context.Background(), authRequest()); err != nil {
		t.Fatalf("pre-revoke Authenticate() error = %v", err)
	}

	revokeReq := withPrincipal(httptest.NewRequest(http.MethodDelete, "/api-keys/"+created.ID, nil), "tenant-a", "admin")
	w = serve(http.MethodDelete, "/api-keys/{id}", h.Revoke, revokeReq)
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, body = %s", w.Code, w.Body.String())
	}

	// Immediately after revoke -- no time advanced, CacheTTL nowhere
	// near elapsed -- the same Authenticator must already refuse the key.
	if _, err := authn.Authenticate(context.Background(), authRequest()); !errors.Is(err, pkgauth.ErrInvalid) {
		t.Fatalf("post-revoke Authenticate() error = %v, want ErrInvalid (cache should have been invalidated immediately, not after the 60s TTL)", err)
	}
}

// TestAPIKeys_Rotate_InvalidatesOldKeyImmediately is Rotate's counterpart:
// the old key must stop authenticating the instant Rotate returns, not
// after CacheTTL.
func TestAPIKeys_Rotate_InvalidatesOldKeyImmediately(t *testing.T) {
	deps := newTestDeps()
	authn := apikey.New(deps.Store.APIKeys(), apikey.Options{CacheTTL: 60 * time.Second})
	deps.KeyInvalidator = authn
	h := APIKeys{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"ci-bot","role":"agent"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/api-keys", h.Create, createReq)
	var original apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &original); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	oldAuthRequest := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+original.Key)
		return r
	}
	if _, err := authn.Authenticate(context.Background(), oldAuthRequest()); err != nil {
		t.Fatalf("pre-rotate Authenticate() error = %v", err)
	}

	rotateReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys/"+original.ID+"/rotate", nil), "tenant-a", "admin")
	w = serve(http.MethodPost, "/api-keys/{id}/rotate", h.Rotate, rotateReq)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, body = %s", w.Code, w.Body.String())
	}

	if _, err := authn.Authenticate(context.Background(), oldAuthRequest()); !errors.Is(err, pkgauth.ErrInvalid) {
		t.Fatalf("post-rotate old-key Authenticate() error = %v, want ErrInvalid (cache should have been invalidated immediately)", err)
	}
}

// TestAPIKeys_Revoke_NilKeyInvalidatorIsNoOp proves Deps.KeyInvalidator
// being nil (the default in most tests, and in any deployment that
// doesn't wire the api-key Authenticator through) doesn't panic and the
// revoke still succeeds.
func TestAPIKeys_Revoke_NilKeyInvalidatorIsNoOp(t *testing.T) {
	deps := newTestDeps() // KeyInvalidator left nil
	h := APIKeys{Deps: deps}

	createReq := withPrincipal(httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"ci-bot","role":"agent"}`)), "tenant-a", "admin")
	w := serve(http.MethodPost, "/api-keys", h.Create, createReq)
	var created apiKeyCreateView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	revokeReq := withPrincipal(httptest.NewRequest(http.MethodDelete, "/api-keys/"+created.ID, nil), "tenant-a", "admin")
	w = serve(http.MethodDelete, "/api-keys/{id}", h.Revoke, revokeReq)
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, body = %s", w.Code, w.Body.String())
	}
}
