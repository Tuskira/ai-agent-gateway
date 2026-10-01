package secrets

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// fakeCredentialStore is a minimal, map-backed store.CredentialStore for
// exercising Service without a real database. It mirrors the semantics
// documented on store.CredentialStore (tenant-scoped, List never
// populates Ciphertext/Nonce, duplicate Create -> ErrConflict, missing row
// -> ErrNotFound).
type fakeCredentialStore struct {
	mu    sync.Mutex
	byKey map[string]*store.Credential // key: tenantID + "|" + name
	seq   int
}

func newFakeCredentialStore() *fakeCredentialStore {
	return &fakeCredentialStore{byKey: make(map[string]*store.Credential)}
}

func fakeKey(tenantID, name string) string { return tenantID + "|" + name }

func (f *fakeCredentialStore) Create(_ context.Context, c *store.Credential) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := fakeKey(c.TenantID, c.Name)
	if _, exists := f.byKey[key]; exists {
		return store.ErrConflict
	}

	f.seq++
	cp := *c
	cp.ID = "cred-" + strconv.Itoa(f.seq)
	cp.CreatedAt = time.Now().UTC()
	f.byKey[key] = &cp

	*c = cp
	return nil
}

func (f *fakeCredentialStore) Get(_ context.Context, tenantID, name string) (*store.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, ok := f.byKey[fakeKey(tenantID, name)]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (f *fakeCredentialStore) List(_ context.Context, tenantID string) ([]*store.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []*store.Credential
	for _, c := range f.byKey {
		if c.TenantID != tenantID {
			continue
		}
		// Metadata only: never leak Ciphertext/Nonce, matching the real
		// backend's List contract.
		meta := *c
		meta.Ciphertext = nil
		meta.Nonce = nil
		out = append(out, &meta)
	}
	return out, nil
}

func (f *fakeCredentialStore) Rotate(_ context.Context, tenantID, name string, ciphertext, nonce []byte, keyID string, fieldNames []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, ok := f.byKey[fakeKey(tenantID, name)]
	if !ok {
		return store.ErrNotFound
	}
	c.Ciphertext = ciphertext
	c.Nonce = nonce
	c.KeyID = keyID
	c.FieldNames = fieldNames
	now := time.Now().UTC()
	c.RotatedAt = &now
	return nil
}

func (f *fakeCredentialStore) Delete(_ context.Context, tenantID, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := fakeKey(tenantID, name)
	if _, ok := f.byKey[key]; !ok {
		return store.ErrNotFound
	}
	delete(f.byKey, key)
	return nil
}

var _ store.CredentialStore = (*fakeCredentialStore)(nil)

// ---------------------------------------------------------------------------
// Service tests
// ---------------------------------------------------------------------------

func newTestService(t *testing.T) (*Service, *fakeCredentialStore) {
	t.Helper()
	cs := newFakeCredentialStore()
	ring := testKeyRing(t)
	return NewService(cs, ring), cs
}

func TestService_CreateGetRoundTrip(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	payload := map[string]string{"client_id": "abc", "client_secret": "s3cr3t"}
	cred, err := svc.Create(ctx, "tenant-a", "okta-prod", "secret_store", payload, "avinash@tuskira.ai")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cred.ID == "" {
		t.Error("Create did not populate ID")
	}
	if cred.KeyID != "k1" {
		t.Errorf("KeyID = %q, want active key k1", cred.KeyID)
	}
	wantFields := []string{"client_id", "client_secret"}
	if !equalStrings(cred.FieldNames, wantFields) {
		t.Errorf("FieldNames = %v, want sorted %v", cred.FieldNames, wantFields)
	}

	got, err := svc.Get(ctx, "tenant-a", "okta-prod")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got["client_secret"] != "s3cr3t" || got["client_id"] != "abc" {
		t.Errorf("Get = %v, want round-tripped payload", got)
	}
}

func TestService_Create_DuplicateConflict(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	payload := map[string]string{"a": "1"}
	if _, err := svc.Create(ctx, "tenant-a", "dup", "x", payload, "u"); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := svc.Create(ctx, "tenant-a", "dup", "x", payload, "u")
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("second Create error = %v, want wraps store.ErrConflict", err)
	}
}

