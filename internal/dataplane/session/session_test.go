package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	pkgsession "github.com/Tuskira/tusk-ai-secured-gateway/pkg/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/memory"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)} }

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

// newManager builds a Manager over the memory driver, with the driver
// and the Manager on the same fake clock, so expiry can be tested
// without sleeping.
func newManager(t *testing.T, ttl time.Duration) (*session.Manager, *memory.Store, *clock) {
	t.Helper()
	clk := newClock()
	store := memory.NewWithClock(clk.Now)
	return session.NewManager(store, session.Options{TTL: ttl, Now: clk.Now}), store, clk
}

func TestCreateIssuesAResolvableSession(t *testing.T) {
	mgr, _, _ := newManager(t, time.Hour)
	ctx := context.Background()

	caps := map[string]json.RawMessage{"elicitation": json.RawMessage(`{}`), "roots": json.RawMessage(`{"listChanged":true}`)}
	sess, err := mgr.Create(ctx, "tenant-a", "key-1", mcp.ProtocolVersion, mcp.Implementation{Name: "claude-code", Version: "2"}, caps)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == "" {
		t.Fatal("Create issued an empty session id")
	}
	if sess.TenantID != "tenant-a" || sess.Principal != "key-1" || sess.ClientInfo.Name != "claude-code" || sess.ProtocolVersion != mcp.ProtocolVersion {
		t.Fatalf("session did not record the caller: %+v", sess)
	}

	got, err := mgr.Resolve(ctx, sess.ID, "tenant-a")
	if err != nil {
		t.Fatalf("Resolve(issued id) = %v", err)
	}
	if got.ID != sess.ID || got.ProtocolVersion != mcp.ProtocolVersion || got.ClientInfo != sess.ClientInfo {
		t.Fatalf("Resolve returned %+v, want the session Create issued", got)
	}
	if len(got.ClientCapabilities) != 2 || string(got.ClientCapabilities["roots"]) != `{"listChanged":true}` {
		t.Fatalf("Resolve returned capabilities %s, want what Create recorded", got.ClientCapabilities)
	}
}

