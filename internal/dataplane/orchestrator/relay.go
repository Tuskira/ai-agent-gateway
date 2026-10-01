package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/reqctx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/upstream"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/trace"
)

// The relay of server-initiated requests.
//
// An MCP server may ask its client for something mid-conversation:
// sampling/createMessage (run a completion on the client's model),
// elicitation/create (ask the human for input), roots/list (which
// directories is the client working in). The gateway is the connector's
// client, but the answer can only come from the agent behind it, so the
// gateway relays: the request goes to the agent's GET /mcp/stream as a
// JSON-RPC request under a gateway-minted id ("gw-<16 hex>"), the agent
// POSTs its JSON-RPC response to /mcp, and the answer goes back to the
// connector under the connector's own id.
//
// Two gates stand in front of the agent, and both must pass:
//
//   - the connector's policy (metadata.server_requests, all off by
//     default): whoever configured the connector decides whether it may
//     spend the agent's model budget or talk to its human;
//   - the agent's own initialize: a client that did not declare
//     sampling, elicitation or roots is never sent that request.
//
// The same intersection is what the gateway declares to the connector at
// its handshake, so a well-behaved connector never asks for anything the
// relay would refuse; the relay re-checks anyway, since a connector's
// policy can change under an open backend session and a connector need
// not be well-behaved.
//
// A request arrives on one of two paths: inside the streamed reply to a
// tools/call (a tool that needs the agent mid-call; client.Call.OnRequest)
// or on the connector's long-lived stream (upstream.RequestHandler), which
// the handshake opens whenever the intersection is non-empty. Either way
// the pending entry lives on the replica holding the call or the stream;
// an agent response POSTed to another replica is relayed to it over the
// session notifier (transport.Bridge).

const (
	// DefaultMaxPendingServerRequestsPerSession is
	// Deps.MaxPendingServerRequestsPerSession's default.
	DefaultMaxPendingServerRequestsPerSession = 32
	// DefaultServerRequestTimeout is Deps.ServerRequestTimeout's default.
	DefaultServerRequestTimeout = 5 * time.Minute

	// serverRequestsNeed is the upstream need a session holds on a
	// connector's stream while it may carry server-initiated requests.
	serverRequestsNeed = "server-requests"
)

// serverRequestCapability maps each relayed method to the client
// capability that invites it. A method not in the map is not relayed.
var serverRequestCapability = map[string]string{
	"sampling/createMessage": "sampling",
	"elicitation/create":     "elicitation",
	"roots/list":             "roots",
}

// RequestDeliverer is implemented by a SessionNotifier that can tell
// whether a server-initiated request reached a stream of the session on
// ANY replica: transport.Hub reports its own streams, transport.Bridge
// also waits for the replica holding one to acknowledge it. Without it
// the relay takes SessionNotifier.Notify's report as-is.
type RequestDeliverer interface {
	DeliverRequest(ctx context.Context, tenantID, sessionID string, msg mcp.Request) bool
}

// ResponseRelay is implemented by a notifier that reaches other replicas
// (transport.Bridge): an agent's response that matches nothing pending
// on this replica is published for the one that holds it, which hands it
// to DeliverRemoteResponse.
type ResponseRelay interface {
	RelayResponse(tenantID, sessionID string, raw json.RawMessage)
}

// advertisedCapabilities is what the gateway declares to conn, as its
// client, on behalf of the agent session in backends: exactly the
// capabilities the agent declared AND the connector's policy allows,
// each as the agent wrote it. A request with no session (and the
// anonymous handles it shares) declares none.
func advertisedCapabilities(backends session.Backends, conn *store.Connector) map[string]json.RawMessage {
	sess, ok := backends.(*session.Session)
	if !ok || sess == nil || len(sess.ClientCapabilities) == 0 {
		return nil
	}
	policy := client.ServerRequests(conn)
	var out map[string]json.RawMessage
	for method, capability := range serverRequestCapability {
		raw, declared := sess.ClientCapabilities[capability]
		if !declared || !policy.Allows(method) {
			continue
		}
		if out == nil {
			out = make(map[string]json.RawMessage, len(serverRequestCapability))
		}
		out[capability] = raw
	}
	return out
}

