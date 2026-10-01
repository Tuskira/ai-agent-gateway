package transport_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/transport"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	pkgsession "github.com/Tuskira/tusk-ai-secured-gateway/pkg/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/redis"
)

// replica is one mcp replica's notification path: its local Hub and the
// Bridge that relays it through the redis driver's Notifier, with the
// subscriber routine running.
type replica struct {
	hub    *transport.Hub
	bridge *transport.Bridge
}

// startReplica runs a Hub + Bridge pair against the Redis named by
// GATEWAY_TEST_REDIS_ADDR, or skips: the Bridge is tested over the real
// driver, never a fake.
func startReplica(t *testing.T) *replica {
	t.Helper()
	addr := os.Getenv("GATEWAY_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("GATEWAY_TEST_REDIS_ADDR not set; skipping the tools/list_changed bridge tests")
	}
	store, notifier, err := redis.New(context.Background(), pkgsession.Config{Addr: addr})
	if err != nil {
		t.Fatalf("open redis driver: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(); _ = notifier.Close() })

	hub := transport.NewHub()
	bridge := transport.NewBridge(hub, notifier, slog.New(slog.NewTextHandler(io.Discard, nil)))

	routine := bridge.Routine()
	ctx, cancel := context.WithCancel(context.Background())
	if err := routine.Init(ctx); err != nil {
		cancel()
		t.Fatalf("bridge Init: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- routine.Run(ctx) }()
	t.Cleanup(func() {
		_ = routine.Stop(context.Background())
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("bridge Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("bridge Run did not return after Stop")
		}
		cancel()
	})
	return &replica{hub: hub, bridge: bridge}
}

func waitSignal(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// settle publishes probe tenants from a until one arrives at b, proving
// b's subscriber is live (Subscribe offers no readiness signal).
func settle(t *testing.T, a, b *replica) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		probe := "probe-" + uuid.NewString()
		ch, unsub := b.hub.Subscribe(probe)
		a.bridge.ToolsListChanged(probe)
		got := waitSignal(ch, 100*time.Millisecond)
		unsub()
		if got {
			return
		}
	}
	t.Fatal("replica B never received a probe over the bridge")
}

func TestBridgeRelaysToolsListChangedAcrossReplicasExactlyOnce(t *testing.T) {
	a := startReplica(t)
	b := startReplica(t)
	settle(t, a, b)
	settle(t, b, a)

	// A unique tenant, so another test publishing on the shared channel
	// cannot satisfy (or spoil) these assertions.
	tenant := "tenant-" + uuid.NewString()
	other := "tenant-" + uuid.NewString()

	onA, unsubA := a.hub.Subscribe(tenant)
	defer unsubA()
	onB, unsubB := b.hub.Subscribe(tenant)
	defer unsubB()
	otherOnB, unsubOther := b.hub.Subscribe(other)
	defer unsubOther()

	a.bridge.ToolsListChanged(tenant)

	if !waitSignal(onA, time.Second) {
		t.Fatal("the publishing replica's own stream was not signalled")
	}
	if !waitSignal(onB, 5*time.Second) {
		t.Fatal("replica B's stream was not signalled over the bridge")
	}

	// B has now received the message, so A's subscriber has had it too.
	// A must have skipped its own echo: its stream saw one signal, not
	// two.
	if waitSignal(onA, 300*time.Millisecond) {
		t.Fatal("the publishing replica's stream was signalled twice (its own echo was not skipped)")
	}
	if waitSignal(onB, 300*time.Millisecond) {
		t.Fatal("replica B's stream was signalled twice")
	}
	if waitSignal(otherOnB, 100*time.Millisecond) {
		t.Fatal("a stream of a different tenant was signalled")
	}

	// And the other way round.
	b.bridge.ToolsListChanged(tenant)
	if !waitSignal(onA, 5*time.Second) {
		t.Fatal("replica A's stream was not signalled for B's change")
	}
}

// A session's streams sit on one replica; a notification for it raised
// on another (a resource update read off a connector stream there) must
// reach them exactly once, and must not be published at all when the
// raising replica holds the stream itself.
func TestBridgeRelaysASessionNotificationToTheReplicaHoldingItsStream(t *testing.T) {
	a := startReplica(t)
	b := startReplica(t)
	settle(t, a, b)
	settle(t, b, a)

	tenant, sessionID := "tenant-"+uuid.NewString(), "session-"+uuid.NewString()
	onB, unsubB := b.hub.SubscribeSession(tenant, sessionID)
	defer unsubB()
	sameIDOtherTenant, unsubOther := b.hub.SubscribeSession("tenant-"+uuid.NewString(), sessionID)
	defer unsubOther()

	msg := mcp.Request{JSONRPC: mcp.Version, Method: mcp.NotificationResourcesUpdated,
		Params: []byte(`{"uri":"gw://alpha/test://x"}`)}

	// Settle the session channel too: publish until one arrives.
	deadline := time.Now().Add(10 * time.Second)
	for got := false; !got; {
		if time.Now().After(deadline) {
			t.Fatal("replica B never received a session message over the bridge")
		}
		if !a.bridge.Notify(tenant, sessionID, msg) {
			t.Fatal("Notify reported the message neither delivered nor published")
		}
		select {
		case frame := <-onB:
			got = strings.Contains(string(frame), "gw://alpha/test://x")
		case <-time.After(100 * time.Millisecond):
		}
	}
	// Drain probes still in flight.
	for drained := false; !drained; {
		select {
		case <-onB:
		case <-time.After(300 * time.Millisecond):
			drained = true
		}
	}

	a.bridge.Notify(tenant, sessionID, msg)
	select {
	case <-onB:
	case <-time.After(5 * time.Second):
		t.Fatal("the session message did not reach replica B")
	}
	select {
	case frame := <-onB:
		t.Fatalf("replica B's stream got the message twice: %s", frame)
	case frame := <-sameIDOtherTenant:
		t.Fatalf("a stream of another tenant with the same session id got it: %s", frame)
	case <-time.After(300 * time.Millisecond):
	}

	// With a stream of its own on A, A delivers locally and publishes
	// nothing.
	onA, unsubA := a.hub.SubscribeSession(tenant, sessionID)
	defer unsubA()
	a.bridge.Notify(tenant, sessionID, msg)
	select {
	case <-onA:
	case <-time.After(time.Second):
		t.Fatal("the local stream was not delivered to")
	}
	select {
	case frame := <-onB:
		t.Fatalf("a message delivered locally was also published: %s", frame)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestBridgeRoutineStopsPromptly(t *testing.T) {
	r := startReplica(t)
	routine := r.bridge.Routine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := routine.Init(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- routine.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	if err := routine.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
	// A second Stop is harmless.
	if err := routine.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
