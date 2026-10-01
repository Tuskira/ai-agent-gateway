// Package orchestrator implements the MCP methods the gateway serves:
// initialize, ping, tools/list, tools/call, prompts/list, prompts/get,
// resources/list, resources/read, resources/templates/list,
// resources/subscribe and resources/unsubscribe, skills/list and
// skills/get (the MCP Skills Extension, SEP-2640; see skills.go), plus
// the notifications/cancelled that stops a tools/call in flight -- and it
// decides what the notifications connectors push on their streams mean
// for the agent sessions they belong to (subscriptions.go), and relays
// the requests connectors send their client (sampling, elicitation,
// roots) to the agent that can answer them (relay.go).
//
// It is the one place the pipeline's stages meet -- session, profile,
// route, headers, backend call, failure handling -- and it owns the
// decisions that span them: what a missing profile grants (nothing),
// which failures are worth telling the model about (timeouts) versus
// worth reporting as protocol errors (everything else), and when a
// connector's health flips.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/cache"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/reqctx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/router"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/skillsext"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/upstream"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/supervisor"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/trace"
)

const (
	// initializeDeadline bounds ONE connector's handshake during the
	// initialize fan-out, independently of that connector's timeout_ms.
	//
	// A connector configured with a five-minute tool timeout is making a
	// statement about slow queries, not about how long a handshake may
	// take. Without a separate, short bound, one unreachable backend
	// would hold the caller's initialize open for that entire window.
	initializeDeadline = 5 * time.Second

	// serverName and serverVersion identify the gateway to its clients.
	serverName    = "tusk-ai-secured-gateway"
	serverVersion = "0.1.0"

	instructions = "Tool gateway. Tools and prompts are named \"<connector>__<name>\"; call them by that name. " +
		"Resource URIs are \"gw://<connector>/<uri>\"; read and subscribe to them by that URI. " +
		"The set you see is bounded by your agent profile."
)

// Notifier is told when a tenant's tool list changes, so connected SSE
// clients can be pushed a notifications/tools/list_changed.
type Notifier interface {
	ToolsListChanged(tenantID string)
}

// Deps are the orchestrator's collaborators.
type Deps struct {
	Sessions   *session.Manager
	Profiles   *profile.Enforcer
	Router     *router.Router
	Client     *client.Client
	Cache      cache.ToolCache
	Connectors store.ConnectorStore
	Logger     *slog.Logger
	Notifier   Notifier
	// Skills resolves the MCP Skills Extension (SEP-2640): the skills
	// attached to the caller's agent profile, served by skills/list,
	// skills/get and resources/read ("skill://" URIs), independently of
	// Profiles' tool allow-list. Nil disables the extension entirely --
	// initialize never advertises it, and skills/list/skills/get answer
	// method-not-found -- which some tests rely on.
	Skills *skillsext.Resolver

	// RequireProfile rejects a request that names no profile.
	RequireProfile bool

	// MaxUpstreamStreamsPerSession bounds the connector streams one
	// session may hold (zero: upstream.DefaultMaxPerSession), and
	// MaxSubscriptionsPerSession its resource subscriptions (zero:
	// DefaultMaxSubscriptionsPerSession).
	MaxUpstreamStreamsPerSession int
	MaxSubscriptionsPerSession   int
	// MaxPendingServerRequestsPerSession bounds the server-initiated
	// requests relayed to one session that may await its answer at once
	// (zero: DefaultMaxPendingServerRequestsPerSession), and
	// ServerRequestTimeout how long each may await it (zero:
	// DefaultServerRequestTimeout).
	MaxPendingServerRequestsPerSession int
	ServerRequestTimeout               time.Duration
	// Sink receives one access-log record per relayed server-initiated
	// request. Nil writes none.
	Sink sink.LogSink
	// Upstream tunes the connector-stream manager beyond the fields
	// above (backoff, sweep interval); tests shorten them. Its Client,
	// Notifications, Requests, Lookup, OnSessionGone and MaxPerSession
	// are set by New.
	Upstream upstream.Options
}

// Orchestrator serves MCP methods.
type Orchestrator struct {
	deps     Deps
	log      *slog.Logger
	inflight *inflightRegistry

	upstream         *upstream.Manager
	subs             *subscriptionRegistry
	maxSubscriptions int
	maxStreams       int

	// pending holds the server-initiated requests relayed to agents and
	// awaiting their answer (relay.go).
	pending        *pendingTable
	maxPending     int
	requestTimeout time.Duration

	// refreshing holds the connector ids whose tools are being re-listed
	// after a tools/list_changed, so a burst of them costs one re-list.
	refreshing sync.Map
}

