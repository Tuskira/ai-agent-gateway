package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/cache"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/router"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/upstream"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Resource subscriptions and the relay of what connectors push.
//
// resources/subscribe takes a gateway URI ("gw://<connector>/<uri>"),
// applies the same profile rule as resources/read, makes sure the session
// holds a server-to-client stream to that connector (upstream.Manager),
// and forwards the subscribe with the connector's ORIGINAL uri. The
// subscription is recorded per agent session; an update the connector
// pushes for it is relayed to that session's GET /mcp/stream with the uri
// re-namespaced. An update for anything the session did not subscribe to
// is dropped.
//
// Each subscription holds its own need on the stream, so the stream
// closes exactly when the session's last subscription to that connector
// goes away -- or the session does.
//
// The same streams carry list-changed notifications, relayed as they
// arrive: prompts/resources list_changed to the session whose stream it
// was, tools/list_changed as a refresh of that connector's cached tools
// and the tenant-wide signal the tool cache already sends.

const (
	// DefaultMaxSubscriptionsPerSession is Deps.MaxSubscriptionsPerSession's
	// default.
	DefaultMaxSubscriptionsPerSession = 256

	// toolsRefreshTimeout bounds the re-list of one connector's tools
	// after it announced tools/list_changed.
	toolsRefreshTimeout = 30 * time.Second
)

// subscriptionNeed is the upstream need one subscription holds on its
// connector's stream.
func subscriptionNeed(uri string) string { return mcp.MethodResourcesSubscribe + " " + uri }

// subscriptionRegistry is every session's subscriptions, by connector.
type subscriptionRegistry struct {
	mu       sync.Mutex
	sessions map[upstream.SessionRef]*sessionSubscriptions
}

type sessionSubscriptions struct {
	// byConnector maps connector id to the backend URIs subscribed on
	// it; slugs maps each connector's slug back to its id, since
	// unsubscribe names the connector by slug.
	byConnector map[string]map[string]struct{}
	slugs       map[string]string
	count       int
}

func newSubscriptionRegistry() *subscriptionRegistry {
	return &subscriptionRegistry{sessions: make(map[upstream.SessionRef]*sessionSubscriptions)}
}

func (r *subscriptionRegistry) has(sess upstream.SessionRef, connectorID, uri string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[sess]
	if s == nil {
		return false
	}
	_, ok := s.byConnector[connectorID][uri]
	return ok
}

func (r *subscriptionRegistry) count(sess upstream.SessionRef) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[sess]; s != nil {
		return s.count
	}
	return 0
}

// add records a subscription, reporting false when it was already held.
func (r *subscriptionRegistry) add(sess upstream.SessionRef, connectorID, slug, uri string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[sess]
	if s == nil {
		s = &sessionSubscriptions{byConnector: make(map[string]map[string]struct{}), slugs: make(map[string]string)}
		r.sessions[sess] = s
	}
	uris := s.byConnector[connectorID]
	if uris == nil {
		uris = make(map[string]struct{})
		s.byConnector[connectorID] = uris
	}
	if _, dup := uris[uri]; dup {
		return false
	}
	uris[uri] = struct{}{}
	s.slugs[slug] = connectorID
	s.count++
	return true
}

// remove drops the subscription to uri on the connector named slug,
// returning that connector's id when there was one.
func (r *subscriptionRegistry) remove(sess upstream.SessionRef, slug, uri string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[sess]
	if s == nil {
		return "", false
	}
	connectorID, ok := s.slugs[slug]
	if !ok {
		return "", false
	}
	uris := s.byConnector[connectorID]
	if _, ok := uris[uri]; !ok {
		return "", false
	}
	delete(uris, uri)
	s.count--
	if len(uris) == 0 {
		delete(s.byConnector, connectorID)
		delete(s.slugs, slug)
	}
	if s.count == 0 {
		delete(r.sessions, sess)
	}
	return connectorID, true
}

func (r *subscriptionRegistry) drop(sess upstream.SessionRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, sess)
}

// ---------------------------------------------------------------------------
// resources/subscribe, resources/unsubscribe
// ---------------------------------------------------------------------------

