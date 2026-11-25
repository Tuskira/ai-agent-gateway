package invalidate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type recTarget struct {
	mu      sync.Mutex
	applied []store.Invalidation
	flushes int
}

func (r *recTarget) Apply(ev store.Invalidation) {
	r.mu.Lock()
	r.applied = append(r.applied, ev)
	r.mu.Unlock()
}

func (r *recTarget) Flush() {
	r.mu.Lock()
	r.flushes++
	r.mu.Unlock()
}

func (r *recTarget) snapshot() (int, []store.Invalidation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushes, append([]store.Invalidation(nil), r.applied...)
}

// fakeNotifier plays one scripted subscription per call: it reports ready,
// delivers its events, then fails, so Listen has to reconnect.
type fakeNotifier struct {
	mu     sync.Mutex
	rounds [][]store.Invalidation
	calls  int
	done   chan struct{}
}

func (f *fakeNotifier) ListenInvalidations(ctx context.Context, onReady func(), onEvent func(store.Invalidation)) error {
	f.mu.Lock()
	i := f.calls
	f.calls++
	f.mu.Unlock()
	if i >= len(f.rounds) {
		select {
		case <-f.done:
		default:
			close(f.done)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	onReady()
	for _, ev := range f.rounds[i] {
		onEvent(ev)
	}
	return errors.New("connection lost")
}

// A lost subscription is re-established, and every (re)establishment flushes
// the target: events published while it was down are gone.
func TestListenFlushesOnEveryReconnect(t *testing.T) {
	ev1 := store.Invalidation{Kind: store.InvalidateProfile, Tenant: "t1", ID: "p1"}
	ev2 := store.Invalidation{Kind: store.InvalidateCredential, Tenant: "t2", ID: "c1"}
	n := &fakeNotifier{rounds: [][]store.Invalidation{{ev1}, {ev2}}, done: make(chan struct{})}
	tgt := &recTarget{}

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		Listen(ctx, n, tgt, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	select {
	case <-n.done:
	case <-time.After(10 * time.Second):
		t.Fatal("listener did not reconnect")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return after its context ended")
	}

	flushes, applied := tgt.snapshot()
	if flushes != 2 {
		t.Errorf("flushes = %d, want 2 (one per established subscription)", flushes)
	}
	if len(applied) != 2 || applied[0] != ev1 || applied[1] != ev2 {
		t.Errorf("applied = %+v, want [%+v %+v]", applied, ev1, ev2)
	}
}

type recTenantCache struct {
	tenants []string
	all     int
}

func (r *recTenantCache) InvalidateTenant(t string) { r.tenants = append(r.tenants, t) }
func (r *recTenantCache) InvalidateAll()            { r.all++ }

type recKeys struct {
	keys    []string
	flushed int
}

func (r *recKeys) Invalidate(h string) { r.keys = append(r.keys, h) }
func (r *recKeys) Flush()              { r.flushed++ }

func TestCachesApplyRoutesByKindAndTenant(t *testing.T) {
	prof, secrets, ext, keys := &recTarget{}, &recTenantCache{}, &recTenantCache{}, &recKeys{}
	c := Caches{Profiles: prof, Secrets: secrets, External: ext, Keys: keys}

	c.Apply(store.Invalidation{Kind: store.InvalidateProfile, Tenant: "t1", ID: "p"})
	if _, got := prof.snapshot(); len(got) != 1 {
		t.Errorf("profile event reached the plane %d times, want 1", len(got))
	}
	if len(secrets.tenants)+len(ext.tenants)+len(keys.keys) != 0 {
		t.Error("a profile event touched the secret, external or key caches")
	}

	for _, kind := range []string{store.InvalidateCredential, store.InvalidateSource} {
		c.Apply(store.Invalidation{Kind: kind, Tenant: "t1", ID: "x"})
	}
	if len(secrets.tenants) != 2 || secrets.tenants[0] != "t1" || len(ext.tenants) != 2 || ext.tenants[0] != "t1" {
		t.Errorf("credential/source events: secrets %v external %v, want tenant t1 twice each", secrets.tenants, ext.tenants)
	}
	if secrets.all != 0 || ext.all != 0 {
		t.Error("a tenant's credential event emptied every tenant's cache")
	}

	// No tenant on a credential event: cannot scope it, so drop everything.
	c.Apply(store.Invalidation{Kind: store.InvalidateCredential, ID: "x"})
	if secrets.all != 1 || ext.all != 1 || keys.flushed != 1 {
		t.Errorf("tenantless credential event: secrets.all %d external.all %d keys flushed %d, want 1 each", secrets.all, ext.all, keys.flushed)
	}

	c.Apply(store.Invalidation{Kind: store.InvalidateKey, Tenant: "t1", ID: "hash"})
	c.Apply(store.Invalidation{Kind: store.InvalidateKey, Tenant: "t1"})
	if len(keys.keys) != 1 || keys.keys[0] != "hash" {
		t.Errorf("key events evicted %v, want [hash] (an empty hash evicts nothing)", keys.keys)
	}

	c.Apply(store.Invalidation{Kind: "unknown", Tenant: "t1", ID: "x"})
}

// Every field of Caches is optional.
func TestCachesWithNothingWired(t *testing.T) {
	var c Caches
	for _, k := range []string{store.InvalidateProfile, store.InvalidateCredential, store.InvalidateSource, store.InvalidateKey} {
		c.Apply(store.Invalidation{Kind: k, Tenant: "t", ID: "i"})
	}
	c.Flush()
}
