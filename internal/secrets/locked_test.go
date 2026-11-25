package secrets

import (
	"context"
	"errors"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// A platform-locked credential is never decrypted for a caller whose ctx
// carries no locked access, whichever read path it takes.
func TestService_LockedCredentialNeedsLockedAccess(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.CreateLocked(ctx, "tenant-a", "plat", "generic", map[string]string{"token": "p-secret"}, "platform", true); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Get(ctx, "tenant-a", "plat"); !errors.Is(err, store.ErrLocked) {
		t.Errorf("Get without locked access = %v, want ErrLocked", err)
	}
	if _, err := svc.GetField(store.WithLockedAccess(ctx, false), "tenant-a", "plat", "token"); !errors.Is(err, store.ErrLocked) {
		t.Errorf("GetField with access=false = %v, want ErrLocked", err)
	}
	if _, err := svc.Fetcher()(ctx, "tenant-a", "plat", "token"); !errors.Is(err, store.ErrLocked) {
		t.Errorf("Fetcher without locked access = %v, want ErrLocked", err)
	}
	v, err := svc.GetField(store.WithLockedAccess(ctx, true), "tenant-a", "plat", "token")
	if err != nil || v != "p-secret" {
		t.Errorf("GetField with locked access = %q, %v", v, err)
	}
}

// The Fetcher cache keeps the locked flag with the value: a value cached
// for an allowed read is still refused to a caller without access, and a
// lock set through the service is enforced at once even on a cached value.
func TestService_FetcherCacheHonoursLock(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "tenant-a", "c", "generic", map[string]string{"token": "v1"}, "u"); err != nil {
		t.Fatal(err)
	}
	fetch := svc.Fetcher()
	if v, err := fetch(ctx, "tenant-a", "c", "token"); err != nil || v != "v1" {
		t.Fatalf("unlocked fetch = %q, %v", v, err)
	}
	if err := svc.SetLocked(ctx, "tenant-a", "c", true); err != nil {
		t.Fatal(err)
	}
	if _, err := fetch(ctx, "tenant-a", "c", "token"); !errors.Is(err, store.ErrLocked) {
		t.Errorf("fetch after lock = %v, want ErrLocked", err)
	}
	// Warm the cache with an allowed read, then read again without access.
	if v, err := fetch(store.WithLockedAccess(ctx, true), "tenant-a", "c", "token"); err != nil || v != "v1" {
		t.Fatalf("allowed fetch = %q, %v", v, err)
	}
	if _, err := fetch(ctx, "tenant-a", "c", "token"); !errors.Is(err, store.ErrLocked) {
		t.Errorf("cached fetch without access = %v, want ErrLocked", err)
	}
}

// Rotate and Delete pass their guard to the store: UnlockedOnly leaves a
// locked row as it was.
func TestService_GuardedWritesOnLockedRow(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.CreateLocked(ctx, "tenant-a", "plat", "generic", map[string]string{"token": "p"}, "platform", true); err != nil {
		t.Fatal(err)
	}
	if err := svc.Rotate(ctx, "tenant-a", "plat", map[string]string{"token": "evil"}, store.UnlockedOnly); !errors.Is(err, store.ErrLocked) {
		t.Errorf("Rotate(UnlockedOnly) = %v, want ErrLocked", err)
	}
	if err := svc.Delete(ctx, "tenant-a", "plat", store.UnlockedOnly); !errors.Is(err, store.ErrLocked) {
		t.Errorf("Delete(UnlockedOnly) = %v, want ErrLocked", err)
	}
	allowed := store.WithLockedAccess(ctx, true)
	if v, _ := svc.GetField(allowed, "tenant-a", "plat", "token"); v != "p" {
		t.Errorf("payload after refused rotate = %q, want p", v)
	}
	if err := svc.Rotate(ctx, "tenant-a", "plat", map[string]string{"token": "p2"}, store.AllowLocked); err != nil {
		t.Fatalf("Rotate(AllowLocked): %v", err)
	}
	if v, _ := svc.GetField(allowed, "tenant-a", "plat", "token"); v != "p2" {
		t.Errorf("payload after allowed rotate = %q, want p2", v)
	}
}

// secret_store reports the credential its config names, and resolves a
// locked one only when ctx allows it.
func TestSecretStore_LockedCredential(t *testing.T) {
	svc, _ := newTestService(t)
	p := NewSecretStoreProvider(svc)
	if got := p.ReferencedCredentials(map[string]any{"credential": "x", "field": "f"}); len(got) != 1 || got[0] != "x" {
		t.Errorf("ReferencedCredentials = %v", got)
	}
	if got := p.ReferencedCredentials(map[string]any{}); len(got) != 0 {
		t.Errorf("ReferencedCredentials(empty) = %v", got)
	}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "k", TenantID: "tenant-a"})
	if _, err := svc.CreateLocked(ctx, "tenant-a", "plat", "generic", map[string]string{"token": "p"}, "platform", true); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"credential": "plat", "field": "token"}
	if _, err := p.Resolve(ctx, cfg); !errors.Is(err, store.ErrLocked) {
		t.Errorf("Resolve without locked access = %v, want ErrLocked", err)
	}
	if v, err := p.Resolve(store.WithLockedAccess(ctx, true), cfg); err != nil || v != "p" {
		t.Errorf("Resolve with locked access = %q, %v", v, err)
	}
}

// external_service authenticates with a locked credential only through a
// locked source, and never borrows an allowance from ctx: an unlocked
// source's URL is the tenant's to change.
func TestExternalService_LockedAuthCredentialNeedsLockedSource(t *testing.T) {
	p, rs, svc := newExternalServiceFixture(t)
	ctx := context.Background()
	if err := svc.SetLocked(ctx, "tenant-a", "vault-auth", true); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"credential_id": "splunk-prod", "field": "token"}

	unlocked := sourceFor(rs, "tenant-a")
	if _, err := p.Resolve(resolveCtx("tenant-a", unlocked), cfg); !errors.Is(err, store.ErrLocked) {
		t.Errorf("Resolve through an unlocked source = %v, want ErrLocked", err)
	}
	// An allowance in ctx (as a platform-admin header would carry) is not
	// inherited by an unlocked source.
	if _, err := p.Resolve(store.WithLockedAccess(resolveCtx("tenant-a", unlocked), true), cfg); !errors.Is(err, store.ErrLocked) {
		t.Errorf("Resolve through an unlocked source with ctx allowance = %v, want ErrLocked", err)
	}
	if err := p.TestSource(ctx, *unlocked); !errors.Is(err, store.ErrLocked) {
		t.Errorf("TestSource of an unlocked source = %v, want ErrLocked", err)
	}
	if _, err := p.ListOptions(ctx, *unlocked, "credential_id"); !errors.Is(err, store.ErrLocked) {
		t.Errorf("ListOptions of an unlocked source = %v, want ErrLocked", err)
	}
	if n := rs.resolves.Load(); n != 0 {
		t.Errorf("the service was asked to resolve %d times through an unlocked source", n)
	}

	locked := sourceFor(rs, "tenant-a")
	locked.Locked = true
	if v, err := p.Resolve(resolveCtx("tenant-a", locked), cfg); err != nil || v != "secret-v1" {
		t.Errorf("Resolve through a locked source = %q, %v", v, err)
	}
	if err := p.TestSource(ctx, *locked); err != nil {
		t.Errorf("TestSource of a locked source: %v", err)
	}
}
