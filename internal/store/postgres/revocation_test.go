package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// TestRevocationNotifier exercises the LISTEN/NOTIFY path against a real
// Postgres (GATEWAY_TEST_DATABASE_URL, like TestConformance): Revoke
// publishes the key hash in its own transaction, a listener on a SEPARATE
// store (a different process in production) receives it, a miss publishes
// nothing, and a dropped listener connection surfaces as an error so the
// caller can reconnect.
func TestRevocationNotifier(t *testing.T) {
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping Postgres revocation test")
	}
	ctx := context.Background()

	writer := newIsolatedStore(t, rawURL)

	// A second, independent connection pool plays the other replica.
	otherDB, err := sql.Open("pgx", rawURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { otherDB.Close() })
	listener := postgres.New(otherDB)

	tn := &store.Tenant{Slug: "t-" + uuid.NewString()[:8], Name: "t"}
	if err := writer.Tenants().Create(ctx, tn); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	mk := func() *store.APIKey {
		k := &store.APIKey{TenantID: tn.ID, Name: "k", Role: "admin", KeyHash: "hash-" + uuid.NewString(), KeyPrefix: "gk_x"}
		if err := writer.APIKeys().Create(ctx, k); err != nil {
			t.Fatalf("create key: %v", err)
		}
		return k
	}
	k1, k2 := mk(), mk()

	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ready := make(chan struct{}, 4)
	events := make(chan string, 8)
	done := make(chan error, 1)
	go func() {
		done <- listener.ListenKeyRevocations(lctx, func() { ready <- struct{}{} }, func(h string) { events <- h })
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("listener never became ready")
	}

	// A revoke on the writer reaches the listener, carrying the key hash.
	if err := writer.APIKeys().Revoke(ctx, tn.ID, k1.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	select {
	case got := <-events:
		if got != k1.KeyHash {
			t.Errorf("notified hash = %q, want %q", got, k1.KeyHash)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revocation was not delivered to the other connection")
	}

	// The revoke itself still did its job.
	got, err := writer.APIKeys().GetByID(ctx, tn.ID, k1.ID)
	if err != nil || got.RevokedAt == nil {
		t.Fatalf("key not revoked: %v %+v", err, got)
	}

	// A revoke that matches nothing is ErrNotFound and publishes nothing.
	if err := writer.APIKeys().Revoke(ctx, tn.ID, uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoke of unknown id: err = %v, want ErrNotFound", err)
	}
	if err := writer.APIKeys().Revoke(ctx, "00000000-0000-0000-0000-000000000000", k2.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoke under wrong tenant: err = %v, want ErrNotFound", err)
	}
	select {
	case h := <-events:
		t.Fatalf("unexpected notification for a failed revoke: %q", h)
	case <-time.After(500 * time.Millisecond):
	}

	// Killing the listener's backend makes ListenKeyRevocations return an
	// error (the caller's cue to reconnect and flush).
	admin, err := sql.Open("pgx", rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, `
		SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE pid <> pg_backend_pid() AND query LIKE 'LISTEN gateway_key_revoked%'`); err != nil {
		t.Fatalf("terminate listener backend: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("listener returned nil after its connection was killed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("listener did not notice its dropped connection")
	}

	// A fresh subscription works again after the drop, and ctx cancel ends it cleanly.
	lctx2, cancel2 := context.WithCancel(ctx)
	done2 := make(chan error, 1)
	go func() {
		done2 <- listener.ListenKeyRevocations(lctx2, func() { ready <- struct{}{} }, func(h string) { events <- h })
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("re-subscribe never became ready")
	}
	if err := writer.APIKeys().Revoke(ctx, tn.ID, k2.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-events:
		if got != k2.KeyHash {
			t.Errorf("after reconnect hash = %q, want %q", got, k2.KeyHash)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery after reconnect")
	}
	cancel2()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not stop on ctx cancel")
	}
}
