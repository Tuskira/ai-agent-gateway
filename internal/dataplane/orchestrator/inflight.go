package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/router"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/trace"
)

// Cancellation and progress for tools/call.
//
// A tools/call made on a session is registered here, under its inbound
// JSON-RPC id, for as long as its backend call runs. That is what lets a
// later notifications/cancelled on the same session find it, cancel its
// context (which aborts the backend exchange mid-stream) and tell the
// connector to stop too. The registry is per process: a call lives on the
// replica executing it, so with a shared session store a cancellation
// that lands on another replica finds nothing and is a no-op -- the spec
// makes cancellation best-effort, and docs/architecture.md records the
// limitation.
//
// Progress rides the same bookkeeping: when the inbound call carried
// params._meta.progressToken, the backend's notifications/progress for
// that token are relayed to the session's GET /mcp/stream as they arrive.

const (
	// maxInflightPerSession bounds the registry for one session. A client
	// with more calls than this in flight at once is misbehaving; the
	// excess still runs, it just cannot be cancelled.
	maxInflightPerSession = 1024

	// cancelForwardTimeout bounds the notifications/cancelled the gateway
	// forwards to a connector. It is fire-and-forget, detached from the
	// request that raised it, so it gets its own deadline.
	cancelForwardTimeout = 5 * time.Second
)

// SessionNotifier delivers one notification to the SSE streams a single
// session has open, reporting whether it was delivered: transport.Hub
// reaches the streams on this replica, transport.Bridge also relays to
// the replica holding them. The orchestrator relays progress and
// connector notifications through it when Deps.Notifier also implements
// it.
type SessionNotifier interface {
	Notify(tenantID, sessionID string, msg mcp.Request) bool
}

// cancelledByClient is the cancellation cause recorded when a
// notifications/cancelled stops a call, so the call can tell "the client
// asked" apart from "the client's connection went away".
type cancelledByClient struct{ reason string }

func (c *cancelledByClient) Error() string { return "request cancelled by the client" }

// inflightCall is one tools/call whose backend exchange is running.
type inflightCall struct {
	// ctx carries the call's values (principal, request info) for the
	// forwarded cancellation; it is detached from the call's
	// cancellation so the forward outlives it.
	ctx    context.Context
	cancel context.CancelCauseFunc

	connector  *store.Connector
	upstreamID string
	sessionID  string
	protocol   string
	inbound    *http.Request
	trace      trace.Context
}

type inflightKey struct{ tenantID, sessionID string }

type inflightSession struct {
	calls map[string]*inflightCall
	// warned is set once the session has hit maxInflightPerSession, so
	// the overflow is logged once rather than per call.
	warned bool
}

// inflightRegistry maps (tenant, session, request id) to the call running
// under it.
type inflightRegistry struct {
	mu       sync.Mutex
	sessions map[inflightKey]*inflightSession
}

func newInflightRegistry() *inflightRegistry {
	return &inflightRegistry{sessions: make(map[inflightKey]*inflightSession)}
}

// register records call under id. It reports false (and records nothing)
// when the id is already in flight on the session or the session is at
// its bound; overflowed is true only the first time the bound is hit.
func (r *inflightRegistry) register(tenantID, sessionID, id string, call *inflightCall) (ok, overflowed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := inflightKey{tenantID, sessionID}
	s := r.sessions[key]
	if s == nil {
		s = &inflightSession{calls: make(map[string]*inflightCall)}
		r.sessions[key] = s
	}
	if _, dup := s.calls[id]; dup {
		return false, false
	}
	if len(s.calls) >= maxInflightPerSession {
		first := !s.warned
		s.warned = true
		return false, first
	}
	s.calls[id] = call
	return true, false
}

// remove drops id if it still maps to call.
func (r *inflightRegistry) remove(tenantID, sessionID, id string, call *inflightCall) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := inflightKey{tenantID, sessionID}
	s := r.sessions[key]
	if s == nil || s.calls[id] != call {
		return
	}
	delete(s.calls, id)
	if len(s.calls) == 0 {
		delete(r.sessions, key)
	}
}

// take removes and returns the call registered under id, or nil. Taking
// it is what makes a second cancellation of the same id a no-op.
func (r *inflightRegistry) take(tenantID, sessionID, id string) *inflightCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := inflightKey{tenantID, sessionID}
	s := r.sessions[key]
	if s == nil {
		return nil
	}
	call := s.calls[id]
	if call == nil {
		return nil
	}
	delete(s.calls, id)
	if len(s.calls) == 0 {
		delete(r.sessions, key)
	}
	return call
}

