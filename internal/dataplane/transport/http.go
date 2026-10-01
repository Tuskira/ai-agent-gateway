// Package transport is the MCP plane's HTTP surface: POST /mcp for
// JSON-RPC, GET /mcp/stream for server-pushed notifications, and an open
// /health probe.
//
// One rule governs the status codes: an authentication failure is an
// HTTP 401 (there is no JSON-RPC layer yet when a credential is refused),
// and everything else is HTTP 200 carrying a JSON-RPC error object.
// Clients parse the body, not the status, and a JSON-RPC error is the
// only shape that reaches them intact.
package transport

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/orchestrator"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/reqctx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// maxRequestBytes bounds an inbound JSON-RPC body. A tools/call's
// arguments are text a model wrote; nothing legitimate approaches this,
// and without a bound a single request can exhaust the process.
const maxRequestBytes = 10 << 20 // 10 MiB

// Deps are the handler's collaborators.
type Deps struct {
	Orchestrator  *orchestrator.Orchestrator
	Sessions      *session.Manager
	Authenticator pkgauth.Authenticator
	// Authorizer gates the plane on the mcp.access permission. nil = allow
	// (tests); production always sets it.
	Authorizer  pkgauth.Authorizer
	Sink        sink.LogSink
	Hub         *Hub
	Capture     config.Capture
	Logger      *slog.Logger
	ServiceName string
	Version     string
	// ClientIP derives the caller's address (the process-wide
	// clientip.Resolver). Nil falls back to the TCP peer address; the
	// X-Forwarded-For header is never trusted on its own.
	ClientIP func(*http.Request) string
}

// Handler serves the MCP plane.
type Handler struct {
	deps Deps
	log  *slog.Logger
}

// NewHandler returns the plane's http.Handler: /health open, everything
// else authenticated.
func NewHandler(deps Deps) http.Handler {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Hub == nil {
		deps.Hub = NewHub()
	}
	h := &Handler{deps: deps, log: deps.Logger}

	auth := AuthMiddleware(deps.Authenticator, deps.Authorizer, deps.Logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.health)
	mux.Handle("POST /mcp", auth(http.HandlerFunc(h.rpc)))
	mux.Handle("DELETE /mcp", auth(http.HandlerFunc(h.deleteSession)))
	mux.Handle("GET /mcp/stream", auth(http.HandlerFunc(h.stream)))
	mux.Handle("/", auth(http.NotFoundHandler()))

	// The access log wraps authentication so a rejected request is
	// still recorded: a burst of 401s is exactly the thing an operator
	// needs the log for.
	return h.withAccessLog(mux)
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"plane":   "mcp",
		"service": h.deps.ServiceName,
		"version": h.deps.Version,
	})
}

// rpc serves POST /mcp.
func (h *Handler) rpc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	info := reqctx.From(ctx)

	principal, ok := pkgauth.PrincipalFrom(ctx)
	if !ok || principal == nil {
		// Unreachable behind AuthMiddleware; refuse rather than run a
		// request with no tenant to scope it to.
		writeUnauthorized(w)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		h.writeResponse(w, info, nil, mcp.NewErrorResponse(nil, mcp.NewParseError("failed to read request body: "+err.Error())))
		return
	}
	info.SetBytes(int64(len(body)), 0)
	if h.deps.Capture.StoreBodies {
		captureRequest(ctx, body, h.deps.Capture)
	}

	if isResponse(body) {
		h.clientResponse(w, r, principal, body)
		return
	}

	var req mcp.Request
	if err := json.Unmarshal(body, &req); err != nil {
		h.writeResponse(w, info, nil, mcp.NewErrorResponse(nil, mcp.NewParseError("invalid JSON: "+err.Error())))
		return
	}
	if rpcErr := req.Validate(); rpcErr != nil {
		h.writeResponse(w, info, nil, mcp.NewErrorResponse(req.ID, rpcErr))
		return
	}

	info.SetMethod(req.Method)
	info.SetJSONRPCID(mcp.FormatID(req.ID))

	// Session resolution. An id that is present but unknown or expired
	// is rejected even for methods that would otherwise work without a
	// session: a client that thinks it holds a session must be told the
	// session is gone, not silently served statelessly and left
	// believing its backend handles survived.
	var sess *session.Session
	backends := h.deps.Sessions.Anonymous()
	if id := r.Header.Get(mcp.HeaderSessionID); id != "" {
		resolved, err := h.deps.Sessions.Resolve(ctx, id, principal.TenantID)
		if err != nil {
			if !errors.Is(err, session.ErrNotFound) {
				h.log.Error("failed to resolve session", "error", err)
			}
			h.writeResponse(w, info, nil, mcp.NewErrorResponse(req.ID,
				mcp.NewError(mcp.ErrorCodeSessionNotFound, "session not found", nil)))
			return
		}
		sess = resolved
		backends = resolved
		info.SetSession(resolved.ID)
	}

	result := h.deps.Orchestrator.Handle(ctx, orchestrator.Request{
		JSONRPC:     &req,
		Principal:   principal,
		Session:     sess,
		Backends:    backends,
		ProfileName: strings.TrimSpace(r.Header.Get(profile.Header)),
		Inbound:     r,
		Trace:       info.Trace,
	})

	// A notification gets no body at all, per JSON-RPC.
	if result.Response == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if result.NewSession != nil {
		w.Header().Set(mcp.HeaderSessionID, result.NewSession.ID)
		info.SetSession(result.NewSession.ID)
	}
	if result.ConnectorID != "" {
		w.Header().Set(client.HeaderConnectorID, result.ConnectorID)
	}
	h.writeResponse(w, info, result.NewSession, result.Response)
}

