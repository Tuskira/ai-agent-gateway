// Package upstream keeps the long-lived server-to-client streams the
// gateway holds open to its connectors on behalf of agent sessions.
//
// A Streamable HTTP MCP server sends two kinds of message that are not
// replies to anything the client asked: notifications (a subscribed
// resource changed, a list changed) and requests (sampling, elicitation).
// It sends them on a GET stream the client opens and holds. The gateway
// is that client, once per (agent session, connector), because what
// arrives on the stream belongs to the backend session it was opened on
// -- and that backend session belongs to one agent session.
//
// A Manager owns those streams:
//
//   - lazily opened: a stream exists only while something needs it, named
//     by a "need" string (resources/subscribe is the first; each need is
//     acquired and released by the feature that has it), and is closed
//     when the last need is released or its agent session ends;
//   - kept alive: a stream that drops is reopened with exponential
//     backoff (1s to 30s, jittered), resuming with Last-Event-ID when the
//     server numbers its events, until the agent session is gone;
//   - never a health signal: an idle stream is healthy, has no deadline,
//     and neither it nor a failure to (re)open it touches connector
//     health, which only real calls decide;
//   - bounded: at most MaxPerSession streams per agent session;
//   - honest about support: a connector answering the GET with 405 or 404
//     has no such stream for that session, is recorded as such, and is
//     not retried -- the feature that needed it gets ErrStreamUnsupported.
//
// Every message read is dispatched: a notification to the
// NotificationHandler, a request to the RequestHandler, whose answer is
// POSTed back to the connector as the JSON-RPC response to that request.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/supervisor"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/trace"
)

const (
	// DefaultMaxPerSession is MaxPerSession's default.
	DefaultMaxPerSession = 16

	defaultInitialBackoff = time.Second
	defaultMaxBackoff     = 30 * time.Second
	defaultSweepInterval  = 30 * time.Second

	// maxRequestsInFlight bounds how many server-to-client requests one
	// stream may have awaiting an answer. A connector that sends more at
	// once is refused on the excess rather than growing goroutines
	// without limit.
	maxRequestsInFlight = 8
)

var (
	// ErrStreamUnsupported is client.ErrStreamUnsupported: the
	// connector answered the GET with 405 or 404.
	ErrStreamUnsupported = client.ErrStreamUnsupported
	// ErrTooManyStreams means the agent session already holds
	// MaxPerSession streams.
	ErrTooManyStreams = errors.New("upstream: too many connector streams for this session")
)

// SessionRef names one agent session. The tenant is part of the name so
// a session id alone can never address another tenant's session.
type SessionRef struct {
	TenantID  string
	SessionID string
}

// ConnectorRef names the connector a stream is open to. Slug is the
// qualifier its tools, prompts and resource URIs are namespaced under
// (client.Qualifier).
type ConnectorRef struct {
	ID   string
	Slug string
}

// NotificationHandler receives every notification a connector sends on
// a stream. It runs on the goroutine reading the stream, so it must not
// block; ctx lives as long as the stream.
type NotificationHandler interface {
	HandleNotification(ctx context.Context, sess SessionRef, conn ConnectorRef, note mcp.Request)
}

// RequestHandler answers a request a connector sends on a stream
// (sampling/createMessage, elicitation/create, roots/list, ...). It runs
// on its own goroutine and may block until it has an answer or ctx, which
// ends with the stream, is done. It returns exactly one of a result and
// an error; the Manager POSTs it back as the response to req.
type RequestHandler interface {
	HandleRequest(ctx context.Context, sess SessionRef, conn ConnectorRef, req mcp.Request) (result json.RawMessage, rpcErr *mcp.Error)
}

// Unsupported is the default RequestHandler when Options.Requests is
// nil: it answers every request with -32601. The gateway itself sets the
// orchestrator's relay, which forwards the requests it may to the agent.
type Unsupported struct{}

// HandleRequest implements RequestHandler.
func (Unsupported) HandleRequest(_ context.Context, _ SessionRef, _ ConnectorRef, req mcp.Request) (json.RawMessage, *mcp.Error) {
	return nil, mcp.NewError(mcp.ErrorCodeMethodNotFound, "not supported by this gateway: "+req.Method, nil)
}

// SessionLookup reports whether an agent session still exists and the
// backend handle it currently holds for connectorID (the zero Backend
// when connectorID is empty or it holds none). It must not extend the
// session's idle window: a stream is not the agent using its session.
type SessionLookup func(ctx context.Context, sess SessionRef, connectorID string) (b session.Backend, alive bool, err error)