func TestSessionIDsAreUnpredictable(t *testing.T) {
	// The MCP spec requires a cryptographically secure session id
	// because it is a replayable handle.
	mgr, _, _ := newManager(t, time.Hour)
	ctx := context.Background()

	seen := make(map[string]struct{}, 200)
	for range 200 {
		sess, err := mgr.Create(ctx, "tenant-a", "key-1", mcp.ProtocolVersion, mcp.Implementation{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(sess.ID) < 32 {
			t.Fatalf("session id %q is only %d characters", sess.ID, len(sess.ID))
		}
		if _, dup := seen[sess.ID]; dup {
			t.Fatalf("session id repeated: %q", sess.ID)
		}
		seen[sess.ID] = struct{}{}
	}
}

func TestResolveRejectsAnUnknownID(t *testing.T) {
	mgr, _, _ := newManager(t, time.Hour)

	_, err := mgr.Resolve(context.Background(), "never-issued", "tenant-a")
	if !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Resolve(unknown) = %v, want ErrNotFound", err)
	}
	if !errors.Is(err, pkgsession.ErrNotFound) {
		t.Fatal("the manager's ErrNotFound must be pkg/session's, so the transport needs one check")
	}
}

func TestResolveRejectsAnExpiredSessionAndDropsIt(t *testing.T) {
	mgr, store, clk := newManager(t, time.Hour)
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "tenant-a", "key-1", mcp.ProtocolVersion, mcp.Implementation{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	clk.Advance(time.Hour + time.Second)

	if _, err := mgr.Resolve(ctx, sess.ID, "tenant-a"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Resolve(expired) = %v, want ErrNotFound", err)
	}
	if store.Len() != 0 {
		t.Error("an expired session must be dropped when it is rejected")
	}
}

// The Manager decides expiry with its own clock even when the store
// would still return the record: a store may lag the gateway's clock.
func TestManagerExpiryDoesNotDependOnTheStoreClock(t *testing.T) {
	clk := newClock()
	// The store's clock is frozen at creation time; only the Manager's
	// advances.
	frozen := clk.Now()
	store := memory.NewWithClock(func() time.Time { return frozen })
	mgr := session.NewManager(store, session.Options{TTL: time.Hour, Now: clk.Now})
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "tenant-a", "key-1", mcp.ProtocolVersion, mcp.Implementation{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	if _, err := store.Get(ctx, sess.ID); err != nil {
		t.Fatalf("precondition: the store (frozen clock) should still return the record: %v", err)
	}
	if _, err := mgr.Resolve(ctx, sess.ID, "tenant-a"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Resolve(expired by the manager's clock) = %v, want ErrNotFound", err)
	}
}

func TestResolveSlidesTheIdleWindow(t *testing.T) {
	mgr, store, clk := newManager(t, time.Hour)
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "tenant-a", "key-1", mcp.ProtocolVersion, mcp.Implementation{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Used every 50 minutes, a session must survive indefinitely: the
	// TTL is an IDLE timeout, not an absolute lifetime.
	for range 5 {
		clk.Advance(50 * time.Minute)
		if _, err := mgr.Resolve(ctx, sess.ID, "tenant-a"); err != nil {
			t.Fatalf("Resolve after activity = %v", err)
		}
	}

	// And the slide reached the store: the persisted record carries the
	// new window, so another replica would agree.
	rec, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.LastSeenAt.Equal(clk.Now()) || !rec.ExpiresAt.Equal(clk.Now().Add(time.Hour)) {
		t.Fatalf("stored window = (%v, %v), want (%v, %v)", rec.LastSeenAt, rec.ExpiresAt, clk.Now(), clk.Now().Add(time.Hour))
	}
}

func TestResolveRejectsAnotherTenantsSession(t *testing.T) {
	// A session id is a bearer handle. Without this check a leaked id
	// from tenant A, presented with tenant B's key, would hand B the
	// backend handles A negotiated.
	mgr, _, _ := newManager(t, time.Hour)
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "tenant-a", "key-1", mcp.ProtocolVersion, mcp.Implementation{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.Resolve(ctx, sess.ID, "tenant-b"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Resolve(other tenant) = %v, want ErrNotFound", err)
	}
}

func TestBackendHandlesAreRecordedOnTheSession(t *testing.T) {
	mgr, _, _ := newManager(t, time.Hour)
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "tenant-a", "key-1", mcp.ProtocolVersion, mcp.Implementation{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	sess.SetBackend("conn-1", session.Backend{SessionID: "backend-abc", ProtocolVersion: mcp.ProtocolVersion})
	if err := mgr.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}

	got, err := mgr.Resolve(ctx, sess.ID, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if b := got.Backend("conn-1"); b.SessionID != "backend-abc" || b.ProtocolVersion != mcp.ProtocolVersion {
		t.Fatalf("backend handle = %+v", b)
	}
	if b := got.Backend("conn-unknown"); b.SessionID != "" {
		t.Fatalf("unknown connector yielded %+v, want the zero handle", b)
	}
}

// The memory driver hands out copies, so a handle that is set but never
// saved must NOT be visible to the next Resolve -- exactly what a
// serializing store would do. This is the test that catches a missing
// Save in the orchestrator.
func TestUnsavedBackendHandlesAreLost(t *testing.T) {
	mgr, _, _ := newManager(t, time.Hour)
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "tenant-a", "key-1", mcp.ProtocolVersion, mcp.Implementation{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.SetBackend("conn-1", session.Backend{SessionID: "never-saved", ProtocolVersion: mcp.ProtocolVersion})

	got, err := mgr.Resolve(ctx, sess.ID, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if b := got.Backend("conn-1"); b.ProtocolVersion != "" {
		t.Fatalf("an unsaved handle reached the store: %+v", b)
	}
}

func TestSaveIfChangedWritesOnlyWhenAHandleChanged(t *testing.T) {
	mgr, store, _ := newManager(t, time.Hour)
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "tenant-a", "key-1", mcp.ProtocolVersion, mcp.Implementation{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := mgr.Resolve(ctx, sess.ID, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}

	resolved.SetBackend("conn-1", session.Backend{SessionID: "rotated", ProtocolVersion: mcp.ProtocolVersion})
	if err := mgr.SaveIfChanged(ctx, resolved); err != nil {
		t.Fatal(err)
	}
	rec, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Backends["conn-1"].SessionID != "rotated" {
		t.Fatalf("SaveIfChanged did not persist the new handle: %+v", rec.Backends)
	}

	// Delete behind the manager's back: an unchanged session must not be
	// written again, so the record stays gone.
	if err := store.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	resolved.SetBackend("conn-1", session.Backend{SessionID: "rotated", ProtocolVersion: mcp.ProtocolVersion})
	if err := mgr.SaveIfChanged(ctx, resolved); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, sess.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("SaveIfChanged rewrote an unchanged session: Get = %v", err)
	}

	// Forgetting a handle (zeroing it) is a change too.
	resolved.SetBackend("conn-1", session.Backend{})
	if err := mgr.SaveIfChanged(ctx, resolved); err != nil {
		t.Fatal(err)
	}
	rec, err = store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("forgetting a handle must be persisted: %v", err)
	}
	if rec.Backends["conn-1"] != (session.Backend{}) {
		t.Fatalf("forgotten handle still stored: %+v", rec.Backends["conn-1"])
	}
}

func TestDeleteEndsTheSession(t *testing.T) {
	mgr, _, _ := newManager(t, time.Hour)
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "tenant-a", "key-1", mcp.ProtocolVersion, mcp.Implementation{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Resolve(ctx, sess.ID, "tenant-a"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Resolve after Delete = %v, want ErrNotFound", err)
	}
	if err := mgr.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("second Delete = %v, want nil", err)
	}
}

func TestCleanupSweepsOnlyExpiredSessions(t *testing.T) {
	mgr, store, clk := newManager(t, time.Hour)
	ctx := context.Background()

	old, err := mgr.Create(ctx, "tenant-a", "key-1", mcp.ProtocolVersion, mcp.Implementation{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour + time.Minute)
	fresh, err := mgr.Create(ctx, "tenant-a", "key-2", mcp.ProtocolVersion, mcp.Implementation{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	n, err := store.Sweep(ctx, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Sweep dropped %d sessions, want 1", n)
	}
	if _, err := store.Get(ctx, fresh.ID); err != nil {
		t.Error("Sweep dropped a live session")
	}
	if _, err := store.Get(ctx, old.ID); !errors.Is(err, session.ErrNotFound) {
		t.Error("Sweep left an expired session behind")
	}
}

func TestCleanupRoutineOnlyForSweepableStores(t *testing.T) {
	mgr, _, _ := newManager(t, time.Hour)
	if mgr.CleanupRoutine() == nil {
		t.Fatal("the memory driver implements Sweeper; the manager must run a cleanup routine over it")
	}

	noSweep := session.NewManager(nonSweeping{inner: memory.New()}, session.Options{})
	if r := noSweep.CleanupRoutine(); r != nil {
		t.Fatalf("a store without Sweep got a cleanup routine (%s)", r.Name())
	}
}

// nonSweeping hides the memory driver's Sweep (no embedding, so the
// method is not promoted), standing in for a driver whose backend
// expires records itself.
type nonSweeping struct{ inner *memory.Store }

func (n nonSweeping) Get(ctx context.Context, id string) (*pkgsession.Record, error) {
	return n.inner.Get(ctx, id)
}
func (n nonSweeping) Save(ctx context.Context, r *pkgsession.Record) error {
	return n.inner.Save(ctx, r)
}
func (n nonSweeping) Delete(ctx context.Context, id string) error { return n.inner.Delete(ctx, id) }
func (n nonSweeping) Close() error                                { return n.inner.Close() }

func TestAnonymousBackendsAreSharedAcrossStatelessCallers(t *testing.T) {
	// A caller that never sends a session id still gets to reuse the
	// backend handshake, which is what makes a stateless client cheap.
	mgr, _, _ := newManager(t, time.Hour)

	mgr.Anonymous().SetBackend("conn-1", session.Backend{SessionID: "b1", ProtocolVersion: "v"})
	if got := mgr.Anonymous().Backend("conn-1"); got.SessionID != "b1" {
		t.Fatalf("anonymous backend = %+v", got)
	}
}

func TestSessionBackendMapIsSafeForConcurrentUse(t *testing.T) {
	// The initialize fan-out writes to several connectors at once.
	mgr, _, _ := newManager(t, time.Hour)
	sess, err := mgr.Create(context.Background(), "tenant-a", "key-1", mcp.ProtocolVersion, mcp.Implementation{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "conn-" + string(rune('a'+i%8))
			sess.SetBackend(id, session.Backend{SessionID: "s", ProtocolVersion: "v"})
			_ = sess.Backend(id)
		}(i)
	}
	wg.Wait()
}