// New returns an Orchestrator.
func New(deps Deps) (*Orchestrator, error) {
	switch {
	case deps.Sessions == nil:
		return nil, fmt.Errorf("orchestrator: Sessions is required")
	case deps.Router == nil:
		return nil, fmt.Errorf("orchestrator: Router is required")
	case deps.Client == nil:
		return nil, fmt.Errorf("orchestrator: Client is required")
	case deps.Connectors == nil:
		return nil, fmt.Errorf("orchestrator: Connectors is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.MaxUpstreamStreamsPerSession <= 0 {
		deps.MaxUpstreamStreamsPerSession = upstream.DefaultMaxPerSession
	}
	if deps.MaxSubscriptionsPerSession <= 0 {
		deps.MaxSubscriptionsPerSession = DefaultMaxSubscriptionsPerSession
	}
	if deps.MaxPendingServerRequestsPerSession <= 0 {
		deps.MaxPendingServerRequestsPerSession = DefaultMaxPendingServerRequestsPerSession
	}
	if deps.ServerRequestTimeout <= 0 {
		deps.ServerRequestTimeout = DefaultServerRequestTimeout
	}

	o := &Orchestrator{
		deps:             deps,
		log:              deps.Logger,
		inflight:         newInflightRegistry(),
		subs:             newSubscriptionRegistry(),
		maxSubscriptions: deps.MaxSubscriptionsPerSession,
		maxStreams:       deps.MaxUpstreamStreamsPerSession,
		pending:          newPendingTable(),
		maxPending:       deps.MaxPendingServerRequestsPerSession,
		requestTimeout:   deps.ServerRequestTimeout,
	}

	opts := deps.Upstream
	opts.Client = deps.Client
	opts.Notifications = o
	opts.Requests = o
	opts.Lookup = o.lookupSession
	opts.OnSessionGone = func(sess upstream.SessionRef) {
		o.subs.drop(sess)
		o.pending.endSession(sess)
	}
	opts.MaxPerSession = deps.MaxUpstreamStreamsPerSession
	if opts.Logger == nil {
		opts.Logger = deps.Logger
	}
	manager, err := upstream.New(opts)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: %w", err)
	}
	o.upstream = manager
	return o, nil
}

// UpstreamRoutine returns the connector-stream sweep as a
// supervisor.Routine: it closes the streams of sessions that expired or
// were deleted on another replica, and closes every stream when stopped.
func (o *Orchestrator) UpstreamRoutine() supervisor.Routine { return o.upstream.Routine() }

// UpstreamStreams reports how many connector streams this replica holds
// and how many goroutines serve them, for diagnostics and leak checks.
func (o *Orchestrator) UpstreamStreams() (streams, goroutines int) {
	return o.upstream.Streams(), o.upstream.Running()
}

// Close closes every connector stream and waits for them to stop.
func (o *Orchestrator) Close() { o.upstream.Close() }

// Request is one inbound MCP call, already authenticated and
// session-resolved by the transport.
type Request struct {
	// JSONRPC is the decoded request.
	JSONRPC *mcp.Request
	// Principal is the authenticated caller. Never nil.
	Principal *pkgauth.Principal
	// Session is the caller's session, or nil for a stateless request.
	Session *session.Session
	// Backends is where backend handles are read and written: the
	// session when there is one, the process-wide anonymous store
	// otherwise.
	Backends session.Backends
	// ProfileName is the X-Agent-Profile-Name header value, if any.
	ProfileName string
	// Inbound is the caller's HTTP request, for the incoming_field
	// header resolver. May be nil.
	Inbound *http.Request
	// Trace is the request's trace context, propagated to backends.
	Trace trace.Context
}

// Result is the orchestrator's answer.
type Result struct {
	// Response is the JSON-RPC response, or nil for a notification.
	Response *mcp.Response
	// NewSession is set by initialize: the session whose id the
	// transport must return in the Mcp-Session-Id header.
	NewSession *session.Session
	// ConnectorID names the connector that served a tools/call, for
	// the X-Connector-ID response header.
	ConnectorID string
}

