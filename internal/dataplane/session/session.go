// Package session implements MCP-spec sessions: initialize mints a
// cryptographically random Mcp-Session-Id, the client echoes it on
// subsequent requests, and the gateway hangs the per-connector backend
// session ids off it.
//
// Two things are deliberately NOT here, both carried over from the
// gateway's predecessor as bugs rather than features:
//
//   - There is no shared "default session". Every caller that does not
//     present a session id is served statelessly (see Anonymous), rather
//     than being folded into one process-wide session whose backend
//     handles then leak between tenants.
//   - The session id is the spec's Mcp-Session-Id header, not a bespoke
//     X-Session-ID, so an off-the-shelf MCP client works unmodified.
//
// Persistence is behind the public seam pkg/session: Manager converts
// its Session to and from a pkg/session.Record on every read and write,
// and a pkg/session.Store (memory by default, Redis for more than one
// replica, or a driver from another module) keeps the Records. Nothing
// above Manager knows which store is in use.
package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	pkgsession "github.com/Tuskira/tusk-ai-secured-gateway/pkg/session"
)

// ErrNotFound is returned by Manager.Resolve for a session id that is
// unknown, expired, or belongs to another tenant. The transport turns it
// into JSON-RPC -32000, whose spec-defined meaning is "re-initialize".
// It is pkg/session's error, so a store's not-found and the manager's
// are one and the same to errors.Is.
var ErrNotFound = pkgsession.ErrNotFound

// idBytes is the entropy in a session id. The MCP spec requires session
// ids to be cryptographically secure because they are replayable handles;
// 32 bytes is the same budget the gateway's API keys use.
const idBytes = 32

// Backend is what the gateway holds for one connector inside a session;
// see pkg/session.Backend.
type Backend = pkgsession.Backend

// Backends is the per-request view of backend handles. Both a real
// Session and the Anonymous store satisfy it, so the orchestrator treats
// a session-bearing and a stateless request identically.
type Backends interface {
	// Backend returns the handle held for connectorID, or the zero
	// Backend when there is none yet.
	Backend(connectorID string) Backend
	// SetBackend records a handle, overwriting any previous one.
	SetBackend(connectorID string, b Backend)
}

// Session is one MCP session. Callers must treat the exported fields as
// read-only after creation and reach the backend handles through the
// Backends methods, which are safe for concurrent use (the initialize
// fan-out writes to several connectors at once).
//
// A Session is this process's working copy of a stored Record: every
// mutation (touch, backend handle) is tracked and must reach the store
// through Manager.Save or Manager.SaveIfChanged, or another replica --
// or this one, on its next Resolve -- will not see it.
type Session struct {
	ID              string
	TenantID        string
	Principal       string
	ClientInfo      mcp.Implementation
	ProtocolVersion string
	CreatedAt       time.Time
	// ClientCapabilities is what the client declared at initialize of
	// the capabilities that invite server-initiated requests; see
	// pkg/session.Record.ClientCapabilities.
	ClientCapabilities map[string]json.RawMessage

	mu         sync.Mutex
	lastSeenAt time.Time
	expiresAt  time.Time
	backends   map[string]Backend
	// changed is set when a backend handle changes and cleared when the
	// session is persisted; see Manager.SaveIfChanged.
	changed bool
}

// Backend implements Backends.
func (s *Session) Backend(connectorID string) Backend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backends[connectorID]
}

// SetBackend implements Backends.
func (s *Session) SetBackend(connectorID string, b Backend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backends == nil {
		s.backends = make(map[string]Backend)
	}
	if prev, ok := s.backends[connectorID]; ok && prev == b {
		return
	}
	s.backends[connectorID] = b
	s.changed = true
}

// LastSeenAt returns when the session was last used.
func (s *Session) LastSeenAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeenAt
}

// ExpiresAt returns when the session lapses if it is not used again.
func (s *Session) ExpiresAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expiresAt
}

// touch slides the session's idle window forward.
func (s *Session) touch(now time.Time, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSeenAt = now
	s.expiresAt = now.Add(ttl)
}

// takeChanged reports whether a backend handle changed since the last
// persist, clearing the flag.
func (s *Session) takeChanged() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.changed
	s.changed = false
	return c
}