// isResponse reports whether body is a JSON-RPC response -- an id and a
// result or an error, and no method -- rather than a request or a
// notification.
func isResponse(body []byte) bool {
	var probe map[string]json.RawMessage
	if json.Unmarshal(body, &probe) != nil {
		return false
	}
	_, hasMethod := probe["method"]
	_, hasID := probe["id"]
	_, hasResult := probe["result"]
	_, hasError := probe["error"]
	return !hasMethod && hasID && (hasResult || hasError)
}

// clientResponse serves a POST /mcp carrying a JSON-RPC response: the
// agent's answer to a request the gateway relayed to it from a connector
// (sampling, elicitation, roots). It is acknowledged with 202 and no
// body, as the spec has a server do; whether it matched anything is the
// orchestrator's business, and an answer to an unknown or expired id is
// dropped there.
//
// The session is resolved exactly as for a request, and the answer is
// matched only against requests relayed to THAT session: a response can
// never settle another session's (or tenant's) request.
func (h *Handler) clientResponse(w http.ResponseWriter, r *http.Request, principal *pkgauth.Principal, body []byte) {
	ctx := r.Context()
	info := reqctx.From(ctx)
	info.SetMethod("response")

	var resp mcp.Response
	if err := json.Unmarshal(body, &resp); err != nil || resp.JSONRPC != mcp.Version || (resp.Result == nil) == (resp.Error == nil) {
		h.writeResponse(w, info, nil, mcp.NewErrorResponse(nil,
			mcp.NewInvalidRequestError(`a response needs "jsonrpc":"2.0", an id, and exactly one of "result" and "error"`)))
		return
	}
	info.SetJSONRPCID(mcp.FormatID(resp.ID))

	id := r.Header.Get(mcp.HeaderSessionID)
	if id == "" {
		h.writeResponse(w, info, nil, mcp.NewErrorResponse(nil,
			mcp.NewInvalidRequestError("a response must be sent on the session its request arrived on: send the "+mcp.HeaderSessionID+" header")))
		return
	}
	sess, err := h.deps.Sessions.Resolve(ctx, id, principal.TenantID)
	if err != nil {
		if !errors.Is(err, session.ErrNotFound) {
			h.log.Error("failed to resolve session", "error", err)
		}
		h.writeResponse(w, info, nil, mcp.NewErrorResponse(nil,
			mcp.NewError(mcp.ErrorCodeSessionNotFound, "session not found", nil)))
		return
	}
	info.SetSession(sess.ID)

	h.deps.Orchestrator.HandleClientResponse(ctx, principal.TenantID, sess.ID, body)
	w.WriteHeader(http.StatusAccepted)
}

// deleteSession serves DELETE /mcp, the spec's way for a client to end a
// session it is finished with.
func (h *Handler) deleteSession(w http.ResponseWriter, r *http.Request) {
	principal, ok := pkgauth.PrincipalFrom(r.Context())
	if !ok || principal == nil {
		writeUnauthorized(w)
		return
	}

	id := r.Header.Get(mcp.HeaderSessionID)
	if id == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Resolve first, so one tenant cannot delete another's session by
	// guessing (or replaying) its id.
	if _, err := h.deps.Sessions.Resolve(r.Context(), id, principal.TenantID); err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.deps.Sessions.Delete(r.Context(), id); err != nil {
		h.log.Warn("failed to delete session", "error", err)
	}
	// Its subscriptions and connector streams on this replica end with
	// it; another replica's are swept once it sees the session gone.
	h.deps.Orchestrator.EndSession(principal.TenantID, id)
	w.WriteHeader(http.StatusNoContent)
}

