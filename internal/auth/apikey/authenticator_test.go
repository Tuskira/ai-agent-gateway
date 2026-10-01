package apikey

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// fakeStore is a minimal in-memory store.APIKeyStore for testing the
// Authenticator without a real database.
type fakeStore struct {
	mu         sync.Mutex
	byHash     map[string]*store.APIKey
	getCalls   int
	touchCalls []touchRequest
	// touchCh, if non-nil, also receives every TouchLastUsed call so
	// tests can synchronize on the async background worker without
	// sleeping.
	touchCh chan touchRequest
}

func newFakeStore() *fakeStore {
	return &fakeStore{byHash: make(map[string]*store.APIKey)}
}

func (f *fakeStore) Create(_ context.Context, k *store.APIKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byHash[k.KeyHash] = k
	return nil
}

func (f *fakeStore) GetByHash(_ context.Context, hash string) (*store.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	k, ok := f.byHash[hash]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *k
	return &cp, nil
}

// GetByID is unused by Authenticator itself (only GetByHash is on its
// read path); it exists so fakeStore keeps satisfying store.APIKeyStore.
func (f *fakeStore) GetByID(_ context.Context, tenantID, id string) (*store.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.byHash {
		if k.ID == id && k.TenantID == tenantID {
			cp := *k
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) List(_ context.Context, _ string) ([]*store.APIKey, error) { return nil, nil }

func (f *fakeStore) Revoke(_ context.Context, _, _ string) error { return nil }

func (f *fakeStore) SetLimits(_ context.Context, _, _ string, _ *store.Limits) error { return nil }
func (f *fakeStore) SetProfile(_ context.Context, _, _ string, _ *string) error      { return nil }

func (f *fakeStore) TouchLastUsed(_ context.Context, id string, at time.Time) error {
	req := touchRequest{id: id, at: at}
	f.mu.Lock()
	f.touchCalls = append(f.touchCalls, req)
	f.mu.Unlock()
	if f.touchCh != nil {
		f.touchCh <- req
	}
	return nil
}

func (f *fakeStore) getCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls
}

func (f *fakeStore) touchCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.touchCalls)
}

// clock is a mutable, thread-safe time source for deterministic
// cache-expiry and touch-throttling tests.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(start time.Time) *clock { return &clock{now: start} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func bearerRequest(plaintext string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if plaintext != "" {
		r.Header.Set("Authorization", "Bearer "+plaintext)
	}
	return r
}

func TestAuthenticator_HappyPath(t *testing.T) {
	plaintext, hash, prefix, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	fs := newFakeStore()
	fs.byHash[hash] = &store.APIKey{
		ID: "key-1", TenantID: "tenant-1", Name: "test", Role: "admin",
		KeyHash: hash, KeyPrefix: prefix, CreatedAt: time.Now(),
	}

	a := New(fs, Options{})
	p, err := a.Authenticate(context.Background(), bearerRequest(plaintext))
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}

	if p.Subject != "key-1" || p.TenantID != "tenant-1" || p.KeyID != "key-1" {
		t.Errorf("Principal = %+v", p)
	}
	if len(p.Roles) != 1 || p.Roles[0] != "admin" {
		t.Errorf("Roles = %v, want [admin]", p.Roles)
	}
	if p.AuthMethod != "apikey" {
		t.Errorf("AuthMethod = %q, want apikey", p.AuthMethod)
	}
	if p.RawCredential != plaintext {
		t.Errorf("RawCredential = %q, want %q", p.RawCredential, plaintext)
	}
	if p.Email != "" {
		t.Errorf("Email = %q, want empty", p.Email)
	}
}

func TestAuthenticator_Unknown(t *testing.T) {
	fs := newFakeStore()
	a := New(fs, Options{})

	_, err := a.Authenticate(context.Background(), bearerRequest("gk_doesnotexist"))
	if !errors.Is(err, auth.ErrInvalid) {
		t.Fatalf("Authenticate() error = %v, want auth.ErrInvalid", err)
	}
}

func TestAuthenticator_Revoked(t *testing.T) {
	plaintext, hash, prefix, _ := Generate()
	fs := newFakeStore()
	revokedAt := time.Now().Add(-time.Minute)
	fs.byHash[hash] = &store.APIKey{
		ID: "key-1", TenantID: "t1", Role: "admin",
		KeyHash: hash, KeyPrefix: prefix, RevokedAt: &revokedAt,
	}

	a := New(fs, Options{})
	_, err := a.Authenticate(context.Background(), bearerRequest(plaintext))
	if !errors.Is(err, auth.ErrInvalid) {
		t.Fatalf("Authenticate() error = %v, want auth.ErrInvalid", err)
	}
}

func TestAuthenticator_Expired(t *testing.T) {
	plaintext, hash, prefix, _ := Generate()
	fs := newFakeStore()
	expiresAt := time.Now().Add(-time.Minute)
	fs.byHash[hash] = &store.APIKey{
		ID: "key-1", TenantID: "t1", Role: "admin",
		KeyHash: hash, KeyPrefix: prefix, ExpiresAt: &expiresAt,
	}

	a := New(fs, Options{})
	_, err := a.Authenticate(context.Background(), bearerRequest(plaintext))
	if !errors.Is(err, auth.ErrInvalid) {
		t.Fatalf("Authenticate() error = %v, want auth.ErrInvalid", err)
	}
}