// Handle dispatches one request.
func (o *Orchestrator) Handle(ctx context.Context, in Request) Result {
	req := in.JSONRPC
	info := reqctx.From(ctx)
	info.SetMethod(req.Method)
	info.SetJSONRPCID(mcp.FormatID(req.ID))

	// A key bound to a profile has that profile enforced: the header is
	// ignored when absent and rejected when it names another profile.
	in, bindErr := o.bindProfile(ctx, in)
	if bindErr != nil {
		return o.fail(in, bindErr)
	}

	switch req.Method {
	case mcp.MethodInitialize:
		return o.handleInitialize(ctx, in)
	case mcp.MethodInitialized:
		// A notification: acknowledged, no response.
		return Result{}
	case mcp.MethodPing:
		return Result{Response: mcp.NewSuccessResponse(req.ID, mcp.PingResult{})}
	case mcp.NotificationCancelled:
		return o.handleCancelled(ctx, in)
	case mcp.MethodToolsList:
		defer o.persistBackends(ctx, in)
		return o.handleToolsList(ctx, in)
	case mcp.MethodToolsCall:
		defer o.persistBackends(ctx, in)
		return o.handleToolsCall(ctx, in)
	case mcp.MethodPromptsList:
		defer o.persistBackends(ctx, in)
		return o.handlePromptsList(ctx, in)
	case mcp.MethodPromptsGet:
		defer o.persistBackends(ctx, in)
		return o.handlePromptsGet(ctx, in)
	case mcp.MethodSkillsList:
		// Never touches a connector or a backend handle: no
		// persistBackends, unlike every method above and below it.
		return o.handleSkillsList(ctx, in)
	case mcp.MethodSkillsGet:
		return o.handleSkillsGet(ctx, in)
	case mcp.MethodResourcesList:
		defer o.persistBackends(ctx, in)
		return o.handleResourcesList(ctx, in)
	case mcp.MethodResourcesTemplatesList:
		defer o.persistBackends(ctx, in)
		return o.handleResourceTemplatesList(ctx, in)
	case mcp.MethodResourcesRead:
		defer o.persistBackends(ctx, in)
		return o.handleResourcesRead(ctx, in)
	case mcp.MethodResourcesSubscribe:
		defer o.persistBackends(ctx, in)
		return o.handleResourcesSubscribe(ctx, in)
	case mcp.MethodResourcesUnsubscribe:
		defer o.persistBackends(ctx, in)
		return o.handleResourcesUnsubscribe(ctx, in)
	default:
		return o.fail(in, mcp.NewMethodNotFoundError(req.Method))
	}
}

// persistBackends saves the caller's session when serving the request
// changed a backend handle -- a lazy handshake (ensureBackend), a
// rotated backend session id (rememberBackend) or a dropped one
// (forgetBackend). With the in-process store that is a no-op in effect;
// with a shared one it is what lets the next request, on any replica,
// reuse the handle instead of re-handshaking.
func (o *Orchestrator) persistBackends(ctx context.Context, in Request) {
	if in.Session == nil {
		return
	}
	if err := o.deps.Sessions.SaveIfChanged(ctx, in.Session); err != nil {
		o.log.Warn("failed to persist session backend handles", "session_id", in.Session.ID, "error", err)
	}
}

// fail renders a JSON-RPC error response and records its code for the
// access log.
func (o *Orchestrator) fail(in Request, err *mcp.Error) Result {
	if in.JSONRPC != nil && in.JSONRPC.IsNotification() {
		return Result{}
	}
	return Result{Response: mcp.NewErrorResponse(idOf(in), err)}
}

func idOf(in Request) any {
	if in.JSONRPC == nil {
		return nil
	}
	return in.JSONRPC.ID
}

// ---------------------------------------------------------------------------
// initialize
// ---------------------------------------------------------------------------

// handleInitialize mints a session and hands the client the gateway's
// capabilities, having first handshaken with every connector in the
// tenant.
//
// The fan-out is concurrent and best-effort: a backend that is down
// marks itself unhealthy and is left out of the answer, rather than
// failing the caller's initialize. The gateway is useful with three of
// four backends up, and a client that cannot initialize has no way to
// discover that.
func (o *Orchestrator) handleInitialize(ctx context.Context, in Request) Result {
	var params mcp.InitializeParams
	if err := decodeParams(in.JSONRPC.Params, &params); err != nil {
		return o.fail(in, mcp.NewInvalidParamsError(err.Error()))
	}

	protocolVersion := negotiateClientVersion(params.ProtocolVersion)
	// What the agent declared of sampling, elicitation and roots decides
	// which server-initiated requests the gateway may relay to it, and so
	// what it declares to each connector on its behalf (relay.go).
	caps := declaredCapabilities(in.JSONRPC.Params)
	sess, err := o.deps.Sessions.Create(ctx, in.Principal.TenantID, in.Principal.Subject, protocolVersion, params.ClientInfo, caps)
	if err != nil {
		o.log.Error("failed to create session", "error", err)
		return o.fail(in, mcp.NewInternalError("failed to create session"))
	}
	reqctx.From(ctx).SetSession(sess.ID)

	connectors, err := o.deps.Connectors.List(ctx, in.Principal.TenantID)
	if err != nil {
		o.log.Error("failed to list connectors for initialize", "error", err)
		return o.fail(in, mcp.NewInternalError("failed to list connectors"))
	}

	serverCaps := o.fanOutInitialize(ctx, in, sess, connectors)
	o.advertiseSkills(ctx, in, &serverCaps)

	// Resolved best-effort, purely to shape the instructions text and the
	// prompts capability below -- NOT enforcement (that still runs, as
	// always, on tools/list, tools/call, prompts/* and resources/*). A
	// missing profile name or a resolution failure is treated the same as
	// "no profile": initialize itself never fails on it, matching
	// mcp.require_profile's existing behaviour of not gating initialize.
	allow := o.resolveProfileForInitialize(ctx, in)
	if allow != nil && allow.Found && len(allow.Commands) > 0 {
		if serverCaps.Prompts == nil {
			serverCaps.Prompts = &mcp.PromptsCapability{}
		}
	}

	if err := o.deps.Sessions.Save(ctx, sess); err != nil {
		o.log.Warn("failed to persist session after initialize", "session_id", sess.ID, "error", err)
	}

	result := mcp.InitializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities:    serverCaps,
		ServerInfo:      mcp.Implementation{Name: serverName, Version: serverVersion},
		Instructions:    buildInstructions(allow),
	}

	return Result{
		Response:   mcp.NewSuccessResponse(in.JSONRPC.ID, result),
		NewSession: sess,
	}
}

