// Package sessiontest is the conformance suite every session.Store (and
// session.Notifier) backend must pass.
//
// A new backend is added by implementing session.Store (see
// pkg/session), registering its driver via session.Register in an init(),
// and then wiring this suite up as its test:
//
//	func TestConformance(t *testing.T) {
//		sessiontest.Run(t, func(t *testing.T) session.Store {
//			// Build and return a Store over a fresh backend (or a
//			// shared one: the suite uses random ids, so subtests
//			// cannot see each other's records).
//		})
//	}
//
//	func TestNotifierConformance(t *testing.T) {
//		sessiontest.RunNotifier(t, func(t *testing.T) session.Notifier {
//			// Return a NEW Notifier instance on the same backend each
//			// call: the suite builds two and expects them to reach
//			// each other.
//		})
//	}
//
// Run exercises the Store contract documented on session.Store:
// round-tripping every Record field, ErrNotFound for an unknown id,
// idempotent Delete, upsert-on-Save, the expiry contract (an expired
// Record is never returned), Get handing back an independent copy, and
// concurrent Save/Get safety. RunNotifier exercises the Notifier
// contract, for tools/list_changed and for session messages alike: a
// publish reaches every other instance exactly once and never the
// publisher's own callback.
//
// These tests run against the real backend -- there is no fake Redis in
// this module -- so a driver's test skips when its backend is not
// reachable (the shipped Redis driver reads GATEWAY_TEST_REDIS_ADDR).
package sessiontest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session"
)

// Run drives the Store conformance suite against Stores built by
// newStore, which is called once per subtest.
func Run(t *testing.T, newStore func(t *testing.T) session.Store) {
	t.Helper()

	t.Run("RoundTripsEveryField", func(t *testing.T) { testRoundTrip(t, open(t, newStore)) })
	t.Run("GetUnknownIsNotFound", func(t *testing.T) { testGetUnknown(t, open(t, newStore)) })
	t.Run("DeleteIsIdempotent", func(t *testing.T) { testDeleteIdempotent(t, open(t, newStore)) })
	t.Run("SaveIsAnUpsert", func(t *testing.T) { testSaveUpsert(t, open(t, newStore)) })
	t.Run("SaveRejectsAnInvalidRecord", func(t *testing.T) { testSaveInvalid(t, open(t, newStore)) })
	t.Run("ExpiredRecordsAreNeverReturned", func(t *testing.T) { testExpiry(t, open(t, newStore)) })
	t.Run("GetReturnsAnIndependentCopy", func(t *testing.T) { testIndependentCopy(t, open(t, newStore)) })
	t.Run("ConcurrentSaveAndGetAreSafe", func(t *testing.T) { testConcurrent(t, open(t, newStore)) })
	t.Run("SweepDropsOnlyExpiredRecords", func(t *testing.T) { testSweep(t, open(t, newStore)) })
}

