package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/reqctx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// keepAliveInterval is how often an idle SSE stream emits a comment
// frame. Proxies and load balancers close a connection that has sent
// nothing for a while, and a comment is the cheapest thing that counts
// as traffic without being a protocol message.
const keepAliveInterval = 25 * time.Second

// sessionQueue is how many per-session messages a stream may fall
// behind by before new ones are dropped. Progress is best-effort and
// monotonic, so dropping an intermediate update under backpressure loses
// nothing a later one does not restate.
const sessionQueue = 64

// Hub fans server-initiated notifications out to the SSE streams open
// on this replica.
//
// It carries three kinds. Tenant-wide signals (tools/list_changed) are
// edges: "your tool list changed" is true for every client of the
// tenant and reveals nothing about any one of them. Per-session messages
// (notifications/progress for a tools/call in flight, a subscribed
// resource's update, a connector's prompts or resources list_changed, a
// connector's sampling, elicitation or roots request relayed to the
// agent) are addressed to the streams one session has open, keyed by
// tenant AND session id so a session id alone can never reach another
// tenant's stream. Per-profile signals (see NotifyProfileListChanged) are
// addressed to the streams opened with a given X-Agent-Profile-Name,
// keyed by tenant AND profile slug -- a skill/command/instructions change
// on one profile must not wake a stream bound to a different profile in
// the same tenant.
type Hub struct {
	mu          sync.Mutex
	subs        map[string]map[chan struct{}]struct{}
	sessions    map[sessionKey]map[chan []byte]struct{}
	profileSubs map[profileKey]map[chan mcp.Request]struct{}
}

type sessionKey struct{ tenantID, sessionID string }

// profileKey scopes a profile-bound stream subscription: same (tenant,
// slug) convention internal/dataplane/profile.Enforcer's own cache uses,
// so a write that calls Enforcer.InvalidateSlug can wake exactly the
// streams that resolution affects and no others.
type profileKey struct{ tenantID, slug string }

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{
		subs:        make(map[string]map[chan struct{}]struct{}),
		sessions:    make(map[sessionKey]map[chan []byte]struct{}),
		profileSubs: make(map[profileKey]map[chan mcp.Request]struct{}),
	}
}

// Subscribe registers a listener for a tenant and returns it with the
// function that removes it. The channel is buffered by one and written
// non-blockingly: a notification is an edge, not a queue, so a slow
// client coalescing two signals into one loses nothing.
func (h *Hub) Subscribe(tenantID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	h.mu.Lock()
	if h.subs[tenantID] == nil {
		h.subs[tenantID] = make(map[chan struct{}]struct{})
	}
	h.subs[tenantID][ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if set, ok := h.subs[tenantID]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.subs, tenantID)
			}
		}
		h.mu.Unlock()
	}
}

// SubscribeSession registers a listener for one session's messages and
// returns it with the function that removes it. Each message is an
// encoded JSON-RPC notification, ready to frame.
func (h *Hub) SubscribeSession(tenantID, sessionID string) (<-chan []byte, func()) {
	ch := make(chan []byte, sessionQueue)
	key := sessionKey{tenantID, sessionID}

	h.mu.Lock()
	if h.sessions[key] == nil {
		h.sessions[key] = make(map[chan []byte]struct{})
	}
	h.sessions[key][ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if set, ok := h.sessions[key]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.sessions, key)
			}
		}
		h.mu.Unlock()
	}
}

// ToolsListChanged signals every stream open for the tenant.
func (h *Hub) ToolsListChanged(tenantID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[tenantID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// SubscribeProfile registers a listener for one profile's list-changed
// notifications and returns it with the function that removes it. Unlike
// Subscribe's bare struct{} edge, a profile subscriber receives full
// mcp.Request notifications: a skill/command attach or detach changes
// tools/list (gateway__skill's own listing) and/or prompts/list (native
// commands), and the two can fire independently, so the stream needs to
// know which one to relay.
func (h *Hub) SubscribeProfile(tenantID, slug string) (<-chan mcp.Request, func()) {
	ch := make(chan mcp.Request, 4)
	key := profileKey{tenantID, slug}

	h.mu.Lock()
	if h.profileSubs[key] == nil {
		h.profileSubs[key] = make(map[chan mcp.Request]struct{})
	}
	h.profileSubs[key][ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if set, ok := h.profileSubs[key]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.profileSubs, key)
			}
		}
		h.mu.Unlock()
	}
}

// NotifyProfileListChanged tells every stream open on THIS replica and
// bound to the profile at (tenantID, slug) -- i.e. opened with the
// matching X-Agent-Profile-Name header, see the stream handler -- that
// its tools and/or prompts may have changed: it sends both
// notifications/tools/list_changed and notifications/prompts/list_changed,
// as two separate frames, so a client that declared only one of those
// capabilities still gets exactly the notification it understands (an
// MCP client harmlessly ignores a list_changed notification for a
// capability it never declared, per the spec, so sending both
// unconditionally is safe -- this Hub does not track which capabilities
// any one stream's client declared).
//
// This is the same per-process reach as Enforcer.InvalidateSlug, which
// every caller of this method calls alongside: a stream on another
// replica of a multi-replica deployment is not reached here and instead
// notices the change within that replica's own enforcer cache TTL (see
// docs/profiles.md, "Skills and commands" -- live updates). A no-op when
// no stream is currently subscribed to this profile.
func (h *Hub) NotifyProfileListChanged(tenantID, slug string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	subs := h.profileSubs[profileKey{tenantID, slug}]
	if len(subs) == 0 {
		return
	}
	for _, method := range []string{mcp.NotificationToolsListChanged, mcp.NotificationPromptsListChanged} {
		msg := mcp.Request{JSONRPC: mcp.Version, Method: method}
		for ch := range subs {
			select {
			case ch <- msg:
			default:
				// The stream is too far behind to keep up; like every
				// other list_changed signal, a client that misses one
				// edge still re-lists on its own schedule.
			}
		}
	}
}