func (o *Orchestrator) handleResourcesSubscribe(ctx context.Context, in Request) Result {
	params, slug, uri, r, failed := o.subscriptionParams(in)
	if failed {
		return r
	}
	conn, probe, r, failed := o.routeCatalog(ctx, in, slug, "resource", params.URI)
	if failed {
		return r
	}
	sess := upstream.SessionRef{TenantID: in.Principal.TenantID, SessionID: in.Session.ID}

	if o.subs.has(sess, conn.ID, uri) {
		return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, struct{}{}), ConnectorID: conn.ID}
	}
	if n := o.subs.count(sess); n >= o.maxSubscriptions {
		return o.fail(in, mcp.NewError(mcp.ErrorCodeInvalidRequest,
			"too many resource subscriptions on this session: unsubscribe from one first",
			map[string]any{"limit": o.maxSubscriptions}))
	}

	backend, err := o.ensureBackend(ctx, in, conn)
	if err != nil {
		o.log.Warn("failed to prepare backend session", "connector_id", conn.ID, "error", err)
		return o.fail(in, mcp.NewError(mcp.ErrorCodeToolExecution, "connector handshake failed: "+err.Error(), nil))
	}

	// The stream is opened before the subscribe is forwarded, so an
	// update the connector sends the moment it has subscribed already
	// has somewhere to go.
	need := subscriptionNeed(uri)
	if err := o.upstream.Acquire(ctx, o.streamTarget(in, conn, backend), need); err != nil {
		return o.fail(in, o.streamError(conn, err))
	}

	call := client.Call{
		Connector:       conn,
		SessionID:       backend.SessionID,
		ProtocolVersion: backend.ProtocolVersion,
		Inbound:         in.Inbound,
		Trace:           in.Trace,
	}
	result, err := o.deps.Client.SubscribeResource(ctx, call, uri)
	if err != nil {
		o.upstream.Release(sess, conn.ID, need)
		return o.catalogFailure(ctx, in, conn, probe, mcp.MethodResourcesSubscribe, params.URI, err)
	}
	o.rememberBackend(in, conn.ID, result)
	o.markRecovered(ctx, conn, probe)

	if !o.subs.add(sess, conn.ID, client.Qualifier(conn), uri) {
		// A concurrent subscribe to the same uri won; its need is the
		// one that stays.
		o.upstream.Release(sess, conn.ID, need)
	}
	return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, struct{}{}), ConnectorID: conn.ID}
}

// handleResourcesUnsubscribe drops a subscription. It is not gated on
// the profile -- giving something up is always allowed -- and it answers
// success for a uri the session was not subscribed to, which leaves the
// session exactly as the caller asked.
//
// The unsubscribe is forwarded to the connector best-effort, even when
// this replica holds no record of the subscription: with a shared
// session store the subscription may be another replica's, and the
// backend session it lives on is the one this session holds, so the
// connector can still be told to stop. Once the gateway has dropped the
// subscription a late update is dropped anyway.
func (o *Orchestrator) handleResourcesUnsubscribe(ctx context.Context, in Request) Result {
	params, slug, uri, r, failed := o.subscriptionParams(in)
	if failed {
		return r
	}
	sess := upstream.SessionRef{TenantID: in.Principal.TenantID, SessionID: in.Session.ID}

	var conn *store.Connector
	connectorID, held := o.subs.remove(sess, slug, uri)
	if held {
		defer o.upstream.Release(sess, connectorID, subscriptionNeed(uri))
		conn, _ = o.deps.Connectors.Get(ctx, in.Principal.TenantID, connectorID)
	} else {
		conn, _ = o.deps.Connectors.GetBySlug(ctx, in.Principal.TenantID, slug)
	}
	if conn == nil {
		return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, struct{}{})}
	}

	if backend := in.Backends.Backend(conn.ID); backend.ProtocolVersion != "" {
		result, err := o.deps.Client.UnsubscribeResource(ctx, client.Call{
			Connector:       conn,
			SessionID:       backend.SessionID,
			ProtocolVersion: backend.ProtocolVersion,
			Inbound:         in.Inbound,
			Trace:           in.Trace,
		}, uri)
		if err != nil {
			o.log.Debug("forwarding resources/unsubscribe failed", "connector_id", conn.ID, "uri", params.URI, "error", err)
		} else {
			o.rememberBackend(in, conn.ID, result)
		}
	}
	return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, struct{}{}), ConnectorID: conn.ID}
}

// subscriptionParams decodes and checks a subscribe/unsubscribe: it
// needs a session (a subscription is session state, and its updates go
// to that session's stream) and a gateway URI.
func (o *Orchestrator) subscriptionParams(in Request) (mcp.ResourcesSubscribeParams, string, string, Result, bool) {
	var params mcp.ResourcesSubscribeParams
	if err := decodeParams(in.JSONRPC.Params, &params); err != nil {
		return params, "", "", o.fail(in, mcp.NewInvalidParamsError(err.Error())), true
	}
	if params.URI == "" {
		return params, "", "", o.fail(in, mcp.NewInvalidParamsError(`"uri" is required`)), true
	}
	if in.Session == nil {
		return params, "", "", o.fail(in, mcp.NewInvalidRequestError(
			in.JSONRPC.Method+" needs a session: call initialize and send its "+mcp.HeaderSessionID+" header")), true
	}
	slug, uri, err := router.ParseResourceURI(params.URI)
	if err != nil {
		return params, "", "", o.fail(in, routingError(err)), true
	}
	return params, slug, uri, Result{}, false
}