// declaredCapabilities picks, out of an agent's initialize
// params.capabilities, the ones that invite server-initiated requests,
// raw. A capability sent as JSON null is not declared.
func declaredCapabilities(params json.RawMessage) map[string]json.RawMessage {
	var p struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if len(params) == 0 || json.Unmarshal(params, &p) != nil {
		return nil
	}
	var out map[string]json.RawMessage
	for _, capability := range serverRequestCapability {
		raw, ok := p.Capabilities[capability]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		if out == nil {
			out = make(map[string]json.RawMessage, len(serverRequestCapability))
		}
		out[capability] = raw
	}
	return out
}

// ---------------------------------------------------------------------------
// the pending table
// ---------------------------------------------------------------------------

// pendingRequest is one relayed request awaiting the agent's answer.
type pendingRequest struct {
	id    string
	sess  upstream.SessionRef
	reply chan pendingOutcome // buffered 1; written once, by whoever removes the entry
}

type pendingOutcome struct {
	resp *mcp.Response
	// ended is set when the session ended under the request.
	ended bool
}

// answer is what the connector is told: the agent's result or error,
// relayed as the agent sent it.
func (out pendingOutcome) answer() (json.RawMessage, *mcp.Error) {
	switch {
	case out.ended:
		return nil, mcp.NewInternalError("client session has ended")
	case out.resp.Error != nil:
		return nil, out.resp.Error
	case len(out.resp.Result) == 0:
		return json.RawMessage(`{}`), nil
	default:
		return out.resp.Result, nil
	}
}

// pendingTable is every relayed request awaiting an answer on this
// replica, by gateway id, with a per-session bound.
type pendingTable struct {
	mu        sync.Mutex
	byID      map[string]*pendingRequest
	bySession map[upstream.SessionRef]map[string]*pendingRequest
}

func newPendingTable() *pendingTable {
	return &pendingTable{
		byID:      make(map[string]*pendingRequest),
		bySession: make(map[upstream.SessionRef]map[string]*pendingRequest),
	}
}

// add records a new pending request for sess, or reports false when the
// session already has limit of them.
func (t *pendingTable) add(sess upstream.SessionRef, limit int) (*pendingRequest, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.bySession[sess]) >= limit {
		return nil, false
	}
	p := &pendingRequest{id: newGatewayRequestID(), sess: sess, reply: make(chan pendingOutcome, 1)}
	t.byID[p.id] = p
	if t.bySession[sess] == nil {
		t.bySession[sess] = make(map[string]*pendingRequest)
	}
	t.bySession[sess][p.id] = p
	return p, true
}

// removeLocked unregisters p. t.mu must be held.
func (t *pendingTable) removeLocked(p *pendingRequest) {
	delete(t.byID, p.id)
	if bySess := t.bySession[p.sess]; bySess != nil {
		delete(bySess, p.id)
		if len(bySess) == 0 {
			delete(t.bySession, p.sess)
		}
	}
}

// remove unregisters p, reporting whether it was still registered -- false
// means an answer (or the session's end) already claimed it.
func (t *pendingTable) remove(p *pendingRequest) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byID[p.id] != p {
		return false
	}
	t.removeLocked(p)
	return true
}

// resolve hands resp to the request pending under id, provided it
// belongs to sess: an answer POSTed on one session never settles
// another's request, whoever guessed the id. It reports whether an entry
// took it.
func (t *pendingTable) resolve(sess upstream.SessionRef, id string, resp *mcp.Response) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.byID[id]
	if p == nil || p.sess != sess {
		return false
	}
	t.removeLocked(p)
	p.reply <- pendingOutcome{resp: resp}
	return true
}

// has reports whether id is pending here, for any session.
func (t *pendingTable) has(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byID[id] != nil
}

// endSession fails every request pending for sess.
func (t *pendingTable) endSession(sess upstream.SessionRef) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range t.bySession[sess] {
		delete(t.byID, p.id)
		p.reply <- pendingOutcome{ended: true}
	}
	delete(t.bySession, sess)
}