// negotiateClientVersion answers a client's requested protocol version
// with one we both speak. An unrecognized (or absent) request gets the
// gateway's current version, which is what the spec asks for: the server
// states what it supports and the client decides whether to proceed.
func negotiateClientVersion(requested string) string {
	if requested == mcp.ProtocolVersionLegacy {
		return mcp.ProtocolVersionLegacy
	}
	return mcp.ProtocolVersion
}

// fanOutInitialize handshakes with every connector concurrently and
// returns the gateway's aggregated capabilities.
func (o *Orchestrator) fanOutInitialize(ctx context.Context, in Request, sess *session.Session, connectors []*store.Connector) mcp.ServerCapabilities {
	var (
		mu        sync.Mutex
		resources *mcp.ResourcesCapability
		prompts   *mcp.PromptsCapability
		wg        sync.WaitGroup
	)

	for _, conn := range connectors {
		wg.Add(1)
		go func(conn *store.Connector) {
			defer wg.Done()

			connCtx, cancel := context.WithTimeout(ctx, initializeDeadline)
			defer cancel()

			res, ok := o.handshake(connCtx, in, sess, conn)
			if !ok {
				return
			}

			mu.Lock()
			if r := res.Capabilities.Resources; r != nil {
				if resources == nil {
					resources = &mcp.ResourcesCapability{}
				}
				resources.Subscribe = resources.Subscribe || r.Subscribe
				resources.ListChanged = resources.ListChanged || r.ListChanged
			}
			if p := res.Capabilities.Prompts; p != nil {
				if prompts == nil {
					prompts = &mcp.PromptsCapability{}
				}
				prompts.ListChanged = prompts.ListChanged || p.ListChanged
			}
			mu.Unlock()
		}(conn)
	}
	wg.Wait()

	// tools.listChanged is advertised unconditionally. The gateway is a
	// tool router; a client that sees an empty capability object skips
	// tools/list entirely, which would hide every tool behind whatever
	// cold-start race the fan-out just lost.
	//
	// prompts and resources are advertised only when some reachable
	// backend has them, and each of their flags only when some reachable
	// backend reports it: resources.subscribe (the gateway serves
	// resources/subscribe and relays the updates), resources.listChanged
	// and prompts.listChanged (it relays those notifications). A flag no
	// backend reports would promise something no backend will ever send.
	return mcp.ServerCapabilities{
		Tools:     &mcp.ToolsCapability{ListChanged: true},
		Resources: resources,
		Prompts:   prompts,
	}
}

// handshake runs initialize + notifications/initialized against one
// connector, recording its health, capabilities and session handle.
//
// The client capabilities it declares are the ones the agent session in
// backends declared AND the connector's policy allows (relay.go); when
// there are any, the connector's stream is opened for the session too,
// since a request may arrive on it rather than inside a call's reply.
func (o *Orchestrator) handshake(ctx context.Context, in Request, backends session.Backends, conn *store.Connector) (*mcp.InitializeResult, bool) {
	advertised := advertisedCapabilities(backends, conn)
	call := client.Call{
		Connector:          conn,
		Inbound:            in.Inbound,
		Trace:              in.Trace,
		ClientCapabilities: advertised,
	}

	res, callResult, err := o.deps.Client.Initialize(ctx, call)
	if err != nil {
		o.log.Warn("connector initialize failed; marking unhealthy",
			"connector_id", conn.ID, "connector", conn.Name, "error", err)
		if err := o.deps.Router.MarkUnhealthy(ctx, conn); err != nil {
			o.log.Warn("failed to persist connector health", "connector_id", conn.ID, "error", err)
		}
		return nil, false
	}

	backends.SetBackend(conn.ID, session.Backend{
		SessionID:       callResult.SessionID,
		ProtocolVersion: callResult.ProtocolVersion,
	})

	// The post-handshake notification carries the session header the
	// backend just issued; without it a session-tracking backend rejects
	// every subsequent request.
	if err := o.deps.Client.SendInitialized(ctx, client.Call{
		Connector:       conn,
		SessionID:       callResult.SessionID,
		ProtocolVersion: callResult.ProtocolVersion,
		Inbound:         in.Inbound,
		Trace:           in.Trace,
	}); err != nil {
		// Not fatal: some servers answer the notification with an
		// error yet serve tools perfectly well afterwards.
		o.log.Warn("initialized notification failed", "connector_id", conn.ID, "error", err)
	}

	conn.Capabilities = map[string]any{
		"tools":     res.Capabilities.Tools != nil,
		"resources": res.Capabilities.Resources != nil,
		"prompts":   res.Capabilities.Prompts != nil,
	}
	conn.Status = router.StatusHealthy
	if err := o.deps.Connectors.Update(ctx, conn); err != nil {
		o.log.Warn("failed to persist connector capabilities", "connector_id", conn.ID, "error", err)
	}

	o.log.Info("connector initialized",
		"connector_id", conn.ID, "connector", conn.Name,
		"server", res.ServerInfo.Name, "protocol_version", callResult.ProtocolVersion)

	if len(advertised) > 0 {
		o.openServerRequestStream(ctx, in, backends.(*session.Session), conn, session.Backend{
			SessionID:       callResult.SessionID,
			ProtocolVersion: callResult.ProtocolVersion,
		})
	}

	return res, true
}