// streamTarget describes the stream a session needs to conn.
func (o *Orchestrator) streamTarget(in Request, conn *store.Connector, backend session.Backend) upstream.Target {
	var inbound *http.Request
	if in.Inbound != nil {
		// The stream outlives this request; keep its headers, not it.
		inbound = in.Inbound.Clone(context.Background())
		inbound.Body = http.NoBody
	}
	return upstream.Target{
		Session:   upstream.SessionRef{TenantID: in.Principal.TenantID, SessionID: in.Session.ID},
		Connector: conn,
		Backend:   backend,
		Principal: in.Principal,
		Inbound:   inbound,
		Trace:     in.Trace,
	}
}

// streamError maps an upstream.Manager refusal onto the caller's error.
func (o *Orchestrator) streamError(conn *store.Connector, err error) *mcp.Error {
	switch {
	case errors.Is(err, upstream.ErrStreamUnsupported):
		return mcp.NewError(mcp.ErrorCodeMethodNotFound,
			"connector \""+client.Qualifier(conn)+"\" cannot push resource updates: it offers no server-to-client stream",
			map[string]any{"connector": client.Qualifier(conn)})
	case errors.Is(err, upstream.ErrTooManyStreams):
		return mcp.NewError(mcp.ErrorCodeInvalidRequest,
			"too many connector streams on this session: unsubscribe from another connector's resources first",
			map[string]any{"limit": o.maxStreams})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return mcp.NewError(mcp.ErrorCodeToolExecution, "request cancelled", nil)
	default:
		o.log.Error("failed to open connector stream", "connector_id", conn.ID, "error", err)
		return mcp.NewInternalError("failed to open connector stream")
	}
}

// EndSession drops everything the gateway holds for a session on this
// replica beyond its stored record: its subscriptions, its connector
// streams, and the server-initiated requests awaiting its answer, which
// fail. The transport calls it on DELETE /mcp; the upstream sweep does
// the same for a session that expired or was deleted elsewhere.
func (o *Orchestrator) EndSession(tenantID, sessionID string) {
	sess := upstream.SessionRef{TenantID: tenantID, SessionID: sessionID}
	o.subs.drop(sess)
	o.pending.endSession(sess)
	o.upstream.CloseSession(sess)
}

// lookupSession is the upstream.SessionLookup: the session's current
// backend handle for a connector, without touching its idle window.
func (o *Orchestrator) lookupSession(ctx context.Context, sess upstream.SessionRef, connectorID string) (session.Backend, bool, error) {
	s, err := o.deps.Sessions.Peek(ctx, sess.SessionID, sess.TenantID)
	if errors.Is(err, session.ErrNotFound) {
		return session.Backend{}, false, nil
	}
	if err != nil {
		return session.Backend{}, false, err
	}
	if connectorID == "" {
		return session.Backend{}, true, nil
	}
	return s.Backend(connectorID), true, nil
}

// ---------------------------------------------------------------------------
// relay
// ---------------------------------------------------------------------------

// HandleNotification implements upstream.NotificationHandler: it decides
// what a notification a connector pushed means for the session whose
// stream carried it.
func (o *Orchestrator) HandleNotification(ctx context.Context, sess upstream.SessionRef, conn upstream.ConnectorRef, note mcp.Request) {
	switch note.Method {
	case mcp.NotificationResourcesUpdated:
		o.relayResourceUpdated(sess, conn, note)

	case mcp.NotificationResourcesListChanged, mcp.NotificationPromptsListChanged:
		o.deliver(sess, mcp.Request{JSONRPC: mcp.Version, Method: note.Method, Params: note.Params})

	case mcp.NotificationToolsListChanged:
		o.refreshConnectorTools(ctx, sess, conn)

	default:
		o.log.Debug("dropping a connector notification the gateway does not relay",
			"connector_id", conn.ID, "method", note.Method)
	}
}