// Target is everything opening a stream needs: whose it is, to which
// connector, on which backend session, and the identity its headers
// resolve for.
type Target struct {
	Session   SessionRef
	Connector *store.Connector
	Backend   session.Backend
	// Principal is the agent's identity: X-Tenant-Id and every
	// connector header resolve for it, exactly as for its calls.
	Principal *pkgauth.Principal
	// Inbound is a copy of the request that first needed the stream,
	// for the incoming_field header resolver. The stream outlives that
	// request, so the caller passes a detached clone (or nil).
	Inbound *http.Request
	Trace   trace.Context
}

// Options configures a Manager.
type Options struct {
	Client *client.Client
	// Notifications receives every notification. Required.
	Notifications NotificationHandler
	// Requests answers every server-to-client request. Nil means
	// Unsupported.
	Requests RequestHandler
	// Lookup is consulted before every reconnect and by the sweep. Nil
	// means every session is assumed alive until CloseSession.
	Lookup SessionLookup
	// OnSessionGone is called when the Manager itself finds a session
	// gone (its sweep, or a reconnect) and closes its streams, so the
	// owner of per-session state that rode on them can drop it too.
	OnSessionGone func(SessionRef)
	// MaxPerSession bounds streams per agent session. Zero uses
	// DefaultMaxPerSession.
	MaxPerSession int
	// InitialBackoff and MaxBackoff bound the wait between reconnects.
	// Zero uses 1s and 30s.
	InitialBackoff, MaxBackoff time.Duration
	// SweepInterval is how often Routine checks that every session
	// holding a stream still exists. Zero uses 30s.
	SweepInterval time.Duration
	Logger        *slog.Logger
}

// Manager owns the upstream streams of every agent session on this
// replica.
type Manager struct {
	client   *client.Client
	notes    NotificationHandler
	requests RequestHandler
	lookup   SessionLookup
	onGone   func(SessionRef)
	max      int
	initial  time.Duration
	maxWait  time.Duration
	sweep    time.Duration
	log      *slog.Logger

	mu      sync.Mutex
	streams map[SessionRef]map[string]*stream // connector id -> stream
	// unsupported records the (session, connector) pairs whose
	// connector refused the GET; they are not retried for the session.
	unsupported map[SessionRef]map[string]bool

	// wg tracks every goroutine the Manager starts (stream loops and
	// request answers); running counts them for leak checks.
	wg      sync.WaitGroup
	running atomic.Int64
}