// markChanged re-arms the flag after a persist that failed.
func (s *Session) markChanged() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changed = true
}

// expired reports whether the session has lapsed as of now.
func (s *Session) expired(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !now.Before(s.expiresAt)
}

// record snapshots s as the Record the store keeps, under s's lock.
func (s *Session) record() *pkgsession.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &pkgsession.Record{
		ID:              s.ID,
		TenantID:        s.TenantID,
		Principal:       s.Principal,
		ClientInfo:      s.ClientInfo,
		ProtocolVersion: s.ProtocolVersion,
		CreatedAt:       s.CreatedAt,
		LastSeenAt:      s.lastSeenAt,
		ExpiresAt:       s.expiresAt,
		Backends:        make(map[string]Backend, len(s.backends)),

		ClientCapabilities: s.ClientCapabilities,
	}
	for id, b := range s.backends {
		r.Backends[id] = b
	}
	return r
}

// fromRecord rebuilds a Session from its stored Record. The Record is
// the store's copy to give away (see pkg/session.Store), so its map is
// adopted, not copied.
func fromRecord(r *pkgsession.Record) *Session {
	backends := r.Backends
	if backends == nil {
		backends = make(map[string]Backend)
	}
	return &Session{
		ID:              r.ID,
		TenantID:        r.TenantID,
		Principal:       r.Principal,
		ClientInfo:      r.ClientInfo,
		ProtocolVersion: r.ProtocolVersion,
		CreatedAt:       r.CreatedAt,
		lastSeenAt:      r.LastSeenAt,
		expiresAt:       r.ExpiresAt,
		backends:        backends,

		ClientCapabilities: r.ClientCapabilities,
	}
}

// Options configures a Manager. Zero values fall back to documented
// defaults.
type Options struct {
	// TTL is the idle lifetime of a session. Zero uses one hour.
	TTL time.Duration
	// CleanupInterval is how often the cleanup routine sweeps a store
	// that needs sweeping. Zero uses five minutes.
	CleanupInterval time.Duration
	// Now returns the current time; tests override it.
	Now func() time.Time
	// Logger receives the cleanup routine's log lines.
	Logger *slog.Logger
}

// Manager issues, resolves and expires sessions over a pkg/session.Store.
type Manager struct {
	store           pkgsession.Store
	ttl             time.Duration
	cleanupInterval time.Duration
	now             func() time.Time
	logger          *slog.Logger

	anonymous *Anonymous
}