// relayResourceUpdated relays an update for a subscribed uri, with the
// uri put back into the gateway's namespace and every other parameter
// kept as the connector sent it.
func (o *Orchestrator) relayResourceUpdated(sess upstream.SessionRef, conn upstream.ConnectorRef, note mcp.Request) {
	var params map[string]json.RawMessage
	if err := json.Unmarshal(note.Params, &params); err != nil {
		o.log.Debug("dropping a malformed resources/updated", "connector_id", conn.ID, "error", err)
		return
	}
	var uri string
	if err := json.Unmarshal(params["uri"], &uri); err != nil || uri == "" {
		o.log.Debug("dropping a resources/updated with no uri", "connector_id", conn.ID)
		return
	}
	if !o.subs.has(sess, conn.ID, uri) {
		o.log.Debug("dropping an update for a resource the session is not subscribed to",
			"connector_id", conn.ID, "session_id", sess.SessionID)
		return
	}
	qualified, err := json.Marshal(client.QualifyResourceURI(conn.Slug, uri))
	if err != nil {
		return
	}
	params["uri"] = qualified
	raw, err := json.Marshal(params)
	if err != nil {
		return
	}
	o.deliver(sess, mcp.Request{JSONRPC: mcp.Version, Method: mcp.NotificationResourcesUpdated, Params: raw})
}

// deliver hands a notification to the session's stream: on this replica
// if it has one here, through the cross-replica relay otherwise, when
// the notifier has one (transport.Bridge).
func (o *Orchestrator) deliver(sess upstream.SessionRef, msg mcp.Request) {
	notifier, ok := o.deps.Notifier.(SessionNotifier)
	if !ok {
		return
	}
	if !notifier.Notify(sess.TenantID, sess.SessionID, msg) {
		o.log.Debug("dropping a connector notification: the session has no stream open",
			"session_id", sess.SessionID, "method", msg.Method)
	}
}

// refreshConnectorTools answers a connector's tools/list_changed: its
// cached tools are marked stale and re-listed, then every stream of the
// tenant is told, exactly as a cache refresh tells them. It runs off the
// stream's goroutine, and a refresh already running for the connector
// absorbs another announcement.
func (o *Orchestrator) refreshConnectorTools(ctx context.Context, sess upstream.SessionRef, ref upstream.ConnectorRef) {
	if o.deps.Cache == nil {
		// No cache: every tools/list is already live.
		if o.deps.Notifier != nil {
			o.deps.Notifier.ToolsListChanged(sess.TenantID)
		}
		return
	}
	if _, running := o.refreshing.LoadOrStore(ref.ID, struct{}{}); running {
		return
	}

	principal, _ := pkgauth.PrincipalFrom(ctx)
	go func() {
		defer o.refreshing.Delete(ref.ID)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), toolsRefreshTimeout)
		defer cancel()

		if err := o.deps.Cache.InvalidateConnector(ctx, ref.ID); err != nil {
			o.log.Warn("failed to invalidate a connector's cached tools", "connector_id", ref.ID, "error", err)
		}
		if principal != nil {
			o.relistConnectorTools(ctx, principal, sess.TenantID, ref.ID)
		}
		if o.deps.Notifier != nil {
			o.deps.Notifier.ToolsListChanged(sess.TenantID)
		}
	}()
}

// relistConnectorTools re-reads one connector's tools, as the agent whose
// stream announced the change, and writes them into the cache. A
// connector that now lists nothing keeps its stale rows until the next
// full refresh replaces them.
func (o *Orchestrator) relistConnectorTools(ctx context.Context, principal *pkgauth.Principal, tenantID, connectorID string) {
	conn, err := o.deps.Connectors.Get(ctx, tenantID, connectorID)
	if err != nil {
		o.log.Warn("failed to load connector for a tools refresh", "connector_id", connectorID, "error", err)
		return
	}
	in := Request{Principal: principal, Backends: o.deps.Sessions.Anonymous()}
	backend, err := o.ensureBackend(ctx, in, conn)
	if err != nil {
		o.log.Warn("tools refresh after list_changed: handshake failed", "connector_id", connectorID, "error", err)
		return
	}
	tools, result, err := o.deps.Client.ListTools(ctx, client.Call{
		Connector:       conn,
		SessionID:       backend.SessionID,
		ProtocolVersion: backend.ProtocolVersion,
	})
	if err != nil {
		o.log.Warn("tools refresh after list_changed failed", "connector_id", connectorID, "error", err)
		return
	}
	o.rememberBackend(in, conn.ID, result)
	if len(tools) == 0 {
		return
	}
	entries := make([]cache.Entry, 0, len(tools))
	for _, tool := range tools {
		entries = append(entries, cache.Entry{
			ConnectorID:   conn.ID,
			ConnectorName: client.Qualifier(conn),
			ToolName:      unqualify(client.Qualifier(conn), tool.Name),
			Tool:          tool,
		})
	}
	if err := o.deps.Cache.Put(ctx, tenantID, entries); err != nil {
		o.log.Warn("failed to write refreshed tools", "connector_id", connectorID, "error", err)
	}
}
