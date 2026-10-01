package apikey

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func TestNegativeCache_AbsorbsRepeatedBadKey(t *testing.T) {
	fs := newFakeStore()
	clk := newClock(time.Now())
	a := New(fs, Options{Now: clk.Now})

	for i := 0; i < 300; i++ {
		_, err := a.Authenticate(context.Background(), bearerRequest("gk_nosuchkey"))
		if !errors.Is(err, auth.ErrInvalid) {
			t.Fatalf("attempt %d: err = %v, want ErrInvalid", i, err)
		}
	}
	if got := fs.getCallCount(); got != 1 {
		t.Errorf("store lookups = %d for 300 identical bad requests, want 1", got)
	}

	// After the TTL the key is looked up again (once).
	clk.Advance(defaultNegativeTTL + time.Second)
	_, _ = a.Authenticate(context.Background(), bearerRequest("gk_nosuchkey"))
	_, _ = a.Authenticate(context.Background(), bearerRequest("gk_nosuchkey"))
	if got := fs.getCallCount(); got != 2 {
		t.Errorf("store lookups after TTL = %d, want 2", got)
	}
}

func TestNegativeCache_DisabledWhenNegative(t *testing.T) {
	fs := newFakeStore()
	a := New(fs, Options{NegativeTTL: -1})
	for i := 0; i < 5; i++ {
		_, _ = a.Authenticate(context.Background(), bearerRequest("gk_nosuchkey"))
	}
	if got := fs.getCallCount(); got != 5 {
		t.Errorf("store lookups = %d, want 5 with the negative cache off", got)
	}
}

func TestNegativeCache_BoundedLRU(t *testing.T) {
	fs := newFakeStore()
	a := New(fs, Options{NegativeMax: 3})
	for _, k := range []string{"gk_a", "gk_b", "gk_c", "gk_d"} { // gk_a is evicted
		_, _ = a.Authenticate(context.Background(), bearerRequest(k))
	}
	if n := a.neg.ll.Len(); n != 3 {
		t.Fatalf("negative cache size = %d, want 3", n)
	}
	before := fs.getCallCount()
	_, _ = a.Authenticate(context.Background(), bearerRequest("gk_d")) // still cached
	if fs.getCallCount() != before {
		t.Error("recent entry was not served from the negative cache")
	}
	_, _ = a.Authenticate(context.Background(), bearerRequest("gk_a")) // evicted -> store
	if fs.getCallCount() != before+1 {
		t.Error("evicted entry did not go back to the store")
	}
}

func TestNegativeCache_InvalidateAndFlushClearIt(t *testing.T) {
	fs := newFakeStore()
	a := New(fs, Options{})
	_, _ = a.Authenticate(context.Background(), bearerRequest("gk_x"))
	a.Invalidate(Hash("gk_x"))
	_, _ = a.Authenticate(context.Background(), bearerRequest("gk_x"))
	if fs.getCallCount() != 2 {
		t.Errorf("Invalidate left the negative entry: lookups = %d", fs.getCallCount())
	}
	a.Flush()
	_, _ = a.Authenticate(context.Background(), bearerRequest("gk_x"))
	if fs.getCallCount() != 3 {
		t.Errorf("Flush left the negative entry: lookups = %d", fs.getCallCount())
	}
}

func TestFlush_DropsPositiveCache(t *testing.T) {
	plaintext, hash, prefix, _ := Generate()
	fs := newFakeStore()
	fs.byHash[hash] = &store.APIKey{ID: "k", TenantID: "t", Role: "admin", KeyHash: hash, KeyPrefix: prefix}
	a := New(fs, Options{})
	_, _ = a.Authenticate(context.Background(), bearerRequest(plaintext))
	_, _ = a.Authenticate(context.Background(), bearerRequest(plaintext))
	if fs.getCallCount() != 1 {
		t.Fatalf("lookups = %d, want 1 (cached)", fs.getCallCount())
	}
	a.Flush()
	_, _ = a.Authenticate(context.Background(), bearerRequest(plaintext))
	if fs.getCallCount() != 2 {
		t.Errorf("lookups after Flush = %d, want 2", fs.getCallCount())
	}
}

// fakeGuard is a scripted CredentialGuard.
type fakeGuard struct {
	mu       sync.Mutex
	locked   map[string]bool
	reported []string
}

func (g *fakeGuard) CredLocked(p string) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.locked[p], 42 * time.Second
}

func (g *fakeGuard) ReportCredFailure(p string) {
	g.mu.Lock()
	g.reported = append(g.reported, p)
	g.mu.Unlock()
}

func TestGuard_UnknownKeyFailuresAreReportedByPrefix(t *testing.T) {
	g := &fakeGuard{}
	a := New(newFakeStore(), Options{Guard: g})
	_, _ = a.Authenticate(context.Background(), bearerRequest("gk_ABCDEFGHIJKLMNOP"))
	_, _ = a.Authenticate(context.Background(), bearerRequest("gk_ABCDEFGHIJKLMNOP")) // negative-cache hit still counts
	if len(g.reported) != 2 || g.reported[0] != "gk_ABCDEFGH" {
		t.Errorf("reported = %v, want 2x gk_ABCDEFGH", g.reported)
	}
}