// New returns a Manager.
func New(opts Options) (*Manager, error) {
	switch {
	case opts.Client == nil:
		return nil, fmt.Errorf("upstream: Client is required")
	case opts.Notifications == nil:
		return nil, fmt.Errorf("upstream: Notifications is required")
	}
	if opts.Requests == nil {
		opts.Requests = Unsupported{}
	}
	if opts.MaxPerSession <= 0 {
		opts.MaxPerSession = DefaultMaxPerSession
	}
	if opts.InitialBackoff <= 0 {
		opts.InitialBackoff = defaultInitialBackoff
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = defaultMaxBackoff
	}
	if opts.MaxBackoff < opts.InitialBackoff {
		opts.MaxBackoff = opts.InitialBackoff
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = defaultSweepInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Manager{
		client:      opts.Client,
		notes:       opts.Notifications,
		requests:    opts.Requests,
		lookup:      opts.Lookup,
		onGone:      opts.OnSessionGone,
		max:         opts.MaxPerSession,
		initial:     opts.InitialBackoff,
		maxWait:     opts.MaxBackoff,
		sweep:       opts.SweepInterval,
		log:         opts.Logger,
		streams:     make(map[SessionRef]map[string]*stream),
		unsupported: make(map[SessionRef]map[string]bool),
	}, nil
}

// stream is one (session, connector) stream and the loop that keeps it.
type stream struct {
	sess SessionRef
	conn ConnectorRef

	ctx    context.Context
	cancel context.CancelFunc

	// ready is closed once the first connection attempt has an outcome;
	// firstErr is that outcome when it is final (ErrStreamUnsupported),
	// nil otherwise -- a transient failure is retried in the background.
	ready    chan struct{}
	firstErr error

	// Guarded by Manager.mu.
	needs map[string]struct{}

	// Owned by the loop goroutine.
	target      Target
	lastEventID string

	answering chan struct{} // semaphore for server-to-client requests
}

// Acquire records that sess needs a stream to t.Connector for need and
// opens one if there is none. The first connection attempt is waited
// for, so a connector that has no stream is reported here, as
// ErrStreamUnsupported, rather than discovered later. A transient
// failure is not an error: the stream keeps reconnecting in the
// background. On any error the need is not retained.
//
// Acquiring a need already held is a no-op.
func (m *Manager) Acquire(ctx context.Context, t Target, need string) error {
	if t.Connector == nil {
		return fmt.Errorf("upstream: target has no connector")
	}
	key := t.Session
	connID := t.Connector.ID

	m.mu.Lock()
	if m.unsupported[key][connID] {
		m.mu.Unlock()
		return fmt.Errorf("%w (connector %q)", ErrStreamUnsupported, t.Connector.Name)
	}
	s := m.streams[key][connID]
	if s == nil {
		if len(m.streams[key]) >= m.max {
			m.mu.Unlock()
			return fmt.Errorf("%w (limit %d)", ErrTooManyStreams, m.max)
		}
		s = m.newStream(t)
		if m.streams[key] == nil {
			m.streams[key] = make(map[string]*stream)
		}
		m.streams[key][connID] = s
		m.start(func() { m.run(s) })
	}
	s.needs[need] = struct{}{}
	m.mu.Unlock()

	select {
	case <-s.ready:
	case <-ctx.Done():
		m.Release(key, connID, need)
		return ctx.Err()
	}
	if s.firstErr != nil {
		// The loop has already removed the stream, needs and all.
		return fmt.Errorf("%w (connector %q)", s.firstErr, t.Connector.Name)
	}
	return nil
}

func (m *Manager) newStream(t Target) *stream {
	// The stream acts for the agent, so its calls carry the agent's
	// principal -- but it outlives the request that opened it, so it is
	// rooted in a fresh context, not that request's.
	base := context.Background()
	if t.Principal != nil {
		base = pkgauth.WithPrincipal(base, t.Principal)
	}
	ctx, cancel := context.WithCancel(base)
	return &stream{
		sess:      t.Session,
		conn:      ConnectorRef{ID: t.Connector.ID, Slug: client.Qualifier(t.Connector)},
		ctx:       ctx,
		cancel:    cancel,
		ready:     make(chan struct{}),
		needs:     make(map[string]struct{}),
		target:    t,
		answering: make(chan struct{}, maxRequestsInFlight),
	}
}

// Release drops need from sess's stream to connectorID and closes the
// stream when that was its last need. Releasing a need not held is a
// no-op.
func (m *Manager) Release(sess SessionRef, connectorID, need string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.streams[sess][connectorID]
	if s == nil {
		return
	}
	delete(s.needs, need)
	if len(s.needs) == 0 {
		m.removeLocked(s)
		s.cancel()
	}
}

// CloseSession closes every stream sess holds and forgets which of its
// connectors had none. It does not wait for the loops to exit.
func (m *Manager) CloseSession(sess SessionRef) {
	m.mu.Lock()
	streams := m.streams[sess]
	delete(m.streams, sess)
	delete(m.unsupported, sess)
	m.mu.Unlock()

	for _, s := range streams {
		s.cancel()
	}
}

// Close closes every stream and waits for every goroutine the Manager
// started to exit.
func (m *Manager) Close() {
	m.mu.Lock()
	all := m.streams
	m.streams = make(map[SessionRef]map[string]*stream)
	m.unsupported = make(map[SessionRef]map[string]bool)
	m.mu.Unlock()

	for _, byConn := range all {
		for _, s := range byConn {
			s.cancel()
		}
	}
	m.wg.Wait()
}

// Streams reports how many streams are held (open, or reconnecting).
func (m *Manager) Streams() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, byConn := range m.streams {
		n += len(byConn)
	}
	return n
}

// Running reports how many goroutines the Manager has running: one loop
// per stream plus one per request being answered. A test that ends every
// session expects it to reach zero.
func (m *Manager) Running() int { return int(m.running.Load()) }

func (m *Manager) start(fn func()) {
	m.wg.Add(1)
	m.running.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.running.Add(-1)
		fn()
	}()
}

// removeLocked unregisters s if it is still the registered stream for
// its key. m.mu must be held.
func (m *Manager) removeLocked(s *stream) {
	byConn := m.streams[s.sess]
	if byConn[s.conn.ID] != s {
		return
	}
	delete(byConn, s.conn.ID)
	if len(byConn) == 0 {
		delete(m.streams, s.sess)
	}
}

// markUnsupported unregisters s and records that its connector has no
// stream for its session.
func (m *Manager) markUnsupported(s *stream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.streams[s.sess][s.conn.ID] != s {
		return // released or closed meanwhile; nothing to record
	}
	m.removeLocked(s)
	if m.unsupported[s.sess] == nil {
		m.unsupported[s.sess] = make(map[string]bool)
	}
	m.unsupported[s.sess][s.conn.ID] = true
}