// count reports how many calls are in flight for a session.
func (r *inflightRegistry) count(tenantID, sessionID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[inflightKey{tenantID, sessionID}]; s != nil {
		return len(s.calls)
	}
	return 0
}

// requestKey renders a JSON-RPC id as a registry key. Ids are strings or
// numbers, and 1 and "1" are different ids, so the kind is part of the
// key. A number is keyed through float64, which is how encoding/json
// decodes an inbound mcp.Request.ID, so 7, 7.0 and 7e0 key alike on
// both the tools/call and the notifications/cancelled side.
func requestKey(id any) (string, bool) {
	switch v := id.(type) {
	case string:
		return "s:" + v, true
	case float64:
		return "n:" + mcp.FormatID(v), true
	case int:
		return requestKey(float64(v))
	case int64:
		return requestKey(float64(v))
	default:
		return "", false
	}
}

// rawRequestKey is requestKey for an id still in its JSON form, such as
// notifications/cancelled's requestId or a progressToken.
func rawRequestKey(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	return requestKey(v)
}

// newUpstreamID mints the JSON-RPC id a tools/call is sent upstream
// under. It must be unique within the backend session -- several calls to
// one tool can be in flight on it at once, and a forwarded cancellation
// has to name exactly one of them.
func newUpstreamID(tool string) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "tools/call:" + tool
	}
	return "tools/call:" + tool + ":" + hex.EncodeToString(buf[:])
}

// beginCall prepares a tools/call's backend exchange for cancellation and
// progress: it gives the upstream request a unique id, forwards the
// caller's params._meta, wires the progress relay and the relay of
// server-initiated requests, and registers the call
// under its inbound id. It returns the context the backend call must run
// under and the function that deregisters it, which the caller defers.
func (o *Orchestrator) beginCall(ctx context.Context, in Request, conn *store.Connector, meta json.RawMessage, call *client.Call, tool string) (context.Context, func()) {
	call.RequestID = newUpstreamID(tool)
	call.Meta = meta
	// A request the connector sends inside the reply goes to the agent
	// (relay.go); cancelling the call cancels it too, since it runs under
	// the call's context.
	call.OnRequest = o.callRelay(ctx, in, conn)

	if in.Session == nil || in.JSONRPC == nil || in.JSONRPC.IsNotification() {
		// Without a session there is nothing to address a cancellation
		// or a progress stream by.
		return ctx, func() {}
	}
	tenantID, sessionID := in.Principal.TenantID, in.Session.ID

	call.OnNotification = o.progressRelay(tenantID, sessionID, meta)

	id, ok := requestKey(in.JSONRPC.ID)
	if !ok {
		return ctx, func() {}
	}

	callCtx, cancel := context.WithCancelCause(ctx)
	entry := &inflightCall{
		ctx:        context.WithoutCancel(ctx),
		cancel:     cancel,
		connector:  conn,
		upstreamID: call.RequestID.(string),
		sessionID:  call.SessionID,
		protocol:   call.ProtocolVersion,
		inbound:    in.Inbound,
		trace:      in.Trace,
	}

	registered, overflowed := o.inflight.register(tenantID, sessionID, id, entry)
	if !registered {
		if overflowed {
			o.log.Warn("too many tools/calls in flight on one session; further calls cannot be cancelled",
				"session_id", sessionID, "limit", maxInflightPerSession)
		}
		cancel(nil)
		return ctx, func() {}
	}

	return callCtx, func() {
		o.inflight.remove(tenantID, sessionID, id, entry)
		cancel(nil)
	}
}

// progressRelay returns the OnNotification callback that relays the
// backend's notifications/progress for the caller's token to the
// session's SSE stream, or nil when there is nothing to relay: no token
// on the request, or no per-session delivery configured.
func (o *Orchestrator) progressRelay(tenantID, sessionID string, meta json.RawMessage) func(mcp.Request) {
	notifier, ok := o.deps.Notifier.(SessionNotifier)
	if !ok || len(meta) == 0 {
		return nil
	}
	var m mcp.RequestMeta
	if err := json.Unmarshal(meta, &m); err != nil {
		return nil
	}
	token, ok := rawRequestKey(m.ProgressToken)
	if !ok {
		return nil
	}

	return func(n mcp.Request) {
		if n.Method != mcp.NotificationProgress {
			return
		}
		var p mcp.ProgressParams
		if err := json.Unmarshal(n.Params, &p); err != nil {
			return
		}
		// Only the caller's own token: a backend reporting on some
		// other request of its session must not leak into this one.
		if got, ok := rawRequestKey(p.ProgressToken); !ok || got != token {
			return
		}
		if !notifier.Notify(tenantID, sessionID, mcp.Request{
			JSONRPC: mcp.Version, Method: mcp.NotificationProgress, Params: n.Params,
		}) {
			o.log.Debug("dropping progress: the session has no stream open", "session_id", sessionID)
		}
	}
}