func TestGuard_RevokedKeyIsNotAGuess(t *testing.T) {
	plaintext, hash, prefix, _ := Generate()
	fs := newFakeStore()
	rev := time.Now().Add(-time.Hour)
	fs.byHash[hash] = &store.APIKey{ID: "k", TenantID: "t", KeyHash: hash, KeyPrefix: prefix, RevokedAt: &rev}
	g := &fakeGuard{}
	a := New(fs, Options{Guard: g})
	_, _ = a.Authenticate(context.Background(), bearerRequest(plaintext))
	if len(g.reported) != 0 {
		t.Errorf("a revoked (known) key was reported as a guess: %v", g.reported)
	}
}

func TestGuard_LockedPrefixServesOnlyCachedKeys(t *testing.T) {
	plaintext, hash, prefix, _ := Generate()
	fs := newFakeStore()
	fs.byHash[hash] = &store.APIKey{ID: "k", TenantID: "t", Role: "admin", KeyHash: hash, KeyPrefix: prefix}
	g := &fakeGuard{locked: map[string]bool{}}
	clk := newClock(time.Now())
	a := New(fs, Options{Guard: g, Now: clk.Now})

	// Legitimate holder warms the cache.
	if _, err := a.Authenticate(context.Background(), bearerRequest(plaintext)); err != nil {
		t.Fatal(err)
	}
	g.locked[prefix] = true
	lookups := fs.getCallCount()

	// Past the TTL, a locked prefix still serves the cached holder...
	clk.Advance(time.Hour)
	if _, err := a.Authenticate(context.Background(), bearerRequest(plaintext)); err != nil {
		t.Errorf("cached key refused while its prefix is locked: %v", err)
	}
	// ...but a guess at the same prefix gets 429 with no DB lookup.
	guess := plaintext[:len(plaintext)-1] + "#"
	_, err := a.Authenticate(context.Background(), bearerRequest(guess))
	var rle *auth.RateLimitedError
	if !errors.As(err, &rle) || rle.RetryAfter != 42*time.Second {
		t.Fatalf("guess err = %v, want RateLimitedError(42s)", err)
	}
	if fs.getCallCount() != lookups {
		t.Errorf("a locked-prefix guess hit the store (%d -> %d)", lookups, fs.getCallCount())
	}
	if len(g.reported) != 0 {
		t.Errorf("locked-prefix 429s were reported as new failures: %v", g.reported)
	}
}

// fakeNotifier drives ListenRevocations.
type fakeNotifier struct {
	mu    sync.Mutex
	calls int
	// per-call scripts: events delivered after ready, then the error to return.
	script []notifierRun
	done   chan struct{}
}

type notifierRun struct {
	events []string
	err    error
}

func (f *fakeNotifier) ListenKeyRevocations(ctx context.Context, onReady func(), onRevoke func(string)) error {
	f.mu.Lock()
	i := f.calls
	f.calls++
	f.mu.Unlock()
	if i >= len(f.script) {
		close(f.done)
		<-ctx.Done()
		return ctx.Err()
	}
	onReady()
	for _, e := range f.script[i].events {
		onRevoke(e)
	}
	return f.script[i].err
}

func TestListenRevocations_EvictsFlushesOnReconnectAndBacksOff(t *testing.T) {
	plaintext, hash, prefix, _ := Generate()
	fs := newFakeStore()
	fs.byHash[hash] = &store.APIKey{ID: "k", TenantID: "t", Role: "admin", KeyHash: hash, KeyPrefix: prefix}
	a := New(fs, Options{})
	if _, err := a.Authenticate(context.Background(), bearerRequest(plaintext)); err != nil {
		t.Fatal(err)
	}

	// Run 1: ready (flush), announces our key, then the connection drops.
	// Run 2: ready (flush again) then blocks until ctx is cancelled.
	n := &fakeNotifier{done: make(chan struct{}), script: []notifierRun{
		{events: []string{hash}, err: errors.New("connection reset")},
		{},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		a.ListenRevocations(ctx, n, slog.New(slog.DiscardHandler))
		close(finished)
	}()
	select {
	case <-n.done:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not reconnect after an error")
	}
	if n.calls != 3 { // run1, run2, and the blocking third call that closed done
		t.Errorf("subscription attempts = %d, want 3", n.calls)
	}
	// The announced hash was evicted (and the cache flushed on ready): the
	// next authenticate goes back to the store.
	before := fs.getCallCount()
	_, _ = a.Authenticate(context.Background(), bearerRequest(plaintext))
	if fs.getCallCount() != before+1 {
		t.Error("revocation event did not evict the cached key")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("ListenRevocations did not return after ctx cancel")
	}
}

var _ store.RevocationNotifier = (*fakeNotifier)(nil)