func open(t *testing.T, newStore func(t *testing.T) session.Store) session.Store {
	t.Helper()
	st := newStore(t)
	if st == nil {
		t.Fatal("newStore returned nil")
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

// newID mints an id the way the gateway does, so a shared backend never
// sees two subtests collide.
func newID(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// sample is a fully populated Record with sub-second timestamps, so a
// backend that truncates precision or drops a field is caught.
func sample(t *testing.T, ttl time.Duration) *session.Record {
	t.Helper()
	now := time.Now().Add(-1500 * time.Millisecond).Round(0)
	return &session.Record{
		ID:              newID(t),
		TenantID:        "tenant-" + newID(t)[:8],
		Principal:       "key-" + newID(t)[:8],
		ClientInfo:      mcp.Implementation{Name: "claude-code", Version: "2.1.0"},
		ProtocolVersion: mcp.ProtocolVersion,
		CreatedAt:       now,
		LastSeenAt:      now.Add(700 * time.Millisecond),
		ExpiresAt:       time.Now().Add(ttl).Round(0),
		Backends: map[string]session.Backend{
			"conn-1": {SessionID: "backend-sess-1", ProtocolVersion: mcp.ProtocolVersion},
			"conn-2": {ProtocolVersion: mcp.ProtocolVersionLegacy},
			"conn-3": {},
		},
		ClientCapabilities: map[string]json.RawMessage{
			"sampling":    json.RawMessage(`{}`),
			"elicitation": json.RawMessage(`{"form":{}}`),
			"roots":       json.RawMessage(`{"listChanged":true}`),
		},
	}
}

func mustSave(t *testing.T, ctx context.Context, st session.Store, r *session.Record) {
	t.Helper()
	if err := st.Save(ctx, r); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Cleanup(func() { _ = st.Delete(context.Background(), r.ID) })
}

func assertEqualRecord(t *testing.T, got, want *session.Record) {
	t.Helper()
	if got.ID != want.ID || got.TenantID != want.TenantID || got.Principal != want.Principal {
		t.Errorf("identity fields: got %+v, want %+v", got, want)
	}
	if got.ClientInfo != want.ClientInfo {
		t.Errorf("ClientInfo = %+v, want %+v", got.ClientInfo, want.ClientInfo)
	}
	if got.ProtocolVersion != want.ProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want %q", got.ProtocolVersion, want.ProtocolVersion)
	}
	for name, pair := range map[string][2]time.Time{
		"CreatedAt":  {got.CreatedAt, want.CreatedAt},
		"LastSeenAt": {got.LastSeenAt, want.LastSeenAt},
		"ExpiresAt":  {got.ExpiresAt, want.ExpiresAt},
	} {
		if !pair[0].Equal(pair[1]) {
			t.Errorf("%s = %v, want %v", name, pair[0], pair[1])
		}
	}
	if len(got.Backends) != len(want.Backends) {
		t.Errorf("Backends = %+v, want %+v", got.Backends, want.Backends)
	}
	for id, b := range want.Backends {
		if got.Backends[id] != b {
			t.Errorf("Backends[%s] = %+v, want %+v", id, got.Backends[id], b)
		}
	}
	if len(got.ClientCapabilities) != len(want.ClientCapabilities) {
		t.Errorf("ClientCapabilities = %s, want %s", got.ClientCapabilities, want.ClientCapabilities)
	}
	for name, raw := range want.ClientCapabilities {
		if !sameJSON(got.ClientCapabilities[name], raw) {
			t.Errorf("ClientCapabilities[%s] = %s, want %s", name, got.ClientCapabilities[name], raw)
		}
	}
}

// sameJSON reports whether a and b are the same JSON value up to
// insignificant whitespace: a store that encodes a Record may compact a
// raw capability object, which changes nothing it means.
func sameJSON(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

func testRoundTrip(t *testing.T, st session.Store) {
	ctx := context.Background()
	want := sample(t, time.Hour)
	mustSave(t, ctx, st, want)

	got, err := st.Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertEqualRecord(t, got, want)

	// The zero-valued handle for conn-3 must survive too: "no handshake
	// yet" for a connector the gateway has seen is a fact worth keeping.
	if _, ok := got.Backends["conn-3"]; !ok {
		t.Error("a zero Backend entry was dropped on the round trip")
	}
}

func testGetUnknown(t *testing.T, st session.Store) {
	ctx := context.Background()
	if _, err := st.Get(ctx, newID(t)); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := st.Get(ctx, ""); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get(\"\") = %v, want ErrNotFound", err)
	}
}

func testDeleteIdempotent(t *testing.T, st session.Store) {
	ctx := context.Background()
	r := sample(t, time.Hour)
	mustSave(t, ctx, st, r)

	if err := st.Delete(ctx, r.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get(ctx, r.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := st.Delete(ctx, r.ID); err != nil {
		t.Fatalf("second Delete = %v, want nil", err)
	}
	if err := st.Delete(ctx, newID(t)); err != nil {
		t.Fatalf("Delete(never stored) = %v, want nil", err)
	}
}

func testSaveUpsert(t *testing.T, st session.Store) {
	ctx := context.Background()
	first := sample(t, time.Hour)
	mustSave(t, ctx, st, first)

	// Same id, every mutable field changed, one connector handle rotated,
	// one dropped and one added.
	second := first.Clone()
	second.LastSeenAt = first.LastSeenAt.Add(time.Minute)
	second.ExpiresAt = first.ExpiresAt.Add(time.Minute)
	second.Backends = map[string]session.Backend{
		"conn-1": {SessionID: "rotated", ProtocolVersion: mcp.ProtocolVersion},
		"conn-9": {SessionID: "new", ProtocolVersion: mcp.ProtocolVersion},
	}
	if err := st.Save(ctx, second); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	got, err := st.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertEqualRecord(t, got, second)
	if _, stale := got.Backends["conn-2"]; stale {
		t.Error("Save did not replace the whole record: a dropped connector handle survived")
	}
}

func testSaveInvalid(t *testing.T, st session.Store) {
	ctx := context.Background()
	noID := sample(t, time.Hour)
	noID.ID = ""
	if err := st.Save(ctx, noID); err == nil {
		t.Error("Save accepted a record with an empty id")
	}
	noExpiry := sample(t, time.Hour)
	noExpiry.ExpiresAt = time.Time{}
	if err := st.Save(ctx, noExpiry); err == nil {
		t.Error("Save accepted a record with no expiry")
	}
	if _, err := st.Get(ctx, noExpiry.ID); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Get after a rejected Save = %v, want ErrNotFound", err)
	}
	if err := st.Save(ctx, nil); err == nil {
		t.Error("Save accepted a nil record")
	}
}

func testExpiry(t *testing.T, st session.Store) {
	ctx := context.Background()

	// Already expired on the way in: equivalent to Delete.
	past := sample(t, -time.Minute)
	mustSave(t, ctx, st, past)
	if _, err := st.Get(ctx, past.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get(saved already expired) = %v, want ErrNotFound", err)
	}

	// Saving an expired record over a live one must retire the live one
	// (a Store with a native TTL cannot SET a non-positive expiry; it
	// must delete instead).
	live := sample(t, time.Hour)
	mustSave(t, ctx, st, live)
	retired := live.Clone()
	retired.ExpiresAt = time.Now().Add(-time.Second)
	if err := st.Save(ctx, retired); err != nil {
		t.Fatalf("Save(expired over live): %v", err)
	}
	if _, err := st.Get(ctx, live.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get after saving an expired record over a live one = %v, want ErrNotFound", err)
	}

	// Expires shortly: visible now, gone once ExpiresAt passes.
	const ttl = 700 * time.Millisecond
	soon := sample(t, ttl)
	mustSave(t, ctx, st, soon)
	if _, err := st.Get(ctx, soon.ID); err != nil {
		t.Fatalf("Get(expires in %v) = %v, want the record", ttl, err)
	}
	time.Sleep(time.Until(soon.ExpiresAt) + 300*time.Millisecond)
	if _, err := st.Get(ctx, soon.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get after ExpiresAt passed = %v, want ErrNotFound", err)
	}

	// The window slides: re-saving with a later ExpiresAt keeps it alive
	// past the original expiry.
	slid := sample(t, ttl)
	mustSave(t, ctx, st, slid)
	slid.ExpiresAt = time.Now().Add(time.Hour)
	if err := st.Save(ctx, slid); err != nil {
		t.Fatalf("Save(slid): %v", err)
	}
	time.Sleep(ttl + 200*time.Millisecond)
	if _, err := st.Get(ctx, slid.ID); err != nil {
		t.Fatalf("Get after the idle window was slid = %v, want the record", err)
	}
}

func testIndependentCopy(t *testing.T, st session.Store) {
	ctx := context.Background()
	r := sample(t, time.Hour)
	mustSave(t, ctx, st, r)

	// Mutating what the caller handed to Save must not reach the store.
	r.Backends["conn-1"] = session.Backend{SessionID: "mutated-after-save", ProtocolVersion: "x"}
	r.TenantID = "mutated"
	r.ClientCapabilities["roots"][2] = 'X'
	delete(r.ClientCapabilities, "sampling")

	got, err := st.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Backends["conn-1"].SessionID != "backend-sess-1" || got.TenantID == "mutated" ||
		!sameJSON(got.ClientCapabilities["roots"], json.RawMessage(`{"listChanged":true}`)) ||
		got.ClientCapabilities["sampling"] == nil {
		t.Fatal("the store shares memory with the record passed to Save")
	}

	// Mutating what Get handed back must not reach the store either.
	got.Backends["conn-1"] = session.Backend{SessionID: "mutated-after-get", ProtocolVersion: "x"}
	delete(got.Backends, "conn-2")

	again, err := st.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if again.Backends["conn-1"].SessionID != "backend-sess-1" || len(again.Backends) != 3 {
		t.Fatal("the store shares memory with the record returned by Get")
	}
}

func testConcurrent(t *testing.T, st session.Store) {
	ctx := context.Background()
	base := sample(t, time.Hour)
	mustSave(t, ctx, st, base)

	const writers, readers, rounds = 4, 4, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers+readers)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			mine := base.Clone()
			for n := 0; n < rounds; n++ {
				mine.LastSeenAt = time.Now()
				mine.ExpiresAt = mine.LastSeenAt.Add(time.Hour)
				mine.Backends["conn-1"] = session.Backend{SessionID: string(rune('a' + w)), ProtocolVersion: mcp.ProtocolVersion}
				if err := st.Save(ctx, mine); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < rounds; n++ {
				got, err := st.Get(ctx, base.ID)
				if err != nil {
					errs <- err
					return
				}
				if got.TenantID != base.TenantID || got.ID != base.ID {
					errs <- errors.New("a concurrent read saw a corrupted record")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	got, err := st.Get(ctx, base.ID)
	if err != nil {
		t.Fatalf("Get after concurrent saves: %v", err)
	}
	if id := got.Backends["conn-1"].SessionID; len(id) != 1 || id[0] < 'a' || id[0] >= 'a'+writers {
		t.Fatalf("stored handle = %q, want one writer's value intact", id)
	}
	if got.TenantID != base.TenantID {
		t.Fatalf("concurrent saves corrupted the record: %+v", got)
	}
}

func testSweep(t *testing.T, st session.Store) {
	sw, ok := st.(session.Sweeper)
	if !ok {
		t.Skip("store does not implement session.Sweeper (it expires records natively)")
	}
	ctx := context.Background()

	expired := sample(t, time.Hour)
	live := sample(t, time.Hour)
	mustSave(t, ctx, st, expired)
	mustSave(t, ctx, st, live)

	// Sweep with a clock a little past the first record's expiry, but
	// before the second's: the gateway drives Sweep with its own clock,
	// so the store must use the time it is given, not time.Now.
	n, err := sw.Sweep(ctx, expired.ExpiresAt.Add(time.Second))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n < 1 {
		t.Fatalf("Sweep dropped %d records, want at least 1", n)
	}
	// Both were saved with a one-hour TTL within microseconds of each
	// other, so a sweep at expired.ExpiresAt+1s takes both; the point
	// is that a sweep before either expiry takes neither.
	fresh := sample(t, time.Hour)
	mustSave(t, ctx, st, fresh)
	n, err = sw.Sweep(ctx, fresh.ExpiresAt.Add(-time.Minute))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("Sweep before expiry dropped %d records, want 0", n)
	}
	if _, err := st.Get(ctx, fresh.ID); err != nil {
		t.Fatalf("Sweep removed a live record: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Notifier
// ---------------------------------------------------------------------------

// RunNotifier drives the Notifier conformance suite. newNotifier must
// return a NEW instance over the same backend on every call; the suite
// builds two (two replicas) and checks that a Publish on one reaches the
// other exactly once and never itself.
func RunNotifier(t *testing.T, newNotifier func(t *testing.T) session.Notifier) {
	t.Helper()

	t.Run("PublishReachesOtherInstancesOnceAndNeverItself", func(t *testing.T) {
		testNotifierDelivery(t, newNotifier)
	})
	t.Run("SubscribeReturnsWhenContextIsDone", func(t *testing.T) {
		testNotifierSubscribeReturns(t, newNotifier)
	})
	t.Run("PublishRejectsAnEmptyTenant", func(t *testing.T) {
		n := newNotifier(t)
		t.Cleanup(func() { _ = n.Close() })
		if err := n.Publish(context.Background(), ""); err == nil {
			t.Fatal("Publish(\"\") = nil, want an error")
		}
	})
	t.Run("PublishSessionReachesOtherInstancesOnceAndNeverItself", func(t *testing.T) {
		testSessionDelivery(t, newNotifier)
	})
	t.Run("SubscribeSessionsReturnsWhenContextIsDone", func(t *testing.T) {
		n := newNotifier(t)
		t.Cleanup(func() { _ = n.Close() })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- n.SubscribeSessions(ctx, func(session.SessionMessage) {}) }()
		time.Sleep(200 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("SubscribeSessions returned %v after cancellation, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("SubscribeSessions did not return after its context was cancelled")
		}
	})
	t.Run("PublishSessionRejectsAnIncompleteMessage", func(t *testing.T) {
		n := newNotifier(t)
		t.Cleanup(func() { _ = n.Close() })
		for _, m := range []session.SessionMessage{
			{SessionID: "s", Payload: []byte(`{}`)},
			{TenantID: "t", Payload: []byte(`{}`)},
			{TenantID: "t", SessionID: "s"},
		} {
			if err := n.PublishSession(context.Background(), m); err == nil {
				t.Errorf("PublishSession(%+v) = nil, want an error", m)
			}
		}
	})
}

// subscriber runs one Notifier's Subscribe in the background and counts
// deliveries per tenant.
type subscriber struct {
	n    session.Notifier
	mu   sync.Mutex
	seen map[string]int
	got  chan string
}

func startSubscriber(t *testing.T, n session.Notifier) *subscriber {
	t.Helper()
	s := &subscriber{n: n, seen: make(map[string]int), got: make(chan string, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- n.Subscribe(ctx, func(tenantID string) {
			s.mu.Lock()
			s.seen[tenantID]++
			s.mu.Unlock()
			select {
			case s.got <- tenantID:
			default:
			}
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Subscribe returned %v after cancellation, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Subscribe did not return after its context was cancelled")
		}
		_ = n.Close()
	})
	return s
}

func (s *subscriber) count(tenant string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[tenant]
}

// waitFor blocks until tenant has been delivered at least once, or d.
func (s *subscriber) waitFor(tenant string, d time.Duration) bool {
	deadline := time.After(d)
	for {
		if s.count(tenant) > 0 {
			return true
		}
		select {
		case <-deadline:
			return s.count(tenant) > 0
		case <-s.got:
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// settle proves that b is subscribed as far as the backend is concerned:
// Subscribe blocks and offers no readiness signal, so a publishes probe
// tenants until one arrives at b. Everything published afterwards on the
// same backend reaches b in order.
func settle(t *testing.T, a session.Notifier, b *subscriber) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		probe := "probe-" + newID(t)[:8]
		if err := a.Publish(context.Background(), probe); err != nil {
			t.Fatalf("Publish(probe): %v", err)
		}
		if b.waitFor(probe, 100*time.Millisecond) {
			return
		}
	}
	t.Fatal("the second instance never received a probe; is Subscribe delivering?")
}

func testNotifierDelivery(t *testing.T, newNotifier func(t *testing.T) session.Notifier) {
	a := startSubscriber(t, newNotifier(t))
	b := startSubscriber(t, newNotifier(t))
	settle(t, a.n, b)
	settle(t, b.n, a)

	tenant := "tenant-" + newID(t)[:8]
	if err := a.n.Publish(context.Background(), tenant); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !b.waitFor(tenant, 5*time.Second) {
		t.Fatal("the other instance was not delivered the publish")
	}
	// Anything duplicated would have arrived by the time a second, later
	// message has: publish a marker and wait for it.
	marker := "marker-" + newID(t)[:8]
	if err := a.n.Publish(context.Background(), marker); err != nil {
		t.Fatalf("Publish(marker): %v", err)
	}
	if !b.waitFor(marker, 5*time.Second) {
		t.Fatal("the marker publish was not delivered")
	}
	time.Sleep(100 * time.Millisecond)
	if n := b.count(tenant); n != 1 {
		t.Fatalf("the other instance was delivered %d times, want exactly 1", n)
	}
	if n := a.count(tenant); n != 0 {
		t.Fatalf("the publishing instance was delivered its own publish %d times, want 0", n)
	}

	// And the other way round.
	reverse := "tenant-" + newID(t)[:8]
	if err := b.n.Publish(context.Background(), reverse); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !a.waitFor(reverse, 5*time.Second) {
		t.Fatal("the first instance was not delivered the second's publish")
	}
	if n := b.count(reverse); n != 0 {
		t.Fatalf("the second instance was delivered its own publish %d times, want 0", n)
	}
}

func testNotifierSubscribeReturns(t *testing.T, newNotifier func(t *testing.T) session.Notifier) {
	n := newNotifier(t)
	t.Cleanup(func() { _ = n.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Subscribe(ctx, func(string) {}) }()

	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Subscribe returned %v after cancellation, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe did not return after its context was cancelled")
	}
}

// sessionSubscriber runs one Notifier's SubscribeSessions in the
// background and records what it is delivered, by session id.
type sessionSubscriber struct {
	n    session.Notifier
	mu   sync.Mutex
	seen map[string][]session.SessionMessage
}

func startSessionSubscriber(t *testing.T, n session.Notifier) *sessionSubscriber {
	t.Helper()
	s := &sessionSubscriber{n: n, seen: make(map[string][]session.SessionMessage)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- n.SubscribeSessions(ctx, func(m session.SessionMessage) {
			s.mu.Lock()
			s.seen[m.SessionID] = append(s.seen[m.SessionID], m)
			s.mu.Unlock()
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("SubscribeSessions returned %v after cancellation, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("SubscribeSessions did not return after its context was cancelled")
		}
		_ = n.Close()
	})
	return s
}

func (s *sessionSubscriber) got(sessionID string) []session.SessionMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]session.SessionMessage(nil), s.seen[sessionID]...)
}

func (s *sessionSubscriber) waitFor(sessionID string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if len(s.got(sessionID)) > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return len(s.got(sessionID)) > 0
}

func sessionMessage(t *testing.T, sessionID string) session.SessionMessage {
	return session.SessionMessage{
		TenantID:  "tenant-" + sessionID[:8],
		SessionID: sessionID,
		Payload:   []byte(`{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":"gw://c/x"}}`),
	}
}

// requestMessage is a session message of a non-default kind, so a
// Notifier that drops Kind on the way through is caught.
func requestMessage(t *testing.T, sessionID string) session.SessionMessage {
	return session.SessionMessage{
		TenantID:  "tenant-" + sessionID[:8],
		SessionID: sessionID,
		Kind:      session.KindRequest,
		Payload:   []byte(`{"jsonrpc":"2.0","id":"gw-0123456789abcdef","method":"elicitation/create","params":{}}`),
	}
}

func testSessionDelivery(t *testing.T, newNotifier func(t *testing.T) session.Notifier) {
	a := startSessionSubscriber(t, newNotifier(t))
	b := startSessionSubscriber(t, newNotifier(t))

	// Settle: SubscribeSessions offers no readiness signal, so probe
	// until one arrives each way.
	settleSessions := func(from session.Notifier, to *sessionSubscriber) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			probe := newID(t)
			if err := from.PublishSession(context.Background(), sessionMessage(t, probe)); err != nil {
				t.Fatalf("PublishSession(probe): %v", err)
			}
			if to.waitFor(probe, 100*time.Millisecond) {
				return
			}
		}
		t.Fatal("the other instance never received a probe session message")
	}
	settleSessions(a.n, b)
	settleSessions(b.n, a)

	id := newID(t)
	want := sessionMessage(t, id)
	if err := a.n.PublishSession(context.Background(), want); err != nil {
		t.Fatalf("PublishSession: %v", err)
	}
	if !b.waitFor(id, 5*time.Second) {
		t.Fatal("the other instance was not delivered the session message")
	}
	marker := newID(t)
	if err := a.n.PublishSession(context.Background(), sessionMessage(t, marker)); err != nil {
		t.Fatalf("PublishSession(marker): %v", err)
	}
	if !b.waitFor(marker, 5*time.Second) {
		t.Fatal("the marker session message was not delivered")
	}
	time.Sleep(100 * time.Millisecond)

	got := b.got(id)
	if len(got) != 1 {
		t.Fatalf("the other instance was delivered %d copies, want exactly 1", len(got))
	}
	if got[0].TenantID != want.TenantID || got[0].SessionID != want.SessionID || string(got[0].Payload) != string(want.Payload) ||
		got[0].Kind != session.KindNotification {
		t.Fatalf("delivered %+v, want %+v verbatim", got[0], want)
	}
	if n := len(a.got(id)); n != 0 {
		t.Fatalf("the publishing instance was delivered its own session message %d times, want 0", n)
	}

	// Kind travels verbatim.
	reqID := newID(t)
	wantReq := requestMessage(t, reqID)
	if err := b.n.PublishSession(context.Background(), wantReq); err != nil {
		t.Fatalf("PublishSession(request): %v", err)
	}
	if !a.waitFor(reqID, 5*time.Second) {
		t.Fatal("the request-kind session message was not delivered")
	}
	if got := a.got(reqID)[0]; got.Kind != session.KindRequest || string(got.Payload) != string(wantReq.Payload) {
		t.Fatalf("delivered %+v, want %+v verbatim", got, wantReq)
	}
}
