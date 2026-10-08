package metrics

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// httpDurationBuckets are the explicit histogram boundaries (seconds) of
// http_request_duration_seconds. They run to five minutes because the
// streaming planes (SSE, LLM responses) hold a request open that long.
var httpDurationBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300}

// unmatchedRoute is the route label of a request no route claimed.
const unmatchedRoute = "unmatched"

// httpInstruments are the three HTTP instruments. They are created once
// per Metrics (see Metrics.httpInstruments), not per Instrument call, so
// wrapping several planes shares one set of series.
type httpInstruments struct {
	requests metric.Int64Counter
	duration metric.Float64Histogram
	inFlight metric.Int64UpDownCounter
}

func newHTTPInstruments(meter metric.Meter) (*httpInstruments, error) {
	requests, err := meter.Int64Counter("http_requests",
		metric.WithDescription("HTTP requests served, by plane, method, route and status code."))
	if err != nil {
		return nil, err
	}
	duration, err := meter.Float64Histogram("http_request_duration_seconds",
		metric.WithDescription("Time from request start until the handler returned. Streaming responses count their whole lifetime."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(httpDurationBuckets...))
	if err != nil {
		return nil, err
	}
	inFlight, err := meter.Int64UpDownCounter("http_requests_in_flight",
		metric.WithDescription("HTTP requests currently being served, by plane."))
	if err != nil {
		return nil, err
	}
	return &httpInstruments{requests: requests, duration: duration, inFlight: inFlight}, nil
}

// httpState holds the lazily created HTTP instruments inside Metrics.
type httpState struct {
	once sync.Once
	inst *httpInstruments
	err  error
}

// httpInstruments returns the HTTP instruments, creating them on first
// use. A nil *Metrics yields instruments on a no-op meter.
func (m *Metrics) httpInstruments() (*httpInstruments, error) {
	if m == nil {
		return newHTTPInstruments(m.Meter())
	}
	m.http.once.Do(func() { m.http.inst, m.http.err = newHTTPInstruments(m.meter) })
	return m.http.inst, m.http.err
}

// Instrument returns middleware that records http_requests,
// http_request_duration_seconds and http_requests_in_flight for one
// plane ("mcp", "api" or "llm").
//
// route maps a request to a bounded route label (see MCPRoute, LLMRoute,
// APIRoute). It is called AFTER the wrapped handler returns, so a router
// that fills in its match while serving (chi's RoutePattern) is already
// populated; an empty result is recorded as "unmatched". The method label
// is limited to the standard HTTP methods ("OTHER" for anything else) and
// the status label is the HTTP status code the handler wrote.
//
// With a no-op meter (metrics.driver none) the instruments do nothing, so
// callers wrap unconditionally. A nil *Metrics is valid.
func (m *Metrics) Instrument(plane string, route func(*http.Request) string) func(http.Handler) http.Handler {
	inst, err := m.httpInstruments()
	if err != nil {
		if m != nil {
			m.logger.Warn("metrics: http instruments unavailable; plane is not instrumented", "plane", plane, "error", err)
		}
		return func(next http.Handler) http.Handler { return next }
	}
	planeAttr := attribute.String("plane", plane)
	inFlightOpt := metric.WithAttributes(planeAttr)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ctx := r.Context()
			inst.inFlight.Add(ctx, 1, inFlightOpt)

			sw := newStatusWriter(w)
			finished := false
			defer func() {
				inst.inFlight.Add(ctx, -1, inFlightOpt)
				if !finished && !sw.wroteHeader {
					// The handler panicked before writing anything; net/http
					// answers that with a closed connection, which is a 500
					// for the dashboard's purposes.
					sw.status = http.StatusInternalServerError
				}
				recordHTTP(ctx, inst, planeAttr, r, route, sw.statusCode(), time.Since(start))
			}()
			next.ServeHTTP(sw.writer(), r)
			finished = true
		})
	}
}

func recordHTTP(ctx context.Context, inst *httpInstruments, plane attribute.KeyValue, r *http.Request,
	route func(*http.Request) string, status int, d time.Duration) {
	routeLabel := ""
	if route != nil {
		routeLabel = route(r)
	}
	if routeLabel == "" {
		routeLabel = unmatchedRoute
	}
	method := methodLabel(r.Method)
	inst.requests.Add(ctx, 1, metric.WithAttributes(
		plane,
		attribute.String("method", method),
		attribute.String("route", routeLabel),
		attribute.String("status", strconv.Itoa(status)),
	))
	inst.duration.Record(ctx, d.Seconds(), metric.WithAttributes(
		plane,
		attribute.String("method", method),
		attribute.String("route", routeLabel),
	))
}

// methodLabel bounds the method label: net/http accepts any token as a
// method, so a client could otherwise mint unlimited label values.
func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
		http.MethodConnect, http.MethodTrace:
		return m
	}
	return "OTHER"
}

// statusWriter records the status code a handler writes while keeping
// every optional ResponseWriter capability the handler may look for.
//
// It MUST stay an http.Flusher: the gateway type-asserts w.(http.Flusher)
// directly, not through http.ResponseController, in
// internal/dataplane/transport/sse.go (the SSE stream),
// internal/llmplane/capture.go (the streaming tee) and
// internal/llmplane/translate/engine.go (translated streams). A plain
// wrapper without Flush would silently turn every stream into a buffered
// response. Unwrap lets http.ResponseController reach the real writer for
// the deadline and full-duplex controls, and Hijack is exposed only when
// the underlying writer supports it (see writer).
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	hijacked    bool
}

func newStatusWriter(w http.ResponseWriter) *statusWriter {
	return &statusWriter{ResponseWriter: w}
}

// hijackWriter adds Hijack to a statusWriter whose underlying writer has it.
type hijackWriter struct{ *statusWriter }

// writer returns the value to hand to the handler: a plain statusWriter,
// or one that also implements http.Hijacker when the inner writer does,
// so a handler's w.(http.Hijacker) answers the same as without the
// middleware.
func (w *statusWriter) writer() http.ResponseWriter {
	if _, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hijackWriter{w}
	}
	return w
}

// statusCode is the final status as net/http would send it: 200 when
// the handler wrote a body or nothing, and 101 for a hijacked connection
// that never wrote a status.
func (w *statusWriter) statusCode() int {
	switch {
	case w.status != 0:
		return w.status
	case w.hijacked:
		return http.StatusSwitchingProtocols
	default:
		return http.StatusOK
	}
}

func (w *statusWriter) WriteHeader(code int) {
	// Informational 1xx headers (except 101) are not the final status and
	// may be sent repeatedly.
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.ResponseWriter.WriteHeader(code)
	if !w.wroteHeader {
		w.wroteHeader = true
		w.status = code
	}
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush delegates to the underlying writer (through any Unwrap chain). As
// in net/http, flushing before a status was written sends 200.
func (w *statusWriter) Flush() {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.status = http.StatusOK
	}
	// http.ErrNotSupported means the underlying writer cannot flush
	// (a test recorder, say); there is nothing to do then.
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack delegates to the underlying http.Hijacker.
func (w hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, rw, err
}