// NewManager returns a Manager over store, which must not be nil.
func NewManager(store pkgsession.Store, opts Options) *Manager {
	if store == nil {
		panic("session: NewManager called with a nil store")
	}
	if opts.TTL <= 0 {
		opts.TTL = time.Hour
	}
	if opts.CleanupInterval <= 0 {
		opts.CleanupInterval = 5 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	return &Manager{
		store:           store,
		ttl:             opts.TTL,
		cleanupInterval: opts.CleanupInterval,
		now:             opts.Now,
		logger:          opts.Logger,
		anonymous:       NewAnonymous(),
	}
}

// Create mints a new session for the authenticated caller and persists
// it. protocolVersion is what the gateway negotiated with the client;
// caps is what it declared of the capabilities that invite
// server-initiated requests (nil for none).
func (m *Manager) Create(ctx context.Context, tenantID, principal, protocolVersion string, clientInfo mcp.Implementation, caps map[string]json.RawMessage) (*Session, error) {
	id, err := NewID()
	if err != nil {
		return nil, err
	}

	now := m.now()
	s := &Session{
		ID:              id,
		TenantID:        tenantID,
		Principal:       principal,
		ClientInfo:      clientInfo,
		ProtocolVersion: protocolVersion,
		CreatedAt:       now,
		lastSeenAt:      now,
		expiresAt:       now.Add(m.ttl),
		backends:        make(map[string]Backend),

		ClientCapabilities: caps,
	}

	if err := m.store.Save(ctx, s.record()); err != nil {
		return nil, fmt.Errorf("session: persist new session: %w", err)
	}
	return s, nil
}

// Resolve returns the session named by id, sliding its idle window
// forward. It returns ErrNotFound when the id is unknown, when it has
// expired (the expired record is dropped on the way out), or when it
// belongs to a different tenant than the caller.
//
// The tenant check matters: a session id is a bearer handle, and without
// it a leaked id from tenant A presented with tenant B's API key would
// hand B the backend handles A negotiated.
//
// Expiry is decided here, by the Manager's clock, even though the store
// also refuses to return an expired Record (pkg/session.Store): the
// store's check keeps storage bounded and covers a replica whose clock
// is behind; this one is what a client actually meets.
func (m *Manager) Resolve(ctx context.Context, id, tenantID string) (*Session, error) {
	if id == "" {
		return nil, ErrNotFound
	}

	r, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	s := fromRecord(r)

	now := m.now()
	if s.expired(now) {
		_ = m.store.Delete(ctx, id)
		return nil, ErrNotFound
	}
	if tenantID != "" && s.TenantID != tenantID {
		return nil, ErrNotFound
	}

	s.touch(now, m.ttl)
	if err := m.Save(ctx, s); err != nil {
		return nil, fmt.Errorf("session: touch session: %w", err)
	}
	return s, nil
}

// Peek returns the session named by id without sliding its idle window,
// under the same rules as Resolve: ErrNotFound when it is unknown,
// expired or another tenant's. It is for the gateway's own bookkeeping
// -- is a session whose connector streams it holds still alive, which
// backend handle does it hold now -- which is not the client using its
// session and must not keep it alive.
func (m *Manager) Peek(ctx context.Context, id, tenantID string) (*Session, error) {
	if id == "" {
		return nil, ErrNotFound
	}
	r, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	s := fromRecord(r)
	if s.expired(m.now()) || (tenantID != "" && s.TenantID != tenantID) {
		return nil, ErrNotFound
	}
	return s, nil
}

// Save persists a session a caller has mutated (e.g. after recording a
// backend handle). It is the serialization point for every store.
func (m *Manager) Save(ctx context.Context, s *Session) error {
	if s == nil {
		return nil
	}
	s.takeChanged()
	if err := m.store.Save(ctx, s.record()); err != nil {
		s.markChanged()
		return err
	}
	return nil
}

// SaveIfChanged persists s only when one of its backend handles changed
// since it was last saved, so a request that reused its handles costs
// no second write (Resolve has already written the touch).
func (m *Manager) SaveIfChanged(ctx context.Context, s *Session) error {
	if s == nil || !s.takeChanged() {
		return nil
	}
	if err := m.store.Save(ctx, s.record()); err != nil {
		s.markChanged()
		return err
	}
	return nil
}

// Delete removes a session (the spec's DELETE on the MCP endpoint, and
// what a client should do when it is finished).
func (m *Manager) Delete(ctx context.Context, id string) error {
	return m.store.Delete(ctx, id)
}

// Anonymous returns the process-wide Backends used for requests that
// carry no session id. Backend handles are keyed by connector id, and a
// connector row is already tenant-scoped, so sharing them across
// session-less callers cannot cross a tenant boundary.
func (m *Manager) Anonymous() Backends { return m.anonymous }

// TTL returns the configured session lifetime.
func (m *Manager) TTL() time.Duration { return m.ttl }

// NewID mints a cryptographically random session id, rendered
// URL-safe so it survives a header round-trip unescaped.
func NewID() (string, error) {
	buf := make([]byte, idBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("session: generate id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Anonymous holds backend handles for requests with no gateway session.
// It is process-local by design: a stateless caller's handles are a
// per-replica convenience, not session state, and the background tool
// cache refresher uses it too.
type Anonymous struct {
	mu       sync.Mutex
	backends map[string]Backend
}

// NewAnonymous returns an empty Anonymous store.
func NewAnonymous() *Anonymous { return &Anonymous{backends: make(map[string]Backend)} }

var _ Backends = (*Anonymous)(nil)

// Backend implements Backends.
func (a *Anonymous) Backend(connectorID string) Backend {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.backends[connectorID]
}

// SetBackend implements Backends.
func (a *Anonymous) SetBackend(connectorID string, b Backend) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.backends[connectorID] = b
}

// Forget drops the handle held for connectorID. The orchestrator calls
// it when a backend rejects a session id, so the next call re-handshakes
// rather than replaying a handle the backend has forgotten.
func (a *Anonymous) Forget(connectorID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.backends, connectorID)
}
