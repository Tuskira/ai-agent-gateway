package transport

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/supervisor"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	pkgsession "github.com/Tuskira/tusk-ai-secured-gateway/pkg/session"
)

// publishTimeout bounds one Publish. The signal is fire-and-forget and
// must not be cancelled with the request that happened to raise it, so
// it gets its own deadline rather than the caller's context.
const publishTimeout = 5 * time.Second

// ackTimeout bounds the wait for another replica to acknowledge that it
// wrote a relayed server-initiated request to the session's stream. A
// replica holding the stream answers in a Redis round trip; silence this
// long means no replica holds one.
const ackTimeout = 2 * time.Second

// Bridge carries notifications between mcp replicas over a
// pkg/session.Notifier: the tenant-wide tools/list_changed, and messages
// for one session's stream.
//
// The Hub stays local: an SSE stream is a socket on one replica, so only
// that replica can write to it. The Bridge makes delivery cluster-wide.
// ToolsListChanged signals the local Hub directly and publishes the
// tenant through the Notifier; every replica's subscriber (Routine)
// signals its own Hub on receipt. Notify delivers to the session's local
// streams and, only when there are none here, publishes the message for
// the replica that holds them -- which is how a resource update read off
// a connector stream on the replica that served resources/subscribe
// reaches an agent whose GET /mcp/stream landed on another. The Notifier
// contract (a publish is never delivered to the publishing instance's
// own subscriber) is what keeps a stream from being told twice.
//
// Delivery is the Notifier's: best-effort, to replicas subscribed at that
// moment. A missed signal costs an agent a stale tool list until its next
// tools/list, which is what the signal only ever shortened; a missed
// session message is lost, as one to a dropped stream is.
//
// A server-initiated request relayed to an agent (DeliverRequest) needs
// more than best-effort: the replica waiting on it must fail it fast when
// no stream exists anywhere. So the replica that writes it to a stream
// acknowledges it (pkg/session.KindAck), and DeliverRequest waits for
// that. The agent's answer may be POSTed to any replica; one that holds
// no pending entry for it publishes it (RelayResponse), and the replica
// that does settles it through the handler set with OnResponse.
type Bridge struct {
	hub      *Hub
	notifier pkgsession.Notifier
	log      *slog.Logger

	mu         sync.Mutex
	acks       map[string]ackWaiter // relayed request id -> its waiter
	onResponse func(tenantID, sessionID string, raw json.RawMessage)
}

type ackWaiter struct {
	tenantID, sessionID string
	acked               chan struct{}
}

// NewBridge returns a Bridge that relays hub's signals through notifier.
func NewBridge(hub *Hub, notifier pkgsession.Notifier, logger *slog.Logger) *Bridge {
	if logger == nil {
		logger = slog.Default()
	}
	return &Bridge{hub: hub, notifier: notifier, log: logger, acks: make(map[string]ackWaiter)}
}

// OnResponse sets the handler that receives an agent's response another
// replica relayed (orchestrator.DeliverRemoteResponse). Set it before the
// Routine runs.
func (b *Bridge) OnResponse(fn func(tenantID, sessionID string, raw json.RawMessage)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onResponse = fn
}

// ToolsListChanged signals the local streams for tenantID and publishes
// the signal for every other replica. It implements
// orchestrator.Notifier.
func (b *Bridge) ToolsListChanged(tenantID string) {
	b.hub.ToolsListChanged(tenantID)

	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	if err := b.notifier.Publish(ctx, tenantID); err != nil {
		b.log.Warn("failed to publish tools/list_changed to other replicas", "tenant_id", tenantID, "error", err)
	}
}

