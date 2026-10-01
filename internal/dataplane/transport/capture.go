package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/trace"
)

// Privacy default: payloads are NOT recorded unless capture.store_bodies
// is on, and when they are they are bounded and flagged when cut short.
// Usage, latency and identifiers are always recorded -- that is the audit
// trail, and it needs no payload.

type captureKey struct{}

type captured struct {
	body      []byte
	truncated bool
}

// captureRequest stashes a bounded copy of the request body for the
// access log, when capture is enabled.
func captureRequest(ctx context.Context, body []byte, cfg config.Capture) {
	slot, ok := ctx.Value(captureKey{}).(*captured)
	if !ok {
		return
	}
	slot.body, slot.truncated = sink.TruncateBody(body, cfg.MaxRequestBytes)
}

// capturedRequest returns what captureRequest stashed.
func capturedRequest(ctx context.Context) ([]byte, bool) {
	slot, ok := ctx.Value(captureKey{}).(*captured)
	if !ok {
		return nil, false
	}
	return slot.body, slot.truncated
}

// withCapture attaches the capture slot to a context.
func withCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, captureKey{}, &captured{})
}

// headerAllowList is the set of inbound header names whose values may be
// recorded in an access log.
//
// It is an allow-list, not a deny-list, for the same reason the outbound
// masker is: the inbound header set includes Authorization and
// X-Gateway-Key, and any deployment may add its own credential-bearing
// header that no deny-list anticipated. Recording a name with a masked
// value still shows an operator which headers arrived.
var headerAllowList = map[string]struct{}{
	http.CanonicalHeaderKey("Accept"):               {},
	http.CanonicalHeaderKey("Content-Type"):         {},
	http.CanonicalHeaderKey("Content-Length"):       {},
	http.CanonicalHeaderKey("User-Agent"):           {},
	http.CanonicalHeaderKey("X-Request-Id"):         {},
	http.CanonicalHeaderKey("X-Correlation-Id"):     {},
	http.CanonicalHeaderKey("X-Agent-Profile-Name"): {},
	http.CanonicalHeaderKey(trace.Header):           {},
	http.CanonicalHeaderKey("Mcp-Protocol-Version"): {},
	// Session ids. Mcp-Session-Id is recorded here because session_id may
	// hold the client's own id instead.
	http.CanonicalHeaderKey("Mcp-Session-Id"):           {},
	http.CanonicalHeaderKey("X-Session-Id"):             {},
	http.CanonicalHeaderKey("X-Claude-Code-Session-Id"): {},
	http.CanonicalHeaderKey("X-Forwarded-For"):          {},
	http.CanonicalHeaderKey("Accept-Encoding"):          {},
	http.CanonicalHeaderKey("Connection"):               {},
}

// maskedHeaders renders inbound headers for the access log: names
// always, values only when allow-listed.
func maskedHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for name, values := range h {
		if len(values) == 0 {
			continue
		}
		if _, safe := headerAllowList[http.CanonicalHeaderKey(name)]; safe {
			out[name] = values[0]
			continue
		}
		out[name] = "****"
	}
	return out
}

// newRequestID mints an id for a request that arrived without one.
func newRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// A duplicate id degrades correlation; it must not fail a
		// request. Fall back to the trace id generator, which has its
		// own fallback.
		return trace.GenerateTraceID()
	}
	return hex.EncodeToString(buf[:])
}

// traceFromRequest derives the request's trace context from an inbound
// W3C traceparent, starting a new trace when there is none or it is
// malformed. The boolean reports whether an inbound header was used.
func traceFromRequest(r *http.Request) (trace.Context, bool) {
	return trace.FromHeader(r.Header.Get(trace.Header))
}