// run is a stream's loop: connect, read until the stream ends, back off,
// reconnect -- until the stream is no longer needed, the connector turns
// out to have no stream, or the session is gone.
func (m *Manager) run(s *stream) {
	// Cancel first, then wait: a request still being answered sees the
	// stream end rather than holding the loop open.
	var answers sync.WaitGroup
	defer answers.Wait()
	defer s.cancel()

	first := true
	signalReady := func(err error) {
		if first {
			s.firstErr = err
			close(s.ready)
			first = false
		}
	}
	defer signalReady(nil) // ends a wait on a stream closed before it connected

	failures := 0
	for {
		if !first || failures > 0 {
			if !m.backoff(s.ctx, failures) {
				return
			}
			if !m.refresh(s) {
				return
			}
		}

		st, err := m.client.OpenStream(s.ctx, m.call(s), s.lastEventID)
		if errors.Is(err, client.ErrStreamUnsupported) {
			m.markUnsupported(s)
			m.log.Info("connector has no server-to-client stream; not retrying for this session",
				"connector_id", s.conn.ID, "session_id", s.sess.SessionID, "error", err)
			signalReady(ErrStreamUnsupported)
			return
		}
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			failures++
			m.log.Warn("connector stream failed to open; will retry",
				"connector_id", s.conn.ID, "session_id", s.sess.SessionID, "attempt", failures, "error", err)
			signalReady(nil)
			continue
		}
		signalReady(nil)
		m.log.Debug("connector stream open", "connector_id", s.conn.ID, "session_id", s.sess.SessionID,
			"resumed_from", s.lastEventID)

		opened := time.Now()
		delivered := m.read(s, st, &answers)
		_ = st.Close()
		if s.ctx.Err() != nil {
			return
		}
		// A stream that carried something, or stayed up for a whole
		// backoff cap, was a success: start the next backoff from the
		// bottom. One that drops straight after opening keeps climbing.
		if delivered || time.Since(opened) >= m.maxWait {
			failures = 0
		}
		failures++
		m.log.Info("connector stream ended; reconnecting",
			"connector_id", s.conn.ID, "session_id", s.sess.SessionID, "resume", s.lastEventID != "")
	}
}

// read dispatches every message on st until it ends, reporting whether
// any arrived.
func (m *Manager) read(s *stream, st *client.Stream, answers *sync.WaitGroup) bool {
	// Close unblocks Next when the stream is no longer needed.
	stop := context.AfterFunc(s.ctx, func() { _ = st.Close() })
	defer stop()

	delivered := false
	for {
		msg, err := st.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && s.ctx.Err() == nil {
				m.log.Debug("connector stream read failed", "connector_id", s.conn.ID, "error", err)
			}
			return delivered
		}
		delivered = true
		if msg.EventID != "" {
			s.lastEventID = msg.EventID
		}
		m.dispatch(s, msg.Raw, answers)
	}
}

// dispatch routes one message: a notification to the handler, a request
// to be answered, anything else (a response to nothing the gateway sent
// on this stream) dropped.
func (m *Manager) dispatch(s *stream, raw json.RawMessage, answers *sync.WaitGroup) {
	var probe struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		m.log.Debug("dropping an unparseable message from a connector stream", "connector_id", s.conn.ID, "error", err)
		return
	}
	var msg mcp.Request
	if err := json.Unmarshal(raw, &msg); err != nil || probe.Method == "" {
		return
	}

	if len(probe.ID) == 0 || string(probe.ID) == "null" {
		m.notes.HandleNotification(s.ctx, s.sess, s.conn, msg)
		return
	}

	// Answer under the connector's own id, byte for byte: a number
	// decoded through float64 and re-encoded could come back different.
	id := probe.ID
	select {
	case s.answering <- struct{}{}:
	default:
		m.log.Warn("connector sent more concurrent requests than the gateway answers; refusing one",
			"connector_id", s.conn.ID, "method", msg.Method, "limit", maxRequestsInFlight)
		resp := mcp.NewErrorResponse(id, mcp.NewInternalError("too many concurrent requests"))
		target := s.target
		answers.Add(1)
		m.start(func() {
			defer answers.Done()
			m.reply(s, target, resp)
		})
		return
	}

	target := s.target
	answers.Add(1)
	m.start(func() {
		defer answers.Done()
		defer func() { <-s.answering }()

		result, rpcErr := m.requests.HandleRequest(s.ctx, s.sess, s.conn, msg)
		var resp *mcp.Response
		if rpcErr != nil {
			resp = mcp.NewErrorResponse(id, rpcErr)
		} else {
			if len(result) == 0 {
				result = json.RawMessage(`{}`)
			}
			resp = &mcp.Response{JSONRPC: mcp.Version, ID: id, Result: result}
		}
		m.reply(s, target, resp)
	})
}