func TestService_GetField(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "tenant-a", "okta", "t", map[string]string{"client_secret": "s3cr3t"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	v, err := svc.GetField(ctx, "tenant-a", "okta", "client_secret")
	if err != nil {
		t.Fatalf("GetField: %v", err)
	}
	if v != "s3cr3t" {
		t.Errorf("GetField = %q, want s3cr3t", v)
	}
}

func TestService_GetField_NotFound(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "tenant-a", "okta", "t", map[string]string{"client_secret": "s3cr3t"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := svc.GetField(ctx, "tenant-a", "okta", "does-not-exist")
	if !errors.Is(err, ErrFieldNotFound) {
		t.Errorf("GetField(missing field) error = %v, want wraps ErrFieldNotFound", err)
	}
}

func TestService_TenantIsolation(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "tenant-a", "okta", "t", map[string]string{"x": "1"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := svc.Get(ctx, "tenant-b", "okta"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(tenant-b, tenant-a's credential) error = %v, want ErrNotFound", err)
	}
}

func TestService_Rotate(t *testing.T) {
	svc, cs := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "tenant-a", "okta", "t", map[string]string{"client_secret": "old"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Rotate(ctx, "tenant-a", "okta", map[string]string{"client_secret": "new"}); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	v, err := svc.GetField(ctx, "tenant-a", "okta", "client_secret")
	if err != nil {
		t.Fatalf("GetField after rotate: %v", err)
	}
	if v != "new" {
		t.Errorf("GetField after rotate = %q, want new", v)
	}

	row, err := cs.Get(ctx, "tenant-a", "okta")
	if err != nil {
		t.Fatalf("raw Get after rotate: %v", err)
	}
	if row.RotatedAt == nil {
		t.Error("Rotate did not set RotatedAt")
	}
}

func TestService_Delete(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "tenant-a", "okta", "t", map[string]string{"x": "1"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Delete(ctx, "tenant-a", "okta"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, "tenant-a", "okta"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get after Delete error = %v, want ErrNotFound", err)
	}
}

func TestService_List_NeverDecrypts(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "tenant-a", "okta", "t", map[string]string{"client_secret": "s3cr3t"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	list, err := svc.List(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List = %d rows, want 1", len(list))
	}
	if len(list[0].Ciphertext) != 0 || len(list[0].Nonce) != 0 {
		t.Error("List leaked ciphertext/nonce")
	}
	if !equalStrings(list[0].FieldNames, []string{"client_secret"}) {
		t.Errorf("List FieldNames = %v, want [client_secret]", list[0].FieldNames)
	}
}

func TestService_ReEncryptAll(t *testing.T) {
	svc, cs := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "tenant-a", "okta", "t", map[string]string{"client_secret": "s3cr3t"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate the master key having been rotated: flip the ring's active
	// key to k2 (already present in testKeyRing) without touching the
	// already-persisted row, which is still under k1.
	svc.ring.ActiveKeyID = "k2"

	rewritten, err := svc.ReEncryptAll(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("ReEncryptAll: %v", err)
	}
	if rewritten != 1 {
		t.Errorf("ReEncryptAll rewrote %d rows, want 1", rewritten)
	}

	row, err := cs.Get(ctx, "tenant-a", "okta")
	if err != nil {
		t.Fatalf("raw Get: %v", err)
	}
	if row.KeyID != "k2" {
		t.Errorf("KeyID after ReEncryptAll = %q, want k2", row.KeyID)
	}

	v, err := svc.GetField(ctx, "tenant-a", "okta", "client_secret")
	if err != nil {
		t.Fatalf("GetField after ReEncryptAll: %v", err)
	}
	if v != "s3cr3t" {
		t.Errorf("GetField after ReEncryptAll = %q, want s3cr3t", v)
	}

	// A second run is a no-op: every row is already under the active key.
	rewritten, err = svc.ReEncryptAll(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("ReEncryptAll (second run): %v", err)
	}
	if rewritten != 0 {
		t.Errorf("ReEncryptAll (second run) rewrote %d rows, want 0", rewritten)
	}
}

func TestService_Fetcher_Caches(t *testing.T) {
	svc, cs := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "tenant-a", "okta", "t", map[string]string{"client_secret": "s3cr3t"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fetch := svc.Fetcher()
	v1, err := fetch(ctx, "tenant-a", "okta", "client_secret")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if v1 != "s3cr3t" {
		t.Fatalf("fetch = %q, want s3cr3t", v1)
	}

	// Mutate the row directly (bypassing Service, so no cache
	// invalidation happens) -- the cache should still serve the old
	// value.
	row, err := cs.Get(ctx, "tenant-a", "okta")
	if err != nil {
		t.Fatalf("raw Get: %v", err)
	}
	ciphertext, nonce, err := svc.ring.Encrypt(svc.ring.ActiveKeyID, []byte(`{"client_secret":"changed"}`))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := cs.Rotate(ctx, "tenant-a", "okta", ciphertext, nonce, row.KeyID, row.FieldNames); err != nil {
		t.Fatalf("raw Rotate: %v", err)
	}

	v2, err := fetch(ctx, "tenant-a", "okta", "client_secret")
	if err != nil {
		t.Fatalf("fetch (cached): %v", err)
	}
	if v2 != "s3cr3t" {
		t.Errorf("fetch (cached) = %q, want cached value s3cr3t", v2)
	}
}

func TestService_Fetcher_TTLExpires(t *testing.T) {
	svc, cs := newTestService(t)
	svc.cacheTTL = 10 * time.Millisecond
	ctx := context.Background()

	if _, err := svc.Create(ctx, "tenant-a", "okta", "t", map[string]string{"client_secret": "s3cr3t"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fetch := svc.Fetcher()
	if _, err := fetch(ctx, "tenant-a", "okta", "client_secret"); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	row, err := cs.Get(ctx, "tenant-a", "okta")
	if err != nil {
		t.Fatalf("raw Get: %v", err)
	}
	ciphertext, nonce, err := svc.ring.Encrypt(svc.ring.ActiveKeyID, []byte(`{"client_secret":"changed"}`))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := cs.Rotate(ctx, "tenant-a", "okta", ciphertext, nonce, row.KeyID, row.FieldNames); err != nil {
		t.Fatalf("raw Rotate: %v", err)
	}

	time.Sleep(20 * time.Millisecond)

	v, err := fetch(ctx, "tenant-a", "okta", "client_secret")
	if err != nil {
		t.Fatalf("fetch after TTL expiry: %v", err)
	}
	if v != "changed" {
		t.Errorf("fetch after TTL expiry = %q, want fresh value \"changed\"", v)
	}
}

func TestService_Invalidate(t *testing.T) {
	svc, cs := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "tenant-a", "okta", "t", map[string]string{"client_secret": "s3cr3t"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fetch := svc.Fetcher()
	if _, err := fetch(ctx, "tenant-a", "okta", "client_secret"); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	row, err := cs.Get(ctx, "tenant-a", "okta")
	if err != nil {
		t.Fatalf("raw Get: %v", err)
	}
	ciphertext, nonce, err := svc.ring.Encrypt(svc.ring.ActiveKeyID, []byte(`{"client_secret":"changed"}`))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := cs.Rotate(ctx, "tenant-a", "okta", ciphertext, nonce, row.KeyID, row.FieldNames); err != nil {
		t.Fatalf("raw Rotate: %v", err)
	}

	svc.Invalidate("tenant-a", "okta")

	v, err := fetch(ctx, "tenant-a", "okta", "client_secret")
	if err != nil {
		t.Fatalf("fetch after Invalidate: %v", err)
	}
	if v != "changed" {
		t.Errorf("fetch after Invalidate = %q, want fresh value \"changed\"", v)
	}
}

func TestService_InvalidateTenant(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "tenant-a", "okta", "t", map[string]string{"x": "1"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Create(ctx, "tenant-a", "aws", "t", map[string]string{"y": "2"}, "u"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fetch := svc.Fetcher()
	if _, err := fetch(ctx, "tenant-a", "okta", "x"); err != nil {
		t.Fatalf("fetch okta: %v", err)
	}
	if _, err := fetch(ctx, "tenant-a", "aws", "y"); err != nil {
		t.Fatalf("fetch aws: %v", err)
	}

	svc.cacheMu.Lock()
	before := len(svc.cache)
	svc.cacheMu.Unlock()
	if before != 2 {
		t.Fatalf("expected 2 cache entries before InvalidateTenant, got %d", before)
	}

	svc.InvalidateTenant("tenant-a")

	svc.cacheMu.Lock()
	after := len(svc.cache)
	svc.cacheMu.Unlock()
	if after != 0 {
		t.Errorf("InvalidateTenant left %d cache entries, want 0", after)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