// handleCancelled serves notifications/cancelled: it stops the named
// tools/call if it is in flight on this session on this replica, and is
// a no-op otherwise -- the spec lets a receiver ignore a cancellation for
// a request it does not know or has already finished.
func (o *Orchestrator) handleCancelled(_ context.Context, in Request) Result {
	if in.Session == nil {
		o.log.Debug("ignoring notifications/cancelled without a session")
		return Result{}
	}
	var params mcp.CancelledParams
	if err := decodeParams(in.JSONRPC.Params, &params); err != nil {
		o.log.Debug("ignoring malformed notifications/cancelled", "error", err)
		return Result{}
	}
	id, ok := rawRequestKey(params.RequestID)
	if !ok {
		o.log.Debug("ignoring notifications/cancelled with no usable requestId")
		return Result{}
	}

	entry := o.inflight.take(in.Principal.TenantID, in.Session.ID, id)
	if entry == nil {
		o.log.Debug("notifications/cancelled names no call in flight here",
			"session_id", in.Session.ID, "request_id", string(params.RequestID))
		return Result{}
	}

	o.log.Info("tools/call cancelled by the client",
		"session_id", in.Session.ID, "request_id", string(params.RequestID),
		"connector_id", entry.connector.ID, "reason", params.Reason)

	// Record the cancellation on our own side FIRST, and only then tell
	// the connector. forwardCancel runs on its own goroutine, and the Go
	// memory model only guarantees its effects follow entry.cancel if
	// entry.cancel completed before the "go" statement that launches it
	// -- never the reverse. A connector that honours a forwarded
	// cancellation may close its streamed reply immediately (a
	// spec-compliant "send no response"); if that closure reached
	// CallTool before entry.cancel had actually recorded
	// context.Cause(callCtx), clientCancelled (below, via toolCallFailure)
	// found no cause yet and reported a generic tool-call failure --
	// wrongly marking the connector unhealthy too -- instead of -32800.
	// Cancelling first closes that window: forwardCancel still runs on
	// its own detached context and still reaches the connector, but our
	// own bookkeeping is now guaranteed to already reflect the
	// cancellation before it can have any observable upstream effect.
	entry.cancel(&cancelledByClient{reason: params.Reason})
	go o.forwardCancel(entry, params.Reason)
	return Result{}
}

// forwardCancelStarted, when non-nil, is invoked synchronously the
// instant forwardCancel begins, before any I/O. Tests use it to prove
// the ordering handleCancelled depends on: entry.cancel must already
// have recorded the call's cancellation cause before forwardCancel --
// which runs on its own goroutine -- can have any observable effect on
// the connector.
var forwardCancelStarted func()

// forwardCancel sends notifications/cancelled for the upstream request to
// the connector serving it. Best-effort: a connector that does not
// implement cancellation ignores it or answers an error, and either way
// the gateway's own side of the call is already stopped.
func (o *Orchestrator) forwardCancel(entry *inflightCall, reason string) {
	if forwardCancelStarted != nil {
		forwardCancelStarted()
	}
	ctx, cancel := context.WithTimeout(entry.ctx, cancelForwardTimeout)
	defer cancel()

	err := o.deps.Client.SendCancelled(ctx, client.Call{
		Connector:       entry.connector,
		SessionID:       entry.sessionID,
		ProtocolVersion: entry.protocol,
		Inbound:         entry.inbound,
		Trace:           entry.trace,
	}, entry.upstreamID, reason)
	if err != nil {
		o.log.Debug("forwarding notifications/cancelled to the connector failed",
			"connector_id", entry.connector.ID, "error", err)
	}
}

// clientCancelled reports whether a failed backend call failed because
// the client cancelled it, and if so the answer to give: -32800, with the
// connector left healthy and its backend session kept (nothing is wrong
// with either).
func (o *Orchestrator) clientCancelled(callCtx context.Context, in Request, route *router.Route, qualified string) (Result, bool) {
	var cause *cancelledByClient
	if !errors.As(context.Cause(callCtx), &cause) {
		return Result{}, false
	}
	o.log.Debug("tool call stopped by notifications/cancelled", "tool", qualified, "connector_id", route.Connector.ID)

	var data any
	if cause.reason != "" {
		data = map[string]string{"reason": cause.reason}
	}
	return Result{
		Response:    mcp.NewErrorResponse(in.JSONRPC.ID, mcp.NewError(mcp.ErrorCodeRequestCancelled, "request cancelled", data)),
		ConnectorID: route.Connector.ID,
	}, true
}