// ---------------------------------------------------------------------------
// tools/list
// ---------------------------------------------------------------------------

func (o *Orchestrator) handleToolsList(ctx context.Context, in Request) Result {
	allow, mcpErr := o.resolveProfile(ctx, in)
	if mcpErr != nil {
		return o.fail(in, mcpErr)
	}

	entries, err := o.tenantTools(ctx, in)
	if err != nil {
		o.log.Error("failed to list tenant tools", "tenant_id", in.Principal.TenantID, "error", err)
		return o.fail(in, mcp.NewInternalError("failed to list tools"))
	}

	// Always a slice, never nil: clients treat a missing list and an
	// empty one differently, and "this profile grants nothing" must
	// serialize as [].
	tools := make([]mcp.Tool, 0, len(entries)+1)
	for _, e := range entries {
		if allow != nil && !allow.Allows(e.ConnectorID, e.ToolName) {
			continue
		}
		tools = append(tools, e.Tool)
	}
	// The native gateway__skill tool is listed only when the resolved
	// profile actually has at least one attached skill -- an agent with
	// none has nothing for it to load, and listing it anyway would just
	// invite a call that always answers -32003.
	if allow != nil && allow.Found && len(allow.Skills) > 0 {
		tools = append(tools, nativeSkillTool())
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	if allow != nil {
		o.log.Debug("tools/list filtered by agent profile",
			"profile", allow.Name, "profile_found", allow.Found,
			"available", len(entries), "granted", len(tools))
	}

	return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, mcp.ToolsListResult{Tools: tools})}
}

// tenantTools returns every tool the caller's tenant advertises, from
// the cache when it can and from a live fan-out when it must.
func (o *Orchestrator) tenantTools(ctx context.Context, in Request) ([]cache.Entry, error) {
	tenantID := in.Principal.TenantID

	if o.deps.Cache != nil {
		snap, err := o.deps.Cache.Get(ctx, tenantID)
		if err != nil {
			// A cache read failure must not fail the request: fall
			// through to the backends, which are the source of truth.
			o.log.Warn("tool cache read failed; falling back to a live fan-out", "tenant_id", tenantID, "error", err)
		} else if snap.Hit {
			if snap.Stale {
				o.log.Debug("serving stale tool cache", "tenant_id", tenantID, "tools", len(snap.Entries))
			}
			return snap.Entries, nil
		}
	}

	entries, err := o.fanOutListTools(ctx, in, tenantID)
	if err != nil {
		return nil, err
	}

	if o.deps.Cache != nil {
		if err := o.deps.Cache.Put(ctx, tenantID, entries); err != nil {
			o.log.Warn("failed to write tool cache", "tenant_id", tenantID, "error", err)
		}
	}
	return entries, nil
}