// writeResponse encodes a JSON-RPC response over HTTP 200 and records
// what was sent for the access log.
func (h *Handler) writeResponse(w http.ResponseWriter, info *reqctx.Info, _ *session.Session, resp *mcp.Response) {
	if resp.Error != nil {
		info.SetErrorCode(resp.Error.Code)
	}

	body, err := json.Marshal(resp)
	if err != nil {
		// Marshaling a Response cannot fail in practice (every member
		// is already-valid JSON), but a half-written body is worse
		// than a terse one, so fall back rather than stream garbage.
		h.log.Error("failed to encode JSON-RPC response", "error", err)
		body = []byte(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"failed to encode response"}}`)
	}

	snap := info.Snapshot()
	info.SetBytes(snap.BytesIn, int64(len(body)))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		h.log.Debug("failed to write response body", "error", err)
	}
}

// ---------------------------------------------------------------------------
// access logging
// ---------------------------------------------------------------------------

// withAccessLog stamps a request id, correlation id and trace context
// onto every request, and writes one sink.AccessLog record when it ends.
//
// The record is built from the reqctx.Info the pipeline filled in, not
// from a process-global map keyed by correlation id, so it is correct
// under concurrency and cannot leak when a handler returns early.
func (h *Handler) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		requestID := r.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = newRequestID()
		}
		correlationID := r.Header.Get("X-Correlation-Id")
		if correlationID == "" {
			correlationID = requestID
		}
		tc, _ := traceFromRequest(r)

		info := reqctx.New(requestID, correlationID, tc, h.clientIP(r), r.UserAgent(), start)
		ctx := reqctx.With(r.Context(), info)
		if h.deps.Capture.StoreBodies {
			ctx = withCapture(ctx)
		}

		// Echo the correlation handles so a caller can line its own
		// logs up with ours without having to generate them first.
		w.Header().Set("X-Request-Id", requestID)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK,
			capture: h.deps.Capture.StoreBodies, limit: h.deps.Capture.MaxResponseBytes}
		next.ServeHTTP(rec, r.WithContext(ctx))

		if h.deps.Sink == nil {
			return
		}

		snap := info.Snapshot()
		// SessionID is ALWAYS the gateway-negotiated Mcp-Session-Id
		// (snap.SessionID) -- never a client-supplied header. A caller-sent
		// X-Session-Id/X-Claude-Code-Session-Id is recorded separately as
		// ClientSessionID: letting it override SessionID here would let any
		// key write its calls into another session's timeline (see the
		// Session Timeline ownership rule in docs/observability.md).
		record := &sink.AccessLog{
			Timestamp:       start,
			RequestID:       requestID,
			CorrelationID:   correlationID,
			TraceID:         tc.TraceID,
			SessionID:       snap.SessionID,
			ClientSessionID: sanitizeSessionTag(firstNonEmptyHeader(r, "X-Session-Id", "X-Claude-Code-Session-Id")),
			Method:          snap.Method,
			JSONRPCID:       snap.JSONRPCID,
			ConnectorID:     snap.ConnectorID,
			ToolName:        snap.ToolName,
			SkillName:       snap.SkillName,
			Profile:         firstNonEmpty(snap.Profile, strings.TrimSpace(r.Header.Get(profile.Header))),
			StatusCode:      rec.status,
			DurationMS:      time.Since(start).Milliseconds(),
			BytesIn:         snap.BytesIn,
			Bytes:           snap.BytesOut,
			ClientIP:        info.ClientIP,
			UserAgent:       info.UserAgent,
		}
		if snap.ErrorCode != 0 {
			record.ErrorCode = strconv.Itoa(snap.ErrorCode)
		}
		if principal := info.Principal(); principal != nil {
			record.TenantID = principal.TenantID
			record.Principal = principal.Subject
			record.KeyID = principal.KeyID
		}
		if h.deps.Capture.StoreBodies {
			record.Headers = maskedHeaders(r.Header)
			if body, truncated := capturedRequest(ctx); body != nil {
				record.RequestBody = body
				record.Truncated = record.Truncated || truncated
			}
			record.ResponseBody = rec.body
			record.Truncated = record.Truncated || rec.truncated
		}

		// WriteAccess is contractually non-blocking (it enqueues), so
		// this stays on the request goroutine rather than spawning one
		// per request.
		h.deps.Sink.WriteAccess(record)
	})
}

// statusRecorder remembers the status code written, which the
// ResponseWriter itself does not expose, and, when capture is on, a
// bounded copy of the body (every response path writes through it).
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	capture     bool
	limit       int // <= 0 = unbounded, as for the request body
	body        []byte
	truncated   bool
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.capture {
		keep := p
		if room := s.limit - len(s.body); s.limit > 0 && room < len(p) {
			keep = p[:max(room, 0)]
			s.truncated = true
		}
		s.body = append(s.body, keep...)
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so SSE streaming still works
// through the recorder.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// clientIP returns the caller's address through the configured resolver
// (trusted-proxy aware); without one, the TCP peer.
func (h *Handler) clientIP(r *http.Request) string {
	if h.deps.ClientIP != nil {
		return h.deps.ClientIP(r)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// maxSessionTagLen bounds a caller-supplied session tag (ClientSessionID):
// long enough for any real session id, short enough that a hostile header
// can't bloat a log row.
const maxSessionTagLen = 128

// firstNonEmptyHeader returns the first of the named headers with a
// non-empty (post-trim) value, else "".
func firstNonEmptyHeader(r *http.Request, names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

// sanitizeSessionTag makes a caller-supplied header value safe to store and
// display: invalid UTF-8 is replaced, control characters (which would break
// log/terminal rendering) are stripped, and the result is capped at
// maxSessionTagLen runes.
func sanitizeSessionTag(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	b.Grow(len(s))
	count := 0
	for _, r := range s {
		if count >= maxSessionTagLen {
			break
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// firstNonEmpty returns a if it is non-empty, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
