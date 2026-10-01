// Package session is the public seam for MCP session persistence: the
// Record an MCP session is reduced to when it leaves the process, the
// Store that keeps Records, the Notifier that carries notifications
// between gateway replicas (tenant-wide tools/list_changed, and messages
// addressed to one session's stream), and the driver registry that binds
// a `sessions.store` name to an implementation.
//
// It exists so a session backend can live in a separate Go module. The
// gateway's own session logic (id minting, the sliding idle window, the
// tenant check, the per-connector backend handles) stays in
// internal/dataplane/session, which is unreachable from another module;
// everything that logic needs from storage is expressed here, in terms a
// plugin can implement.
//
// Two backends ship in this module and register themselves from an
// init(): pkg/session/memory (driver "memory", the default: one process,
// one replica) and pkg/session/redis (driver "redis": any replica can
// serve any session, and a tools/list_changed -- or a notification for
// one session -- raised on one replica reaches the SSE streams held open
// on the others). A binary blank-imports
// the drivers it wants, exactly as cmd/gateway does. A third backend is
// added by implementing Store (and Notifier, when the backend can carry a
// broadcast), registering it with Register, and passing
// pkg/session/sessiontest -- see CONTRIBUTING.md, "Adding a session
// backend".
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// ErrNotFound is returned by Store.Get for a session id that is unknown,
// or whose Record has expired. Callers test for it with errors.Is; a
// Store must map its own "no such key" onto it rather than returning a
// driver-specific error.
var ErrNotFound = errors.New("session: not found")