// fanOutListTools queries every callable connector concurrently.
//
// A connector that fails is logged and skipped rather than failing the
// whole list: one broken backend must not blind an agent to the other
// four. It is not marked unhealthy here -- a tools/list failure is a
// weaker signal than a failed handshake, and marking on it would let a
// single slow list take a connector out of rotation.
func (o *Orchestrator) fanOutListTools(ctx context.Context, in Request, tenantID string) ([]cache.Entry, error) {
	connectors, err := o.deps.Router.Callable(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	var (
		mu      sync.Mutex
		entries []cache.Entry
		wg      sync.WaitGroup
	)

	for _, conn := range connectors {
		wg.Add(1)
		go func(conn *store.Connector) {
			defer wg.Done()

			backend, err := o.ensureBackend(ctx, in, conn)
			if err != nil {
				o.log.Warn("skipping connector in tools/list", "connector_id", conn.ID, "error", err)
				return
			}

			tools, result, err := o.deps.Client.ListTools(ctx, client.Call{
				Connector:       conn,
				SessionID:       backend.SessionID,
				ProtocolVersion: backend.ProtocolVersion,
				Inbound:         in.Inbound,
				Trace:           in.Trace,
			})
			if err != nil {
				o.log.Warn("tools/list failed for connector", "connector_id", conn.ID, "error", err)
				return
			}
			o.rememberBackend(in, conn.ID, result)

			mu.Lock()
			for _, tool := range tools {
				entries = append(entries, cache.Entry{
					ConnectorID:   conn.ID,
					ConnectorName: client.Qualifier(conn),
					ToolName:      unqualify(client.Qualifier(conn), tool.Name),
					Tool:          tool,
				})
			}
			mu.Unlock()
		}(conn)
	}
	wg.Wait()

	return entries, nil
}

// RefreshTenant re-reads a tenant's tools from its connectors and writes
// them into the cache. It is what the background refresh routine calls.
//
// The refresher has no caller to borrow an identity from, so it acts as
// a synthetic tenant-scoped principal. That is enough for X-Tenant-Id
// and for static/secret_store headers; a connector whose headers forward
// something from the caller (token_field, incoming_field) cannot be
// refreshed in the background and is logged and skipped by the client's
// per-header degradation. Those connectors stay populated by the live
// fan-out their first real tools/list triggers.
func (o *Orchestrator) RefreshTenant(ctx context.Context, tenantID string) error {
	if o.deps.Cache == nil {
		return nil
	}

	principal := &pkgauth.Principal{
		Subject:    "system:tool-cache-refresh",
		TenantID:   tenantID,
		Roles:      []string{"agent"},
		AuthMethod: "system",
	}
	ctx = pkgauth.WithPrincipal(ctx, principal)

	in := Request{
		Principal: principal,
		Backends:  o.deps.Sessions.Anonymous(),
		Trace:     trace.Context{TraceID: trace.GenerateTraceID(), SpanID: trace.GenerateSpanID(), Flags: trace.FlagsSampled},
	}

	entries, err := o.fanOutListTools(ctx, in, tenantID)
	if err != nil {
		return err
	}
	if err := o.deps.Cache.Put(ctx, tenantID, entries); err != nil {
		return err
	}

	if o.deps.Notifier != nil {
		o.deps.Notifier.ToolsListChanged(tenantID)
	}
	return nil
}

func unqualify(connectorName, qualified string) string {
	prefix := connectorName + "__"
	if len(qualified) > len(prefix) && qualified[:len(prefix)] == prefix {
		return qualified[len(prefix):]
	}
	return qualified
}

// ---------------------------------------------------------------------------
// tools/call
// ---------------------------------------------------------------------------

func (o *Orchestrator) handleToolsCall(ctx context.Context, in Request) Result {
	var params mcp.ToolsCallParams
	if err := decodeParams(in.JSONRPC.Params, &params); err != nil {
		return o.fail(in, mcp.NewInvalidParamsError(err.Error()))
	}
	if params.Name == "" {
		return o.fail(in, mcp.NewInvalidParamsError(`"name" is required`))
	}

	info := reqctx.From(ctx)
	info.SetTool(params.Name)

	allow, mcpErr := o.resolveProfile(ctx, in)
	if mcpErr != nil {
		return o.fail(in, mcpErr)
	}

	// gateway__skill is native: intercepted BEFORE routing (the "gateway"
	// connector name/slug is reserved by the control-plane API precisely
	// so this can never collide with a real connector's tool) and never
	// forwarded to a connector.
	if params.Name == nativeSkillToolName {
		return o.handleGatewaySkillTool(ctx, in, allow, params)
	}

	route, err := o.deps.Router.Resolve(ctx, in.Principal.TenantID, params.Name)
	if err != nil {
		return o.fail(in, routingError(err))
	}
	info.SetConnector(route.Connector.ID)

	// The profile gate sits AFTER routing because the grant is per
	// (connector, tool): "search" on one connector is a different grant
	// from "search" on another, and only routing says which one this is.
	if allow != nil && !allow.Allows(route.Connector.ID, route.ToolName) {
		o.log.Info("tools/call denied by agent profile",
			"profile", allow.Name, "profile_found", allow.Found,
			"tool", params.Name, "connector_id", route.Connector.ID)
		return o.fail(in, mcp.NewError(mcp.ErrorCodeToolNotAllowed, "tool not allowed by profile", map[string]any{
			"tool":    params.Name,
			"profile": allow.Name,
		}))
	}

	backend, err := o.ensureBackend(ctx, in, route.Connector)
	if err != nil {
		o.log.Warn("failed to prepare backend session", "connector_id", route.Connector.ID, "error", err)
		return o.fail(in, mcp.NewError(mcp.ErrorCodeToolExecution, "connector handshake failed: "+err.Error(), nil))
	}

	args := router.StampOverrides(route.Connector, route.ToolName, params.Arguments)

	call := client.Call{
		Connector:       route.Connector,
		SessionID:       backend.SessionID,
		ProtocolVersion: backend.ProtocolVersion,
		Inbound:         in.Inbound,
		Trace:           in.Trace,
	}
	// Cancellation and progress: see inflight.go.
	callCtx, done := o.beginCall(ctx, in, route.Connector, params.Meta, &call, route.ToolName)
	defer done()

	out, result, err := o.deps.Client.CallTool(callCtx, call, route.ToolName, args)

	if err != nil {
		if r, ok := o.clientCancelled(callCtx, in, route, params.Name); ok {
			return r
		}
		return o.toolCallFailure(ctx, in, route, params.Name, err)
	}

	o.rememberBackend(in, route.Connector.ID, result)
	o.markRecovered(ctx, route.Connector, route.Probe)

	return Result{
		Response:    mcp.NewSuccessResponse(in.JSONRPC.ID, out),
		ConnectorID: route.Connector.ID,
	}
}

// toolCallFailure turns a failed backend call into the right answer for
// the caller.
//
// A timeout is reported as tool CONTENT, not as a protocol error: the
// caller is a model, and the one thing that turns a timeout into a
// successful next turn is telling it, in text it will read, that the
// query was too broad and was not retried. A protocol error is
// swallowed by most agent runtimes and produces a silent retry of the
// identical query, which times out identically.
func (o *Orchestrator) toolCallFailure(ctx context.Context, in Request, route *router.Route, qualified string, err error) Result {
	// Drop the backend handle so the next call re-handshakes rather
	// than replaying a session the backend may have dropped.
	o.forgetBackend(in, route.Connector.ID)

	switch {
	case client.IsTimeout(err):
		o.log.Warn("tool call timed out", "tool", qualified, "connector_id", route.Connector.ID, "error", err)
		return Result{
			Response: mcp.NewSuccessResponse(in.JSONRPC.ID, mcp.ToolsCallResult{
				Content: []mcp.Content{mcp.TextContent(timeoutMessage(qualified))},
				IsError: true,
			}),
			ConnectorID: route.Connector.ID,
		}

	case errors.Is(err, context.Canceled):
		// The caller went away. Nothing to report to it; say so plainly
		// for the log and move on.
		o.log.Debug("tool call cancelled by the caller", "tool", qualified, "connector_id", route.Connector.ID)
		return Result{
			Response:    mcp.NewErrorResponse(in.JSONRPC.ID, mcp.NewError(mcp.ErrorCodeToolExecution, "request cancelled", nil)),
			ConnectorID: route.Connector.ID,
		}

	default:
		o.log.Error("tool call failed", "tool", qualified, "connector_id", route.Connector.ID, "error", err)
		if err := o.deps.Router.MarkUnhealthy(ctx, route.Connector); err != nil {
			o.log.Warn("failed to persist connector health", "connector_id", route.Connector.ID, "error", err)
		}
		return Result{
			Response: mcp.NewErrorResponse(in.JSONRPC.ID,
				mcp.NewError(mcp.ErrorCodeToolExecution, "tool call failed: "+err.Error(), nil)),
			ConnectorID: route.Connector.ID,
		}
	}
}

func timeoutMessage(tool string) string {
	return fmt.Sprintf(
		"TOOL_TIMEOUT: the call to %q exceeded its time limit and was stopped. "+
			"It was NOT retried, to avoid duplicating load on the backend. "+
			"Retry with a more targeted request:\n"+
			"  1. narrow the time range (an hour or a day, not a month)\n"+
			"  2. add specific filters (user, host, id, severity, address)\n"+
			"  3. ask for fewer results (a top-N, or a single entity)\n"+
			"  4. split one broad query into several narrow ones\n"+
			"Do not repeat the same request: it will time out again.", tool)
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// bindProfile applies an API key's profile binding to in. For an unbound
// principal it returns in unchanged. For a bound one it sets
// in.ProfileName to the bound profile, or returns -32003 when the request's
// X-Agent-Profile-Name header names a different profile or the binding is
// dangling (profile deleted): fail closed, never fall back to every tool.
func (o *Orchestrator) bindProfile(ctx context.Context, in Request) (Request, *mcp.Error) {
	if in.Principal == nil || in.Principal.ProfileID == "" {
		return in, nil
	}
	if o.deps.Profiles == nil {
		return in, mcp.NewInternalError("profile enforcement is not configured")
	}
	prof, err := o.deps.Profiles.Bound(ctx, in.Principal.TenantID, in.Principal.ProfileID)
	if err != nil {
		o.log.Error("failed to resolve the key's bound profile", "profile_id", in.Principal.ProfileID, "error", err)
		return in, mcp.NewInternalError("failed to resolve agent profile")
	}
	if prof == nil {
		o.log.Warn("API key is bound to a profile that no longer exists; denying",
			"profile_id", in.Principal.ProfileID, "tenant_id", in.Principal.TenantID, "key_id", in.Principal.KeyID)
		return in, mcp.NewError(mcp.ErrorCodeToolNotAllowed,
			"this API key is bound to an agent profile that no longer exists", nil)
	}
	if in.ProfileName != "" && !profile.Matches(in.Principal.TenantID, prof, in.ProfileName) {
		return in, mcp.NewError(mcp.ErrorCodeToolNotAllowed,
			"this API key is bound to agent profile "+prof.Name+"; the "+profile.Header+" header names a different profile", nil)
	}
	in.ProfileName = profile.NameForSlug(in.Principal.TenantID, prof.Slug)
	reqctx.From(ctx).SetProfile(prof.Name)
	return in, nil
}

// BoundProfileSlug returns the slug of the profile p's API key is bound to,
// or "" when p is unbound or the binding is dangling.
func (o *Orchestrator) BoundProfileSlug(ctx context.Context, p *pkgauth.Principal) string {
	if p == nil || p.ProfileID == "" || o.deps.Profiles == nil {
		return ""
	}
	prof, err := o.deps.Profiles.Bound(ctx, p.TenantID, p.ProfileID)
	if err != nil || prof == nil {
		return ""
	}
	return prof.Slug
}

// resolveProfile returns the caller's allow-list, or nil when no profile
// applies (and none is required), in which case every tool in the tenant
// is in scope.
func (o *Orchestrator) resolveProfile(ctx context.Context, in Request) (*profile.AllowList, *mcp.Error) {
	if in.ProfileName == "" {
		if o.deps.RequireProfile {
			return nil, mcp.NewError(mcp.ErrorCodeToolNotAllowed,
				"an agent profile is required: send the "+profile.Header+" header", nil)
		}
		o.log.Debug("no agent profile on request; serving every tool in the tenant",
			"tenant_id", in.Principal.TenantID, "principal", in.Principal.Subject)
		return nil, nil
	}
	if o.deps.Profiles == nil {
		return nil, mcp.NewInternalError("profile enforcement is not configured")
	}

	allow, err := o.deps.Profiles.Resolve(ctx, in.Principal.TenantID, in.ProfileName)
	if err != nil {
		// A store failure is NOT a denial and must not be reported as
		// an empty tool list, which would look to a caller exactly like
		// a correctly-applied empty profile.
		o.log.Error("failed to resolve agent profile", "profile", in.ProfileName, "error", err)
		return nil, mcp.NewInternalError("failed to resolve agent profile")
	}
	if !allow.Found {
		o.log.Warn("agent profile not found; granting nothing",
			"profile", in.ProfileName, "slug", allow.Slug, "tenant_id", in.Principal.TenantID)
	}
	return &allow, nil
}

// ensureBackend returns the backend handle for a connector, performing a
// lazy handshake when there is none yet.
//
// This is what lets a stateless client -- one that never calls
// initialize, or that lost its session -- still call a tool: the gateway
// does the backend's handshake on its behalf, once, and remembers it.
func (o *Orchestrator) ensureBackend(ctx context.Context, in Request, conn *store.Connector) (session.Backend, error) {
	backends := in.Backends
	if backends == nil {
		backends = o.deps.Sessions.Anonymous()
	}

	if b := backends.Backend(conn.ID); b.ProtocolVersion != "" {
		return b, nil
	}

	handshakeCtx, cancel := context.WithTimeout(ctx, initializeDeadline)
	defer cancel()

	if _, ok := o.handshake(handshakeCtx, in, backends, conn); !ok {
		return session.Backend{}, fmt.Errorf("connector %q handshake failed", conn.Name)
	}
	return backends.Backend(conn.ID), nil
}

// markRecovered flips a connector back to healthy after a call it
// answered, which is how a half-open probe closes the circuit.
func (o *Orchestrator) markRecovered(ctx context.Context, conn *store.Connector, probe bool) {
	if !probe && conn.Status == router.StatusHealthy {
		return
	}
	if err := o.deps.Router.MarkHealthy(ctx, conn); err != nil {
		o.log.Warn("failed to persist connector recovery", "connector_id", conn.ID, "error", err)
	} else if probe {
		o.log.Info("connector recovered on half-open probe",
			"connector_id", conn.ID, "connector", conn.Name)
	}
}

// rememberBackend records a session id the backend may have issued or
// rotated on this call.
func (o *Orchestrator) rememberBackend(in Request, connectorID string, result *client.Result) {
	if result == nil || in.Backends == nil {
		return
	}
	in.Backends.SetBackend(connectorID, session.Backend{
		SessionID:       result.SessionID,
		ProtocolVersion: result.ProtocolVersion,
	})
}

// forgetBackend drops a connector's handle so the next call re-handshakes.
func (o *Orchestrator) forgetBackend(in Request, connectorID string) {
	if in.Backends == nil {
		return
	}
	in.Backends.SetBackend(connectorID, session.Backend{})
}

// routingError maps a router failure onto its JSON-RPC code.
func routingError(err error) *mcp.Error {
	switch {
	case errors.Is(err, router.ErrBadToolName),
		errors.Is(err, router.ErrBadPromptName),
		errors.Is(err, router.ErrBadResourceURI):
		return mcp.NewInvalidParamsError(err.Error())
	case errors.Is(err, router.ErrConnectorNotFound):
		return mcp.NewError(mcp.ErrorCodeConnectorNotFound, err.Error(), nil)
	case errors.Is(err, router.ErrConnectorUnhealthy):
		return mcp.NewError(mcp.ErrorCodeConnectorUnhealthy, err.Error(), nil)
	default:
		return mcp.NewInternalError(err.Error())
	}
}