func TestAuthenticator_NoHeader(t *testing.T) {
	fs := newFakeStore()
	a := New(fs, Options{})

	_, err := a.Authenticate(context.Background(), bearerRequest(""))
	if !errors.Is(err, auth.ErrNoCredential) {
		t.Fatalf("Authenticate() error = %v, want auth.ErrNoCredential", err)
	}
}

func TestAuthenticator_NonGKBearer(t *testing.T) {
	fs := newFakeStore()
	a := New(fs, Options{})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.x.y")

	_, err := a.Authenticate(context.Background(), r)
	if !errors.Is(err, auth.ErrNoCredential) {
		t.Fatalf("Authenticate() error = %v, want auth.ErrNoCredential (so chain tries JWT next)", err)
	}
	if fs.getCallCount() != 0 {
		t.Errorf("store was queried for a credential that isn't ours; getCalls = %d", fs.getCallCount())
	}
}

func TestAuthenticator_CacheHitAvoidsSecondStoreCall(t *testing.T) {
	plaintext, hash, prefix, _ := Generate()
	fs := newFakeStore()
	fs.byHash[hash] = &store.APIKey{ID: "key-1", TenantID: "t1", Role: "admin", KeyHash: hash, KeyPrefix: prefix}

	a := New(fs, Options{CacheTTL: time.Minute})

	for i := 0; i < 3; i++ {
		if _, err := a.Authenticate(context.Background(), bearerRequest(plaintext)); err != nil {
			t.Fatalf("Authenticate() [%d] error = %v", i, err)
		}
	}

	if got := fs.getCallCount(); got != 1 {
		t.Errorf("getCalls = %d, want 1 (subsequent lookups should hit the cache)", got)
	}
}

func TestAuthenticator_CacheExpiry(t *testing.T) {
	plaintext, hash, prefix, _ := Generate()
	fs := newFakeStore()
	fs.byHash[hash] = &store.APIKey{ID: "key-1", TenantID: "t1", Role: "admin", KeyHash: hash, KeyPrefix: prefix}

	clk := newClock(time.Now())
	a := New(fs, Options{CacheTTL: 10 * time.Second, Now: clk.Now})

	if _, err := a.Authenticate(context.Background(), bearerRequest(plaintext)); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got := fs.getCallCount(); got != 1 {
		t.Fatalf("getCalls after first call = %d, want 1", got)
	}

	// Still within TTL: cached.
	clk.Advance(5 * time.Second)
	if _, err := a.Authenticate(context.Background(), bearerRequest(plaintext)); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got := fs.getCallCount(); got != 1 {
		t.Fatalf("getCalls within TTL = %d, want 1", got)
	}

	// Past TTL: must re-query the store.
	clk.Advance(10 * time.Second)
	if _, err := a.Authenticate(context.Background(), bearerRequest(plaintext)); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got := fs.getCallCount(); got != 2 {
		t.Fatalf("getCalls past TTL = %d, want 2", got)
	}
}

func TestAuthenticator_Invalidate(t *testing.T) {
	plaintext, hash, prefix, _ := Generate()
	fs := newFakeStore()
	fs.byHash[hash] = &store.APIKey{ID: "key-1", TenantID: "t1", Role: "admin", KeyHash: hash, KeyPrefix: prefix}

	a := New(fs, Options{CacheTTL: time.Minute})

	if _, err := a.Authenticate(context.Background(), bearerRequest(plaintext)); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	a.Invalidate(hash)
	if _, err := a.Authenticate(context.Background(), bearerRequest(plaintext)); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}

	if got := fs.getCallCount(); got != 2 {
		t.Errorf("getCalls after Invalidate = %d, want 2 (cache should have been evicted)", got)
	}
}

func TestAuthenticator_TouchThrottling(t *testing.T) {
	plaintext, hash, prefix, _ := Generate()
	fs := newFakeStore()
	fs.touchCh = make(chan touchRequest, 4)
	fs.byHash[hash] = &store.APIKey{ID: "key-1", TenantID: "t1", Role: "admin", KeyHash: hash, KeyPrefix: prefix}

	clk := newClock(time.Now())
	a := New(fs, Options{TouchInterval: time.Minute, Now: clk.Now})

	if _, err := a.Authenticate(context.Background(), bearerRequest(plaintext)); err != nil {
		t.Fatalf("Authenticate() [1] error = %v", err)
	}
	select {
	case req := <-fs.touchCh:
		if req.id != "key-1" {
			t.Errorf("touch id = %q, want key-1", req.id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first touch")
	}

	// Second call, same (throttled) instant: must NOT enqueue another
	// touch. The throttle decision is synchronous within Authenticate,
	// so this is deterministic -- no need to race the background worker.
	if _, err := a.Authenticate(context.Background(), bearerRequest(plaintext)); err != nil {
		t.Fatalf("Authenticate() [2] error = %v", err)
	}
	select {
	case req := <-fs.touchCh:
		t.Fatalf("unexpected second touch within TouchInterval: %+v", req)
	case <-time.After(200 * time.Millisecond):
		// Expected: nothing arrived.
	}

	// Advance past TouchInterval: the next Authenticate should touch again.
	clk.Advance(2 * time.Minute)
	if _, err := a.Authenticate(context.Background(), bearerRequest(plaintext)); err != nil {
		t.Fatalf("Authenticate() [3] error = %v", err)
	}
	select {
	case <-fs.touchCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for touch after TouchInterval elapsed")
	}

	if got := fs.touchCallCount(); got != 2 {
		t.Errorf("touchCalls = %d, want 2", got)
	}
}