// Notify delivers a per-session message to the session's streams on
// this replica, and publishes it for the other replicas only when there
// is none here: a session's streams normally sit on one replica, so a
// message delivered locally has nowhere else to go. It reports whether
// the message was delivered or published. The publish is synchronous, so
// messages for one session leave in the order they were raised. It
// implements orchestrator.SessionNotifier.
func (b *Bridge) Notify(tenantID, sessionID string, msg mcp.Request) bool {
	raw, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	if b.hub.deliver(tenantID, sessionID, raw) {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	err = b.notifier.PublishSession(ctx, pkgsession.SessionMessage{TenantID: tenantID, SessionID: sessionID, Payload: raw})
	if err != nil {
		b.log.Warn("failed to publish a session notification to other replicas",
			"session_id", sessionID, "method", msg.Method, "error", err)
		return false
	}
	return true
}

// DeliverRequest writes a server-initiated request to the session's
// streams: this replica's, or, when it holds none, another's -- in which
// case it waits up to ackTimeout (or ctx) for that replica to confirm it
// did. It reports whether some stream took the request. It implements
// orchestrator.RequestDeliverer.
func (b *Bridge) DeliverRequest(ctx context.Context, tenantID, sessionID string, msg mcp.Request) bool {
	raw, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	if b.hub.deliver(tenantID, sessionID, raw) {
		return true
	}
	id, ok := msg.ID.(string)
	if !ok {
		return false
	}

	w := ackWaiter{tenantID: tenantID, sessionID: sessionID, acked: make(chan struct{})}
	b.mu.Lock()
	b.acks[id] = w
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.acks, id)
		b.mu.Unlock()
	}()

	pubCtx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	err = b.notifier.PublishSession(pubCtx, pkgsession.SessionMessage{
		TenantID: tenantID, SessionID: sessionID, Kind: pkgsession.KindRequest, Payload: raw,
	})
	cancel()
	if err != nil {
		b.log.Warn("failed to publish a server-initiated request to other replicas",
			"session_id", sessionID, "method", msg.Method, "error", err)
		return false
	}

	timer := time.NewTimer(ackTimeout)
	defer timer.Stop()
	select {
	case <-w.acked:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// RelayResponse publishes an agent's response that matched nothing
// pending on this replica, for the replica holding its request. It
// implements orchestrator.ResponseRelay.
func (b *Bridge) RelayResponse(tenantID, sessionID string, raw json.RawMessage) {
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	err := b.notifier.PublishSession(ctx, pkgsession.SessionMessage{
		TenantID: tenantID, SessionID: sessionID, Kind: pkgsession.KindResponse, Payload: raw,
	})
	if err != nil {
		b.log.Warn("failed to publish a client response to other replicas", "session_id", sessionID, "error", err)
	}
}

// deliverRemote acts on a session message another replica published: a
// notification or request goes to this replica's streams for that
// session, if it holds any (a request is acknowledged when one took it);
// an acknowledgement releases the DeliverRequest waiting on it; a
// response goes to the OnResponse handler, which settles it if its
// request is pending here.
func (b *Bridge) deliverRemote(m pkgsession.SessionMessage) {
	switch m.Kind {
	case pkgsession.KindRequest:
		if !b.hub.deliver(m.TenantID, m.SessionID, m.Payload) {
			return
		}
		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(m.Payload, &probe) != nil || len(probe.ID) == 0 {
			return
		}
		// Off the subscriber's goroutine, which must return promptly.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
			defer cancel()
			err := b.notifier.PublishSession(ctx, pkgsession.SessionMessage{
				TenantID: m.TenantID, SessionID: m.SessionID, Kind: pkgsession.KindAck, Payload: probe.ID,
			})
			if err != nil {
				b.log.Warn("failed to acknowledge a relayed request", "session_id", m.SessionID, "error", err)
			}
		}()

	case pkgsession.KindAck:
		var id string
		if json.Unmarshal(m.Payload, &id) != nil {
			return
		}
		b.mu.Lock()
		w, ok := b.acks[id]
		if ok && w.tenantID == m.TenantID && w.sessionID == m.SessionID {
			delete(b.acks, id)
			close(w.acked)
		}
		b.mu.Unlock()

	case pkgsession.KindResponse:
		b.mu.Lock()
		fn := b.onResponse
		b.mu.Unlock()
		if fn != nil {
			fn(m.TenantID, m.SessionID, m.Payload)
		}

	case pkgsession.KindNotification:
		b.hub.deliver(m.TenantID, m.SessionID, m.Payload)

	default:
		// A kind a newer replica sends that this one does not know:
		// dropped, rather than written to a stream as if it were a
		// notification.
	}
}

// Routine returns the subscriber as a supervisor.Routine: it runs
// Notifier.Subscribe and Notifier.SubscribeSessions until stopped,
// handing each delivery to the local Hub, and re-subscribes either after
// a transport failure.
func (b *Bridge) Routine() supervisor.Routine {
	return &bridgeRoutine{bridge: b, done: make(chan struct{})}
}

// resubscribeDelay is the pause before Subscribe is retried after it
// returned an error.
const resubscribeDelay = time.Second

type bridgeRoutine struct {
	bridge *Bridge
	done   chan struct{}
	once   sync.Once
}

func (r *bridgeRoutine) Name() string { return "mcp-notify-bridge" }

func (r *bridgeRoutine) Init(context.Context) error { return nil }

func (r *bridgeRoutine) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-r.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	b := r.bridge
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.hold(ctx, "tools/list_changed", func(ctx context.Context) error {
			return b.notifier.Subscribe(ctx, b.hub.ToolsListChanged)
		})
	}()
	go func() {
		defer wg.Done()
		r.hold(ctx, "session message", func(ctx context.Context) error {
			return b.notifier.SubscribeSessions(ctx, b.deliverRemote)
		})
	}()
	wg.Wait()
	return nil
}

// hold runs subscribe until ctx is done, re-subscribing after a failure.
func (r *bridgeRoutine) hold(ctx context.Context, kind string, subscribe func(context.Context) error) {
	for {
		err := subscribe(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			r.bridge.log.Warn("cross-replica subscription failed; re-subscribing", "kind", kind, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(resubscribeDelay):
		}
	}
}

func (r *bridgeRoutine) Stop(context.Context) error {
	r.once.Do(func() { close(r.done) })
	return nil
}