// count reports how many requests are pending for sess.
func (t *pendingTable) count(sess upstream.SessionRef) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.bySession[sess])
}

// newGatewayRequestID mints the id a relayed request is sent to the
// agent under. It never reuses the connector's id: two connectors (or
// two requests of one) may pick the same, and the agent's answer has to
// name exactly one pending entry.
func newGatewayRequestID() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:]) // crypto/rand.Read does not fail
	return "gw-" + hex.EncodeToString(buf[:])
}

// ---------------------------------------------------------------------------
// the relay
// ---------------------------------------------------------------------------

// relaySource is where a server-initiated request came from and whose
// agent session it is for.
type relaySource struct {
	sess      upstream.SessionRef
	caps      map[string]json.RawMessage
	conn      *store.Connector
	principal *pkgauth.Principal
	trace     trace.Context
	// correlationID is the tools/call's, for a request that arrived
	// inside its reply; empty for one from the connector's stream.
	correlationID string
}

// callRelay returns the client.Call.OnRequest of a tools/call: every
// request the connector sends inside its reply is relayed to the calling
// session's agent. ctx is the call's; the relayed request lives no longer
// than the call.
func (o *Orchestrator) callRelay(ctx context.Context, in Request, conn *store.Connector) func(context.Context, mcp.Request) (json.RawMessage, *mcp.Error) {
	src := relaySource{conn: conn, principal: in.Principal, trace: in.Trace}
	if in.Session != nil {
		src.sess = upstream.SessionRef{TenantID: in.Principal.TenantID, SessionID: in.Session.ID}
		src.caps = in.Session.ClientCapabilities
	}
	if info := reqctx.From(ctx); info != nil {
		src.correlationID = info.CorrelationID
	}
	return func(ctx context.Context, req mcp.Request) (json.RawMessage, *mcp.Error) {
		return o.relay(ctx, src, req)
	}
}

// HandleRequest implements upstream.RequestHandler: a request a connector
// sent on its long-lived stream, relayed to the session the stream is
// held for. The session and the connector are re-read, so a policy
// turned off, or a session ended, since the stream opened is honoured.
func (o *Orchestrator) HandleRequest(ctx context.Context, sess upstream.SessionRef, ref upstream.ConnectorRef, req mcp.Request) (json.RawMessage, *mcp.Error) {
	principal, _ := pkgauth.PrincipalFrom(ctx)
	src := relaySource{sess: sess, principal: principal, conn: &store.Connector{ID: ref.ID, Slug: ref.Slug}}

	conn, err := o.deps.Connectors.Get(ctx, sess.TenantID, ref.ID)
	if err != nil {
		o.log.Warn("failed to load the connector behind a server-initiated request", "connector_id", ref.ID, "error", err)
		return o.refuse(src, req.Method, mcp.NewInternalError("failed to load connector"))
	}
	src.conn = conn

	s, err := o.deps.Sessions.Peek(ctx, sess.SessionID, sess.TenantID)
	if errors.Is(err, session.ErrNotFound) {
		return o.refuse(src, req.Method, mcp.NewInternalError("client session has ended"))
	}
	if err != nil {
		o.log.Warn("failed to load the session behind a server-initiated request", "session_id", sess.SessionID, "error", err)
		return o.refuse(src, req.Method, mcp.NewInternalError("failed to load client session"))
	}
	src.caps = s.ClientCapabilities
	return o.relay(ctx, src, req)
}

// refuse answers a request without relaying it, and logs it.
func (o *Orchestrator) refuse(src relaySource, method string, rpcErr *mcp.Error) (json.RawMessage, *mcp.Error) {
	o.logServerRequest(src, method, "", time.Now(), rpcErr)
	return nil, rpcErr
}

// relay forwards one server-initiated request to src's agent and waits
// for its answer, which it returns as the connector's result or error.
func (o *Orchestrator) relay(ctx context.Context, src relaySource, req mcp.Request) (json.RawMessage, *mcp.Error) {
	start := time.Now()
	id, result, rpcErr := o.relayRequest(ctx, src, req)
	o.logServerRequest(src, req.Method, id, start, rpcErr)
	return result, rpcErr
}