// Backend is what the gateway holds for one connector inside a session:
// the backend MCP server's own session id, and the protocol version that
// backend was found to accept. The gateway's client negotiates the
// version once per connector (with a one-shot fallback to the legacy
// version) and remembers the outcome here so the fallback is paid for
// once per session, not once per call.
//
// A non-empty ProtocolVersion is the "initialized" flag: the gateway has
// completed initialize + notifications/initialized with this connector on
// the session's behalf. SessionID may legitimately be empty for a backend
// that does not issue session ids. The zero Backend means "no handshake
// yet" -- or "handshake forgotten": the gateway zeroes a connector's
// Backend when the backend rejects a call, so the next call re-handshakes
// rather than replaying a handle the backend has dropped.
type Backend struct {
	SessionID       string `json:"session_id,omitempty"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
}

// Record is one MCP session as it is stored: a plain, serializable value
// with no methods that matter, so any backend can encode it (the shipped
// Redis driver uses JSON; a SQL driver would use columns). The gateway
// converts between Record and its in-memory session on every Get and
// Save, so a Store may hand back a decoded copy and must never assume the
// caller will keep a pointer it returned.
//
// Every field is required for the gateway to serve the session on a
// replica other than the one that created it; a Store must round-trip all
// of them (pkg/session/sessiontest checks that it does).
type Record struct {
	// ID is the Mcp-Session-Id: 32 bytes of cryptographic randomness,
	// URL-safe base64 (43 characters). It is a bearer handle, so a Store
	// should treat it as a secret (no debug-logging of keys).
	ID string `json:"id"`

	// TenantID is the tenant whose API key ran initialize. Manager.Resolve
	// refuses to hand this session to a caller from another tenant, so a
	// leaked id cannot cross a tenant boundary.
	TenantID string `json:"tenant_id"`

	// Principal identifies the authenticated caller that created the
	// session (pkg/auth.Principal.Subject -- for API-key auth, the key's
	// id). Recorded for the access log and for audit; not consulted on
	// Resolve, which checks the tenant only, since a tenant's keys are
	// interchangeable for the purpose of an MCP conversation.
	Principal string `json:"principal"`

	// ClientInfo is what the client sent as initialize.params.clientInfo
	// (name and version). Informational.
	ClientInfo mcp.Implementation `json:"client_info"`

	// ProtocolVersion is the MCP protocol version the gateway negotiated
	// with the CLIENT in the initialize response. It is distinct from
	// each connector's Backend.ProtocolVersion, which is what the gateway
	// negotiated with that backend on the client's behalf.
	ProtocolVersion string `json:"protocol_version,omitempty"`

	// CreatedAt is when initialize minted the session.
	CreatedAt time.Time `json:"created_at"`

	// LastSeenAt is when the session was last presented on a request.
	// Manager.Resolve moves it forward on every use.
	LastSeenAt time.Time `json:"last_seen_at"`

	// ExpiresAt is when the session lapses if it is not used again:
	// LastSeenAt plus the configured idle TTL, recomputed on every use
	// (a sliding window, not an absolute lifetime). See Store for how a
	// backend must honour it.
	ExpiresAt time.Time `json:"expires_at"`

	// Backends holds the per-connector handles, keyed by connector id.
	// It is the reason sessions are worth sharing across replicas at
	// all: without it, a replica that had not seen the session would
	// have to re-handshake with every backend on the client's behalf.
	// A nil and an empty map are equivalent.
	Backends map[string]Backend `json:"backends,omitempty"`

	// ClientCapabilities holds the client capabilities the CLIENT
	// declared at initialize that decide which server-initiated requests
	// it can be sent -- "sampling", "elicitation" and "roots" -- each
	// kept as the raw JSON object it sent, so sub-fields
	// (roots.listChanged) pass through to connectors untouched. An absent
	// key means the client did not declare that capability. A nil and an
	// empty map are equivalent.
	ClientCapabilities map[string]json.RawMessage `json:"client_capabilities,omitempty"`
}

// Clone returns a deep copy of r (the Backends and ClientCapabilities
// maps are copied, not shared). Stores that keep Records in process use it so a caller's
// later mutation of a Record it saved, or one it was handed by Get, does
// not reach into the store.
func (r *Record) Clone() *Record {
	if r == nil {
		return nil
	}
	out := *r
	if r.Backends != nil {
		out.Backends = make(map[string]Backend, len(r.Backends))
		for k, v := range r.Backends {
			out.Backends[k] = v
		}
	}
	if r.ClientCapabilities != nil {
		out.ClientCapabilities = make(map[string]json.RawMessage, len(r.ClientCapabilities))
		for k, v := range r.ClientCapabilities {
			out.ClientCapabilities[k] = append(json.RawMessage(nil), v...)
		}
	}
	return &out
}

// Expired reports whether the record has lapsed as of now.
func (r *Record) Expired(now time.Time) bool {
	return !now.Before(r.ExpiresAt)
}

// Validate reports why r cannot be stored: an empty ID, or a zero
// ExpiresAt (a Store needs it for its TTL, and a session that never
// expires is a bug, not a feature). Stores call it at the top of Save.
func (r *Record) Validate() error {
	switch {
	case r == nil:
		return errors.New("session: nil record")
	case r.ID == "":
		return errors.New("session: record has an empty id")
	case r.ExpiresAt.IsZero():
		return fmt.Errorf("session: record %q has no expiry", r.ID)
	}
	return nil
}

// Store persists Records. Implementations must be safe for concurrent
// use from many goroutines (one per in-flight MCP request).
//
// Expiry contract. Every Record carries an ExpiresAt, and a Store MUST
// NOT return a Record whose ExpiresAt is at or before the Store's own
// current time: Get returns ErrNotFound for it, exactly as for an unknown
// id, and MAY drop it in passing. A backend with native TTLs (Redis)
// meets this by writing the key with a TTL that ends at ExpiresAt; one
// without (a SQL table, an in-process map) filters on read, and may also
// implement Sweeper so the gateway can reclaim what nobody reads again.
// Saving a Record whose ExpiresAt has already passed is therefore
// equivalent to Delete: a subsequent Get must not return it.
//
// The gateway re-checks ExpiresAt against its own clock after Get, so a
// store's clock need not agree with the gateway's to the millisecond;
// the store-side check is what keeps a dead session from being served
// by a replica whose clock is behind, and what keeps storage bounded.
//
// Consistency contract. Save is an unconditional upsert: the whole Record
// replaces whatever was stored under its ID (a connector handle absent
// from the new Record is gone). Two replicas saving the same session
// concurrently is allowed and the later write wins; the gateway is
// designed so the worst a lost write costs is one backend re-handshake,
// never a wrong tenant. Get must hand back a Record the caller may
// mutate freely without affecting the stored copy.
type Store interface {
	// Get returns the Record stored under id, or ErrNotFound when there
	// is none or it has expired.
	Get(ctx context.Context, id string) (*Record, error)
	// Save inserts or replaces r, honouring r.ExpiresAt as described
	// on Store. It returns an error for a Record that fails Validate.
	Save(ctx context.Context, r *Record) error
	// Delete removes the Record under id. Deleting an id that is not
	// stored is not an error.
	Delete(ctx context.Context, id string) error
	// Close releases the Store's resources. The gateway calls it once,
	// at shutdown, after the last request has finished.
	Close() error
}

// Sweeper is implemented by a Store that cannot expire Records on its
// own and needs to be told to reclaim them. The gateway runs Sweep on
// the configured sessions.cleanup_interval when its Store implements
// this; a Store with native TTLs (Redis) simply does not.
type Sweeper interface {
	// Sweep drops every Record whose ExpiresAt is at or before now and
	// returns how many it dropped.
	Sweep(ctx context.Context, now time.Time) (int, error)
}

// Notifier carries notifications between gateway replicas. An SSE
// stream (GET /mcp/stream) is a socket on ONE replica, so only that
// replica can write to it; when something another replica learned has
// to reach it, the message has to be relayed. A Notifier is that relay,
// for two kinds of message:
//
//   - the tenant-wide tools/list_changed signal (Publish/Subscribe): a
//     tenant's tool list changed, which every stream of the tenant is
//     told;
//   - a message addressed to one session (PublishSession/
//     SubscribeSessions): a notification for that session's stream --
//     a subscribed resource's update, say -- raised on a replica that
//     holds no stream for the session. Each receiving replica delivers
//     it to the session's streams it holds, if any, and drops it
//     otherwise. The same channel carries the relay of server-initiated
//     requests (SessionMessage.Kind): the request, the receiver's
//     acknowledgement that a stream took it, and the agent's response
//     when it lands on a replica other than the one waiting for it.
//
// Delivery contract, for both kinds. A publish is delivered to the
// callback of every OTHER Notifier instance currently blocked in the
// matching subscribe on the same backend; it is never delivered to the
// publishing instance's own callback. The publisher delivers to its own
// local streams directly (they need no network hop and must not depend
// on one), so this rule is what makes each stream receive a message
// exactly once: once locally, or once from the relay, never both.
// Delivery is best-effort, at-most-once, to instances subscribed at that
// moment, in publish order per publisher. A missed tools/list_changed
// costs an agent a stale tool list until its next tools/list; a missed
// session message is lost, as it would be to a stream that dropped.
//
// Subscribe and SubscribeSessions block, invoking fn once per delivered
// message from the calling goroutine, until ctx is done, and then return
// nil. Any other return is a transport failure the caller may log and
// retry. fn must return promptly; it is called serially.
type Notifier interface {
	// Publish announces that tenantID's tool list changed. tenantID
	// must be non-empty.
	Publish(ctx context.Context, tenantID string) error
	// Subscribe delivers signals published by other instances to fn
	// until ctx is done.
	Subscribe(ctx context.Context, fn func(tenantID string)) error
	// PublishSession relays msg to the other instances. Its TenantID,
	// SessionID and Payload must all be non-empty.
	PublishSession(ctx context.Context, msg SessionMessage) error
	// SubscribeSessions delivers session messages published by other
	// instances to fn until ctx is done.
	SubscribeSessions(ctx context.Context, fn func(SessionMessage)) error
	// Close releases the Notifier's resources.
	Close() error
}

// Session message kinds. A SessionMessage's Kind says what a receiving
// replica does with its Payload.
const (
	// KindNotification (the zero value) is a notification for the
	// session's streams: the receiver writes it to the ones it holds.
	KindNotification = ""
	// KindRequest is a server-initiated request (sampling, elicitation,
	// roots) for the session's streams. A receiver that wrote it to at
	// least one stream answers with a KindAck, so the replica waiting on
	// the request knows the agent has it.
	KindRequest = "request"
	// KindAck confirms a KindRequest was written to a stream. Its
	// Payload is the request's JSON-RPC id.
	KindAck = "ack"
	// KindResponse is the agent's JSON-RPC response to a KindRequest,
	// POSTed to a replica other than the one waiting for it. Only the
	// replica holding the request's pending entry acts on it.
	KindResponse = "response"
)

// SessionMessage is one message addressed to a single session, as it
// travels between replicas.
type SessionMessage struct {
	// TenantID and SessionID address the session. A receiver matches
	// both, so a session id alone never reaches another tenant.
	TenantID  string `json:"tenant_id"`
	SessionID string `json:"session_id"`
	// Kind is one of the Kind* constants; empty is KindNotification. A
	// Notifier carries it verbatim and never interprets it.
	Kind string `json:"kind,omitempty"`
	// Payload is the encoded JSON-RPC message (for KindNotification and
	// KindRequest, written to the session's streams verbatim).
	Payload json.RawMessage `json:"payload"`
}

// Validate reports why m cannot be published: an empty tenant, session
// or payload.
func (m SessionMessage) Validate() error {
	switch {
	case m.TenantID == "":
		return errors.New("session: session message has an empty tenant id")
	case m.SessionID == "":
		return errors.New("session: session message has an empty session id")
	case len(m.Payload) == 0:
		return errors.New("session: session message has an empty payload")
	}
	return nil
}

// Config is what Open hands a driver. The gateway fills the connection
// fields from its `redis` config section whenever sessions.store names a
// driver other than memory, and Options from `sessions.options`; a driver
// reads what it needs and ignores the rest. The memory driver ignores
// all of it.
type Config struct {
	// Addr is the backend's host:port.
	Addr string
	// Password authenticates to the backend; empty for none.
	Password string
	// DB selects a logical database on backends that have them (Redis).
	DB int
	// TLS dials with TLS (minimum version 1.2) when true.
	TLS bool
	// Options carries driver-specific settings a third-party driver
	// needs beyond the fields above, verbatim from sessions.options in
	// the gateway's config. The built-in drivers ignore it.
	Options map[string]string
}

// Factory builds a driver's Store and, when the driver can carry the
// cross-replica relay, its Notifier. A driver whose scope is one
// process (memory) returns a nil Notifier and the gateway keeps the
// signal local. Both values, when non-nil, are closed by the gateway at
// shutdown; a driver that shares a connection between them must make the
// second Close a no-op.
type Factory func(ctx context.Context, cfg Config) (Store, Notifier, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register makes a driver available to Open under name. It panics on an
// empty name, a nil factory or a name registered twice -- programmer
// errors caught at init time, not conditions to recover from. Drivers
// call it from an init() so a blank import is enough to enable them.
func Register(name string, factory Factory) {
	if strings.TrimSpace(name) == "" {
		panic("session: Register called with an empty driver name")
	}
	if factory == nil {
		panic("session: Register called with a nil factory for driver " + name)
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic("session: Register called twice for driver " + name)
	}
	registry[name] = factory
}

// Drivers returns the registered driver names, sorted.
func Drivers() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Open builds the Store (and Notifier, possibly nil) of the driver
// registered under name. An unknown name is an error that lists the
// drivers that are registered, since the usual cause is a missing blank
// import.
func Open(ctx context.Context, name string, cfg Config) (Store, Notifier, error) {
	registryMu.RLock()
	factory, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, nil, fmt.Errorf("session: unknown driver %q (registered: %s; missing blank import?)",
			name, strings.Join(Drivers(), ", "))
	}

	st, n, err := factory(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("session: open driver %q: %w", name, err)
	}
	if st == nil {
		return nil, nil, fmt.Errorf("session: driver %q returned no store", name)
	}
	return st, n, nil
}