func (m *Manager) reply(s *stream, target Target, resp *mcp.Response) {
	if s.ctx.Err() != nil {
		return
	}
	call := m.callFor(target)
	if err := m.client.Reply(s.ctx, call, resp); err != nil && s.ctx.Err() == nil {
		m.log.Warn("failed to answer a connector's request", "connector_id", s.conn.ID, "error", err)
	}
}

func (m *Manager) call(s *stream) client.Call { return m.callFor(s.target) }

func (m *Manager) callFor(t Target) client.Call {
	return client.Call{
		Connector:       t.Connector,
		SessionID:       t.Backend.SessionID,
		ProtocolVersion: t.Backend.ProtocolVersion,
		Inbound:         t.Inbound,
		Trace:           t.Trace,
	}
}

// refresh re-reads the session before a reconnect: a session that is
// gone ends the stream (and its session's other streams), and a backend
// handle that changed -- the gateway re-handshook after a failure -- is
// the one to reconnect on. A lookup that fails keeps the old handle; the
// sweep will catch a session that is really gone.
func (m *Manager) refresh(s *stream) bool {
	if m.lookup == nil {
		return true
	}
	b, alive, err := m.lookup(s.ctx, s.sess, s.conn.ID)
	if err != nil {
		if s.ctx.Err() != nil {
			return false
		}
		m.log.Debug("session lookup before reconnect failed; reconnecting on the handle held",
			"session_id", s.sess.SessionID, "error", err)
		return true
	}
	if !alive {
		m.log.Info("agent session is gone; closing its connector streams", "session_id", s.sess.SessionID)
		m.sessionGone(s.sess)
		return false
	}
	if b.ProtocolVersion != "" {
		s.target.Backend = b
	}
	return true
}

func (m *Manager) sessionGone(sess SessionRef) {
	m.CloseSession(sess)
	if m.onGone != nil {
		m.onGone(sess)
	}
}

// backoff waits before reconnect attempt n (n >= 1 after a failure, 0
// after a clean end): exponential from InitialBackoff, capped at
// MaxBackoff, with +/-25% jitter so a fleet of gateways does not
// reconnect in lockstep to a connector that just restarted. It reports
// false when the stream was closed while waiting.
func (m *Manager) backoff(ctx context.Context, n int) bool {
	if n > 0 {
		n--
	}
	d := float64(m.initial) * math.Pow(2, float64(n))
	d *= 0.75 + 0.5*rand.Float64() //nolint:gosec // jitter, not a secret
	if d > float64(m.maxWait) {
		d = float64(m.maxWait)
	}
	t := time.NewTimer(time.Duration(d))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Sweep closes the streams of every session Lookup reports gone. The
// transport ends a session's streams itself on DELETE /mcp; Sweep is
// what catches the rest -- a session that expired, or one deleted on
// another replica.
func (m *Manager) Sweep(ctx context.Context) {
	if m.lookup == nil {
		return
	}
	m.mu.Lock()
	sessions := make([]SessionRef, 0, len(m.streams))
	for sess := range m.streams {
		sessions = append(sessions, sess)
	}
	m.mu.Unlock()

	for _, sess := range sessions {
		_, alive, err := m.lookup(ctx, sess, "")
		if err != nil {
			m.log.Debug("session lookup during upstream sweep failed", "session_id", sess.SessionID, "error", err)
			continue
		}
		if !alive {
			m.log.Info("agent session is gone; closing its connector streams", "session_id", sess.SessionID)
			m.sessionGone(sess)
		}
	}
}

// Routine returns the sweep as a supervisor.Routine running every
// SweepInterval. Stopping it closes every stream.
func (m *Manager) Routine() supervisor.Routine {
	return &sweepRoutine{m: m, done: make(chan struct{})}
}

type sweepRoutine struct {
	m    *Manager
	done chan struct{}
	once sync.Once
}

func (r *sweepRoutine) Name() string { return "mcp-upstream-streams" }

func (r *sweepRoutine) Init(context.Context) error { return nil }

func (r *sweepRoutine) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.m.sweep)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return nil
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.m.Sweep(ctx)
		}
	}
}

func (r *sweepRoutine) Stop(context.Context) error {
	r.once.Do(func() {
		close(r.done)
		r.m.Close()
	})
	return nil
}