func (o *Orchestrator) relayRequest(ctx context.Context, src relaySource, req mcp.Request) (string, json.RawMessage, *mcp.Error) {
	capability, supported := serverRequestCapability[req.Method]
	switch {
	case !supported:
		return "", nil, mcp.NewMethodNotFoundError(req.Method)
	case !client.ServerRequests(src.conn).Allows(req.Method):
		o.log.Info("refusing a server-initiated request the connector's policy does not permit",
			"connector_id", src.conn.ID, "method", req.Method)
		return "", nil, mcp.NewError(mcp.ErrorCodeMethodNotFound,
			req.Method+" is not permitted for this connector", map[string]any{"method": req.Method})
	case src.caps[capability] == nil:
		o.log.Debug("refusing a server-initiated request the client did not declare",
			"connector_id", src.conn.ID, "method", req.Method, "session_id", src.sess.SessionID)
		return "", nil, mcp.NewError(mcp.ErrorCodeMethodNotFound,
			"client did not declare "+capability, map[string]any{"method": req.Method})
	}

	p, ok := o.pending.add(src.sess, o.maxPending)
	if !ok {
		o.log.Warn("too many server-initiated requests pending on one session; refusing one",
			"session_id", src.sess.SessionID, "connector_id", src.conn.ID, "limit", o.maxPending)
		return "", nil, mcp.NewError(mcp.ErrorCodeInternalError,
			"too many requests pending for this client", map[string]any{"limit": o.maxPending})
	}

	// The params travel as the connector sent them, never decoded and
	// re-encoded (only insignificant whitespace may be compacted on the
	// way out): what a connector asks a model or a human is its business.
	msg := mcp.Request{JSONRPC: mcp.Version, ID: p.id, Method: req.Method, Params: req.Params}
	if !o.deliverRequest(ctx, src.sess, msg) {
		o.pending.remove(p)
		return p.id, nil, mcp.NewInternalError("client has no open stream")
	}

	timer := time.NewTimer(o.requestTimeout)
	defer timer.Stop()

	select {
	case out := <-p.reply:
		result, rpcErr := out.answer()
		return p.id, result, rpcErr

	case <-timer.C:
		if !o.pending.remove(p) {
			// An answer claimed it just as the wait ran out.
			result, rpcErr := (<-p.reply).answer()
			return p.id, result, rpcErr
		}
		o.cancelAtAgent(src.sess, p.id, "timed out waiting for the client")
		return p.id, nil, mcp.NewInternalError("timed out waiting for the client")

	case <-ctx.Done():
		if !o.pending.remove(p) {
			result, rpcErr := (<-p.reply).answer()
			return p.id, result, rpcErr
		}
		// The tools/call it arose from was cancelled or ended, or the
		// connector's stream closed: nobody is waiting for the answer
		// any more, so the agent should stop working on it.
		o.cancelAtAgent(src.sess, p.id, "the request was cancelled")
		return p.id, nil, mcp.NewError(mcp.ErrorCodeRequestCancelled, "request cancelled", nil)
	}
}

// deliverRequest writes msg to the session's stream, reporting whether
// some stream -- on this replica, or, through a RequestDeliverer, on
// another -- took it.
func (o *Orchestrator) deliverRequest(ctx context.Context, sess upstream.SessionRef, msg mcp.Request) bool {
	if d, ok := o.deps.Notifier.(RequestDeliverer); ok {
		return d.DeliverRequest(ctx, sess.TenantID, sess.SessionID, msg)
	}
	if n, ok := o.deps.Notifier.(SessionNotifier); ok {
		return n.Notify(sess.TenantID, sess.SessionID, msg)
	}
	return false
}

// cancelAtAgent tells the agent to stop working on a relayed request
// nobody will read the answer to. Best-effort, like any notification.
func (o *Orchestrator) cancelAtAgent(sess upstream.SessionRef, id, reason string) {
	n, ok := o.deps.Notifier.(SessionNotifier)
	if !ok {
		return
	}
	raw, err := json.Marshal(mcp.CancelledParams{RequestID: json.RawMessage(strconv.Quote(id)), Reason: reason})
	if err != nil {
		return
	}
	n.Notify(sess.TenantID, sess.SessionID, mcp.Request{JSONRPC: mcp.Version, Method: mcp.NotificationCancelled, Params: raw})
}