// Notify delivers msg to every stream the session has open on this
// replica and reports whether at least one took it. It never blocks: a
// session with no stream, or a stream too far behind, drops the message.
// It implements orchestrator.SessionNotifier.
func (h *Hub) Notify(tenantID, sessionID string, msg mcp.Request) bool {
	raw, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	return h.deliver(tenantID, sessionID, raw)
}

// DeliverRequest writes a server-initiated request to the session's
// streams on this replica, reporting whether one took it. It implements
// orchestrator.RequestDeliverer.
func (h *Hub) DeliverRequest(_ context.Context, tenantID, sessionID string, msg mcp.Request) bool {
	return h.Notify(tenantID, sessionID, msg)
}

// deliver is Notify for a message already encoded -- one relayed from
// another replica.
func (h *Hub) deliver(tenantID, sessionID string, raw []byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	delivered := false
	for ch := range h.sessions[sessionKey{tenantID, sessionID}] {
		select {
		case ch <- raw:
			delivered = true
		default:
		}
	}
	return delivered
}

// stream serves GET /mcp/stream: an SSE channel carrying
// server-initiated notifications, with keep-alives in between.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	principal, ok := pkgauth.PrincipalFrom(r.Context())
	if !ok || principal == nil {
		writeUnauthorized(w)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Without flushing, every frame would sit in a buffer until
		// the response ended -- which, for a stream, is never.
		h.writeResponse(w, reqctx.From(r.Context()), nil,
			mcp.NewErrorResponse(nil, mcp.NewInternalError("streaming is not supported by this server")))
		return
	}

	// A stream opened with an Mcp-Session-Id also carries that session's
	// own messages (progress for its tools/calls). The id is resolved
	// within the caller's tenant exactly as POST /mcp resolves it, so a
	// guessed or stale id is refused rather than silently downgraded to
	// a tenant-only stream the client would wait on in vain.
	var sessionMsgs <-chan []byte
	if id := r.Header.Get(mcp.HeaderSessionID); id != "" {
		sess, err := h.deps.Sessions.Resolve(r.Context(), id, principal.TenantID)
		if err != nil {
			if !errors.Is(err, session.ErrNotFound) {
				h.log.Error("failed to resolve session for stream", "error", err)
			}
			h.writeResponse(w, reqctx.From(r.Context()), nil, mcp.NewErrorResponse(nil,
				mcp.NewError(mcp.ErrorCodeSessionNotFound, "session not found", nil)))
			return
		}
		reqctx.From(r.Context()).SetSession(sess.ID)
		msgs, unsubscribeSession := h.deps.Hub.SubscribeSession(principal.TenantID, sess.ID)
		defer unsubscribeSession()
		sessionMsgs = msgs
	}

	// A stream opened with an X-Agent-Profile-Name header -- the same
	// header POST /mcp reads to resolve the caller's profile -- also
	// carries that profile's own list-changed notifications: a
	// skill/command attach, detach, or instructions edit on the SAME
	// replica pushes here immediately, without waiting for the
	// enforcer's TTL. See Hub.NotifyProfileListChanged for the exact
	// reach and its limit.
	var profileMsgs <-chan mcp.Request
	slug := ""
	if principal.ProfileID != "" {
		// A key bound to a profile listens on that profile's stream
		// whatever the header says.
		slug = h.deps.Orchestrator.BoundProfileSlug(r.Context(), principal)
	} else if name := strings.TrimSpace(r.Header.Get(profile.Header)); name != "" {
		slug = profile.Slug(principal.TenantID, name)
	}
	if slug != "" {
		msgs, unsubscribeProfile := h.deps.Hub.SubscribeProfile(principal.TenantID, slug)
		defer unsubscribeProfile()
		profileMsgs = msgs
	}

	// Subscribe before the headers go out, so a client that has seen
	// the 200 knows its stream is already listening.
	signals, unsubscribe := h.deps.Hub.Subscribe(principal.TenantID)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Tell an nginx in front of us not to buffer the stream.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	reqctx.From(r.Context()).SetMethod("GET /mcp/stream")

	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()

	notification, err := json.Marshal(mcp.Request{JSONRPC: mcp.Version, Method: mcp.NotificationToolsListChanged})
	if err != nil {
		// A fixed, parameterless struct: unreachable.
		h.log.Error("failed to encode tools/list_changed notification", "error", err)
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return

		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()

		case <-signals:
			if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", notification); err != nil {
				return
			}
			flusher.Flush()

		case msg := <-sessionMsgs:
			if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg); err != nil {
				return
			}
			flusher.Flush()

		case msg := <-profileMsgs:
			raw, err := json.Marshal(msg)
			if err != nil {
				// A fixed, parameterless notification: unreachable.
				h.log.Error("failed to encode profile list_changed notification", "error", err, "method", msg.Method)
				continue
			}
			if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