// HandleClientResponse takes a JSON-RPC response an agent POSTed on its
// session -- its answer to a relayed request. One pending on this replica
// is settled; any other is published for the replica holding it when the
// notifier reaches other replicas, and dropped otherwise (an answer
// arriving after its request timed out, or to an id never issued).
func (o *Orchestrator) HandleClientResponse(_ context.Context, tenantID, sessionID string, raw json.RawMessage) {
	if o.resolveResponse(tenantID, sessionID, raw) {
		return
	}
	if r, ok := o.deps.Notifier.(ResponseRelay); ok {
		r.RelayResponse(tenantID, sessionID, raw)
		return
	}
	o.log.Debug("dropping a client response that matches no pending request", "session_id", sessionID)
}

// DeliverRemoteResponse settles a request pending on this replica with a
// response another replica received and relayed. It never re-publishes.
func (o *Orchestrator) DeliverRemoteResponse(tenantID, sessionID string, raw json.RawMessage) {
	o.resolveResponse(tenantID, sessionID, raw)
}

func (o *Orchestrator) resolveResponse(tenantID, sessionID string, raw json.RawMessage) bool {
	var resp mcp.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		o.log.Debug("dropping a malformed client response", "session_id", sessionID, "error", err)
		return false
	}
	id, ok := resp.ID.(string)
	if !ok {
		return false
	}
	sess := upstream.SessionRef{TenantID: tenantID, SessionID: sessionID}
	if o.pending.resolve(sess, id, &resp) {
		return true
	}
	if o.pending.has(id) {
		// Someone else's request: ignored, and not published either --
		// its owner is this replica, and it did not come from its
		// session.
		o.log.Warn("ignoring a client response for a request pending on another session", "session_id", sessionID)
		return true
	}
	return false
}

// logServerRequest writes the access-log record of one server-initiated
// request: which connector asked what, how it ended, and how long the
// agent took. Params and results are never logged -- they are prompts,
// completions and whatever a human typed into a form.
func (o *Orchestrator) logServerRequest(src relaySource, method, id string, start time.Time, rpcErr *mcp.Error) {
	if o.deps.Sink == nil {
		return
	}
	record := &sink.AccessLog{
		Timestamp:     start,
		TenantID:      src.sess.TenantID,
		SessionID:     src.sess.SessionID,
		RequestID:     id,
		CorrelationID: src.correlationID,
		TraceID:       src.trace.TraceID,
		Method:        method,
		JSONRPCID:     id,
		StatusCode:    200,
		DurationMS:    time.Since(start).Milliseconds(),
	}
	if src.conn != nil {
		record.ConnectorID = src.conn.ID
		record.ToolName = client.Qualifier(src.conn)
	}
	if src.principal != nil {
		record.TenantID = src.principal.TenantID
		record.Principal = src.principal.Subject
		record.KeyID = src.principal.KeyID
	}
	if rpcErr != nil {
		record.ErrorCode = strconv.Itoa(rpcErr.Code)
	}
	o.deps.Sink.WriteAccess(record)
}

// openServerRequestStream holds the connector's long-lived stream open
// for sess when the gateway just declared it any capability, so a request
// the connector sends outside a call's reply has somewhere to arrive. A
// connector with no stream is not an error: its requests can still come
// inside a reply.
func (o *Orchestrator) openServerRequestStream(ctx context.Context, in Request, sess *session.Session, conn *store.Connector, backend session.Backend) {
	target := in
	target.Session = sess
	err := o.upstream.Acquire(ctx, o.streamTarget(target, conn, backend), serverRequestsNeed)
	switch {
	case err == nil:
	case errors.Is(err, upstream.ErrStreamUnsupported):
		o.log.Debug("connector has no stream; its server-initiated requests can only arrive inside a call's reply",
			"connector_id", conn.ID)
	default:
		o.log.Warn("failed to open the connector stream for server-initiated requests",
			"connector_id", conn.ID, "session_id", sess.ID, "error", err)
	}
}
