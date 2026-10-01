// Package client speaks MCP to a backend server over HTTP.
//
// One Call is one JSON-RPC exchange with one connector. Around that sit
// the four behaviours that make a backend call survivable:
//
//   - header resolution, in a fixed order: the connector's configured
//     headers first, then the gateway-owned X-Tenant-Id and
//     mcp-session-id, so no connector config can spoof either;
//   - protocol-version negotiation: try 2025-06-18, fall back once to
//     2024-11-05 if the backend rejects it, and report which worked so
//     the caller can remember it for the session;
//   - credential recovery: a 401 evicts the connector's cached
//     credentials and replays the request exactly once;
//   - bounded retry: at most three attempts on a 5xx/429/408 or a
//     network error, never on a timeout (the backend is still working,
//     and a retry would double the load that caused the timeout).
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	dpheaders "github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	pkgheaders "github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/trace"
)

const (
	// maxAttempts bounds transport-level retries for one Call.
	maxAttempts = 3
	// initialBackoff and maxBackoff bound the wait between attempts.
	initialBackoff = 200 * time.Millisecond
	maxBackoff     = 2 * time.Second

	// HeaderTenantID is the gateway-owned tenant header. It is written
	// after connector-configured headers, so no connector config can
	// override it.
	HeaderTenantID = "X-Tenant-Id"
	// HeaderConnectorID is echoed back to the caller on a tools/call so
	// an operator can see which backend served it.
	HeaderConnectorID = "X-Connector-ID"
)

// ErrTimeout wraps a backend call that ran out of time. The orchestrator
// turns it into TOOL_TIMEOUT content for a tools/call rather than an
// error, because a model can act on the former and not the latter.
var ErrTimeout = errors.New("client: backend call timed out")

// Options configures a Client.
type Options struct {
	// Headers resolves a connector's metadata.headers entries. Nil
	// means no connector-configured headers are sent at all.
	Headers *dpheaders.Registry
	// DefaultTimeout bounds a call to a connector whose own timeout_ms
	// is unset. Zero uses 30s.
	DefaultTimeout time.Duration
	// TLS configures outbound verification for every connector; a
	// connector's metadata.tls.insecure_skip_verify overrides it for
	// that connector alone.
	TLS config.ConnectorTLS
	// Logger receives the client's own log lines.
	Logger *slog.Logger
	// sleep waits between retry attempts; tests replace it so a retry
	// path does not cost real wall-clock time.
	sleep func(ctx context.Context, d time.Duration) error
}

// Client calls backend MCP servers.
type Client struct {
	headers        *dpheaders.Registry
	defaultTimeout time.Duration
	logger         *slog.Logger
	sleep          func(ctx context.Context, d time.Duration) error

	tlsCfg config.ConnectorTLS

	mu      sync.Mutex
	clients map[bool]*http.Client // keyed by insecureSkipVerify
	rootsMu sync.Mutex
	roots   *x509.CertPool
	rootsOK bool
}

// New returns a Client. It never fails: a CA bundle that cannot be read
// is reported the first time a call needs it, not at construction, so a
// misconfigured bundle takes down the connectors that need it rather than
// the whole gateway.
func New(opts Options) *Client {
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.sleep == nil {
		opts.sleep = sleepCtx
	}

	return &Client{
		headers:        opts.Headers,
		defaultTimeout: opts.DefaultTimeout,
		logger:         opts.Logger,
		sleep:          opts.sleep,
		tlsCfg:         opts.TLS,
		clients:        make(map[bool]*http.Client, 2),
	}
}

// Call is one JSON-RPC exchange with one connector.
type Call struct {
	// Connector is the backend to talk to.
	Connector *store.Connector
	// Request is the JSON-RPC message to send. A Request with no id is
	// a notification: no response is read, and Result.Response is nil.
	Request *mcp.Request
	// SessionID is the backend's own mcp-session-id, if one has been
	// negotiated. Empty on the first call to a connector.
	SessionID string
	// ProtocolVersion is the version previously found to work for this
	// connector. Empty means "negotiate": try the current version and
	// fall back once.
	ProtocolVersion string
	// Inbound is the caller's HTTP request, consulted only by the
	// incoming_field header resolver. May be nil.
	Inbound *http.Request
	// Trace propagates W3C trace context to the backend.
	Trace trace.Context
	// OnNotification, when set, receives every JSON-RPC notification the
	// backend streams ahead of its final response on a text/event-stream
	// reply (notifications/progress, for one). It runs on the goroutine
	// reading the reply, so it must not block; the final response is
	// returned exactly as it is without a callback.
	OnNotification func(mcp.Request)
	// OnRequest answers a request the backend sends inside its streamed
	// reply (sampling/createMessage, elicitation/create, roots/list): a
	// tool that needs the agent mid-call. It runs on its own goroutine,
	// at most four at once per call, while the reply keeps being read,
	// and may block until it has an answer or ctx -- which ends with the
	// call -- is done. It returns exactly one of a result and an error,
	// which is POSTed back to the backend under the request's own id.
	// While it runs the call's per-connector timeout is paused. Nil
	// answers every such request with -32601.
	OnRequest func(ctx context.Context, req mcp.Request) (json.RawMessage, *mcp.Error)

	// ClientCapabilities is read by Initialize only: the client
	// capabilities to declare to the backend, each as the raw JSON
	// object an agent declared it with. Nil declares none.
	ClientCapabilities map[string]json.RawMessage

	// RequestID and Meta are read by CallTool only. RequestID, when set,
	// is the upstream JSON-RPC id to send -- the caller picks it so it
	// can name the request in a later notifications/cancelled. Meta is
	// the caller's params._meta, forwarded verbatim (it carries the
	// progressToken the backend echoes on notifications/progress).
	RequestID any
	Meta      json.RawMessage
}

// Result is the outcome of a Call.
type Result struct {
	// Response is the decoded JSON-RPC response; nil for a
	// notification.
	Response *mcp.Response
	// SessionID is the backend session id after the call: the one the
	// backend returned, or the one that was sent if it returned none.
	SessionID string
	// ProtocolVersion is the version the backend accepted.
	ProtocolVersion string
	// StatusCode is the last HTTP status observed; 0 when the call
	// never reached a response.
	StatusCode int
	// BytesOut and BytesIn are the request and response body sizes.
	BytesOut, BytesIn int64
}

// Do performs one Call with the full retry/recovery policy.
//
// The per-connector timeout is applied here rather than on the
// http.Client so that it covers the whole exchange -- every attempt, plus
// the backoff between them -- instead of resetting per attempt. It is
// paused while the backend waits on the agent (see Call.OnRequest).
func (c *Client) Do(ctx context.Context, call Call) (*Result, error) {
	if call.Connector == nil {
		return nil, fmt.Errorf("client: call has no connector")
	}
	if call.Request == nil {
		return nil, fmt.Errorf("client: call has no request")
	}

	timeout := c.defaultTimeout
	if call.Connector.TimeoutMS > 0 {
		timeout = time.Duration(call.Connector.TimeoutMS) * time.Millisecond
	}
	ctx, _, release := withCallDeadline(ctx, timeout)
	defer release()

	// fail reports err, as ErrTimeout when the call's own deadline is
	// what ended it: that cancellation surfaces from net/http as a plain
	// "context canceled".
	fail := func(result *Result, err error) (*Result, error) {
		if expired(ctx) && !errors.Is(err, ErrTimeout) {
			return result, fmt.Errorf("%w: %s", ErrTimeout, err.Error())
		}
		return result, classifyErr(err)
	}

	var lastErr error
	var lastResult *Result

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, backoff(attempt-1)); err != nil {
				return fail(lastResult, err)
			}
			c.logger.Debug("retrying backend MCP call",
				"connector_id", call.Connector.ID, "attempt", attempt+1, "max_attempts", maxAttempts)
		}

		result, err := c.doWithAuthRecovery(ctx, call)
		if err == nil {
			return result, nil
		}
		lastErr, lastResult = err, result

		status := 0
		if result != nil {
			status = result.StatusCode
		}
		if !retryable(err, status) || expired(ctx) {
			return fail(result, err)
		}
		if attempt+1 >= maxAttempts {
			break
		}
		c.logger.Warn("backend MCP call failed, will retry",
			"connector_id", call.Connector.ID, "attempt", attempt+1, "status", status, "error", err)
	}

	return lastResult, fmt.Errorf("client: %d attempts exhausted: %w", maxAttempts, classifyErr(lastErr))
}

// doWithAuthRecovery runs one attempt, and on a 401 evicts the
// connector's cached credentials and replays the request exactly once.
//
// This is the "token cached here, revoked upstream" case: an identity
// provider's token still looks unexpired to us while the binding behind
// it has been removed. Re-minting after eviction picks the new state up.
// A second 401 is surfaced: the customer side is genuinely broken, and
// thrashing on it helps nobody. The replay is bounded to one attempt and
// is independent of the outer transport retry.
func (c *Client) doWithAuthRecovery(ctx context.Context, call Call) (*Result, error) {
	result, err := c.do(ctx, call)
	if result == nil || result.StatusCode != http.StatusUnauthorized {
		return result, err
	}

	if !c.evictCredentials(ctx, call.Connector) {
		return result, err
	}
	return c.do(ctx, call)
}

// evictCredentials drops the cached credentials behind conn's headers
// after a 401, reporting whether anything was evicted -- and so whether
// replaying the request once is worth it.
func (c *Client) evictCredentials(ctx context.Context, conn *store.Connector) bool {
	invalidators := c.invalidatorsFor(conn)
	if len(invalidators) == 0 {
		return false
	}

	principal, ok := pkgauth.PrincipalFrom(ctx)
	if !ok || principal == nil {
		// Without a principal there is no cache key to evict. Surface
		// the 401 rather than guess at one.
		c.logger.Warn("backend returned 401 but no principal is in context; not replaying",
			"connector_id", conn.ID)
		return false
	}

	for _, ref := range invalidators {
		ref.inv.Invalidate(ctx, principal.Subject, conn.Slug)
		c.logger.Info("evicted cached credentials after 401; replaying once",
			"connector_id", conn.ID, "connector_slug", conn.Slug, "provider", ref.id)
	}
	return true
}

// do performs a single HTTP exchange, including one protocol-version
// downgrade if the backend rejects the version it was offered.
func (c *Client) do(ctx context.Context, call Call) (*Result, error) {
	version := call.ProtocolVersion
	negotiating := version == ""
	if negotiating {
		version = mcp.ProtocolVersion
	}

	result, err := c.exchange(ctx, call, version)

	// A backend that does not know the newer revision answers 400 (bad
	// request) or 406 (not acceptable). Only downgrade when we picked
	// the version ourselves -- if the caller pinned one, a 400 is a real
	// error about the payload, not about the version.
	if negotiating && result != nil &&
		(result.StatusCode == http.StatusBadRequest || result.StatusCode == http.StatusNotAcceptable) {
		c.logger.Info("backend rejected MCP protocol version, retrying with the legacy revision",
			"connector_id", call.Connector.ID, "rejected", version, "fallback", mcp.ProtocolVersionLegacy,
			"status", result.StatusCode)
		return c.exchange(ctx, call, mcp.ProtocolVersionLegacy)
	}

	return result, err
}

func (c *Client) exchange(ctx context.Context, call Call, version string) (*Result, error) {
	body, err := json.Marshal(call.Request)
	if err != nil {
		return nil, fmt.Errorf("client: marshal request: %w", err)
	}

	// An MCP server may answer either a plain JSON body or a one-shot
	// SSE stream; accept both and decode whichever comes back.
	httpReq, err := c.newRequest(ctx, http.MethodPost, call, version, "application/json, text/event-stream", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	c.logger.Debug("sending backend MCP request",
		"connector_id", call.Connector.ID, "endpoint", call.Connector.Endpoint,
		"method", call.Request.Method, "protocol_version", version,
		"headers", maskHeaders(httpReq.Header))

	httpClient, err := c.httpClient(call.Connector)
	if err != nil {
		return nil, err
	}

	result := &Result{SessionID: call.SessionID, ProtocolVersion: version, BytesOut: int64(len(body))}

	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return result, fmt.Errorf("client: send to %s: %w", call.Connector.Endpoint, err)
	}
	defer httpResp.Body.Close()

	result.StatusCode = httpResp.StatusCode
	if id := httpResp.Header.Get(mcp.HeaderSessionID); id != "" {
		result.SessionID = id
	}

	// A streamed reply is read frame by frame rather than slurped, so a
	// notification the backend sends ahead of its response (progress on
	// a long tool call) reaches OnNotification while the call is still
	// running, and a cancelled context stops the read mid-stream.
	if httpResp.StatusCode == http.StatusOK && !call.Request.IsNotification() &&
		isEventStream(httpResp.Header.Get("Content-Type")) {
		requests := c.newRequestDispatcher(ctx, call, result.SessionID, version)
		resp, n, err := readSSE(httpResp.Body, call.OnNotification, requests.dispatch)
		requests.close()
		result.BytesIn = n
		if err != nil {
			return result, err
		}
		result.Response = resp
		return result, nil
	}

	respBody, err := io.ReadAll(httpResp.Body)
	result.BytesIn = int64(len(respBody))
	if err != nil {
		return result, fmt.Errorf("client: read response: %w", err)
	}

	// A notification gets no response body worth decoding; servers
	// answer 200, 202 or 204 interchangeably.
	if call.Request.IsNotification() {
		switch httpResp.StatusCode {
		case http.StatusOK, http.StatusAccepted, http.StatusNoContent:
			return result, nil
		default:
			return result, fmt.Errorf("client: notification %q returned status %d: %s",
				call.Request.Method, httpResp.StatusCode, snippet(respBody))
		}
	}

	if httpResp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("client: backend returned status %d: %s", httpResp.StatusCode, snippet(respBody))
	}

	resp, err := decodeResponse(httpResp.Header.Get("Content-Type"), respBody)
	if err != nil {
		return result, err
	}
	result.Response = resp

	return result, nil
}

// newRequest builds an outbound request to call.Connector's endpoint
// with every header the gateway sends a backend, in the one fixed order:
// protocol version and trace context, then the connector's configured
// headers, then the gateway-owned X-Tenant-Id and Mcp-Session-Id last,
// so no connector config can substitute its own tenant or hijack the
// session. Every path to a backend -- a call, a reply to a
// server-to-client request, the long-lived stream -- goes through it.
func (c *Client) newRequest(ctx context.Context, method string, call Call, version, accept string, body io.Reader) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, method, call.Connector.Endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("client: build request: %w", err)
	}

	httpReq.Header.Set("Accept", accept)
	httpReq.Header.Set(mcp.HeaderProtocolVersion, version)
	if tp := call.Trace.Traceparent(); tp != "" {
		httpReq.Header.Set(trace.Header, tp)
	}

	if err := c.applyConnectorHeaders(ctx, httpReq, call); err != nil {
		return nil, err
	}

	if principal, ok := pkgauth.PrincipalFrom(ctx); ok && principal != nil && principal.TenantID != "" {
		httpReq.Header.Set(HeaderTenantID, principal.TenantID)
	}
	if call.SessionID != "" {
		httpReq.Header.Set(mcp.HeaderSessionID, call.SessionID)
	}
	return httpReq, nil
}

// applyConnectorHeaders resolves the connector's metadata.headers map and
// writes each resolved value onto the outbound request.
//
// Resolution is best-effort per header: one header that cannot be
// resolved (an expired credential, a provider that is down) is logged and
// skipped rather than failing the whole call, so a connector with one
// optional enrichment header keeps working. The backend decides whether
// the missing header matters, by answering 401.
//
// The one exception is a connector that still carries a disabled
// bearer_token forwarding header (connectors.allow_bearer_token_forwarding
// is off): the call fails with a clear error. Skipping the header would
// make the connector fail with a confusing backend 401 instead.
func (c *Client) applyConnectorHeaders(ctx context.Context, httpReq *http.Request, call Call) error {
	if c.headers == nil {
		return nil
	}
	configs := headerConfigs(call.Connector)
	if len(configs) == 0 {
		return nil
	}

	for name, cfg := range configs {
		value, err := c.headers.Resolve(ctx, name, cfg, call.Inbound)
		if errors.Is(err, dpheaders.ErrBearerForwardingDisabled) {
			return fmt.Errorf("client: connector %q: %w", call.Connector.Name, err)
		}
		if err != nil {
			c.logger.Warn("failed to resolve connector header; skipping it",
				"connector_id", call.Connector.ID, "header", name, "error", err)
			continue
		}
		httpReq.Header.Set(name, value)
	}
	return nil
}

// headerConfigs extracts connector.metadata.headers as a map of per-header
// resolver configs, tolerating the shapes JSONB decoding can produce.
func headerConfigs(c *store.Connector) map[string]map[string]any {
	raw, ok := c.Metadata["headers"].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]map[string]any, len(raw))
	for name, v := range raw {
		cfg, ok := v.(map[string]any)
		if !ok {
			continue
		}
		out[name] = cfg
	}
	return out
}

// invalidatorRef pairs a provider id with its Invalidator so an eviction
// can be logged against the provider that owns the cache.
type invalidatorRef struct {
	id  string
	inv pkgheaders.Invalidator
}

// invalidatorsFor returns one ref per distinct provider backing the
// connector's headers that can evict a cached credential. Two headers
// backed by the same provider yield one ref, not two: they share a cache
// key, and evicting it twice does nothing the first eviction did not.
func (c *Client) invalidatorsFor(conn *store.Connector) []invalidatorRef {
	if c.headers == nil || conn == nil {
		return nil
	}

	var refs []invalidatorRef
	seen := make(map[string]bool)
	for _, cfg := range headerConfigs(conn) {
		if typ, _ := cfg["type"].(string); typ != "external" {
			continue
		}
		id, _ := cfg["provider"].(string)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		provider, ok := c.headers.Provider(id)
		if !ok {
			continue
		}
		inv, ok := provider.(pkgheaders.Invalidator)
		if !ok {
			continue
		}
		refs = append(refs, invalidatorRef{id: id, inv: inv})
	}
	return refs
}

// httpClient returns the shared http.Client for the connector's TLS
// posture. Two are enough: verifying and non-verifying. They are built
// lazily and reused so connection pooling actually pools.
func (c *Client) httpClient(conn *store.Connector) (*http.Client, error) {
	insecure := c.tlsCfg.InsecureSkipVerify || connectorSkipsVerify(conn)

	c.mu.Lock()
	if hc, ok := c.clients[insecure]; ok {
		c.mu.Unlock()
		return hc, nil
	}
	c.mu.Unlock()

	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure} //nolint:gosec // opt-in per connector, default false
	if !insecure {
		roots, err := c.rootPool()
		if err != nil {
			return nil, err
		}
		tlsConf.RootCAs = roots
	}

	hc := &http.Client{
		// No client-level timeout: Do applies the per-connector
		// deadline to the context, which covers retries too.
		// Every dial goes through the SSRF guard: the resolved IP is checked
		// (so redirects and DNS tricks cannot reach internal addresses) and
		// redirect chains are capped.
		CheckRedirect: netguard.LimitRedirects,
		Transport: &http.Transport{
			DialContext:         netguard.DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			TLSClientConfig:     tlsConf,
		},
	}

	c.mu.Lock()
	if existing, ok := c.clients[insecure]; ok {
		c.mu.Unlock()
		return existing, nil
	}
	c.clients[insecure] = hc
	c.mu.Unlock()
	return hc, nil
}

// rootPool returns the verification roots: the system pool, plus the
// configured CA bundle when there is one. Adding to the system pool
// rather than replacing it means configuring a private CA for one
// connector does not quietly stop every public endpoint from verifying.
func (c *Client) rootPool() (*x509.CertPool, error) {
	if c.tlsCfg.CABundle == "" {
		return nil, nil // nil RootCAs means "use the system pool"
	}

	c.rootsMu.Lock()
	defer c.rootsMu.Unlock()
	if c.rootsOK {
		return c.roots, nil
	}

	pem, err := os.ReadFile(c.tlsCfg.CABundle)
	if err != nil {
		return nil, fmt.Errorf("client: read connectors.tls.ca_bundle %q: %w", c.tlsCfg.CABundle, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("client: connectors.tls.ca_bundle %q contains no usable certificate", c.tlsCfg.CABundle)
	}

	c.roots, c.rootsOK = pool, true
	return pool, nil
}

// connectorSkipsVerify reads metadata.tls.insecure_skip_verify.
func connectorSkipsVerify(conn *store.Connector) bool {
	if conn == nil {
		return false
	}
	tlsMeta, ok := conn.Metadata["tls"].(map[string]any)
	if !ok {
		return false
	}
	skip, _ := tlsMeta["insecure_skip_verify"].(bool)
	return skip
}

// Close releases pooled connections.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, hc := range c.clients {
		hc.CloseIdleConnections()
	}
	return nil
}

// retryable decides whether a failed attempt is worth repeating.
//
// A timeout never is: the backend is still working on the original
// request, so a retry adds load to a server that is already too slow and
// risks executing a side-effecting tool twice. A cancelled context never
// is either -- the caller has gone away.
func retryable(err error, status int) bool {
	if err == nil {
		return false
	}
	if isTimeout(err) || errors.Is(err, context.Canceled) {
		return false
	}
	switch {
	case status == 0:
		// No response at all: a connection-level failure, which a
		// retry against a second backend instance may well survive.
		return true
	case status >= 500 && status < 600:
		return true
	case status == http.StatusTooManyRequests, status == http.StatusRequestTimeout:
		return true
	default:
		return false
	}
}

// isTimeout reports whether err is a genuine deadline expiry. String
// matching backs up errors.Is because net/url wraps some errors without
// %w, losing the sentinel.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimeout) || errors.Is(err, errCallDeadline) {
		return true
	}
	if netErr, ok := err.(interface{ Timeout() bool }); ok && netErr.Timeout() {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "TLS handshake timeout")
}

// classifyErr normalizes a deadline expiry onto ErrTimeout so callers can
// branch on it with errors.Is without re-doing the string matching.
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	if isTimeout(err) && !errors.Is(err, ErrTimeout) {
		return fmt.Errorf("%w: %s", ErrTimeout, err.Error())
	}
	return err
}

// IsTimeout reports whether err came from a backend call that ran out of
// time, as opposed to one that was cancelled or failed outright.
func IsTimeout(err error) bool { return errors.Is(err, ErrTimeout) }

// backoff returns the wait before attempt n+1: exponential from
// initialBackoff, capped at maxBackoff, with +/-25% jitter so a fleet of
// gateways does not retry in lockstep against a recovering backend.
func backoff(attempt int) time.Duration {
	d := float64(initialBackoff) * math.Pow(2, float64(attempt))
	if d > float64(maxBackoff) {
		d = float64(maxBackoff)
	}
	jitter := 0.75 + 0.5*rand.Float64() //nolint:gosec // jitter, not a secret
	return time.Duration(d * jitter)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// decodeResponse parses a backend reply, which may be a plain JSON body
// or (for a reply exchange did not stream, such as one read whole) a
// single-message SSE stream.
func decodeResponse(contentType string, body []byte) (*mcp.Response, error) {
	if isEventStream(contentType) {
		resp, _, err := readSSE(bytes.NewReader(body), nil, nil)
		if err != nil {
			return nil, fmt.Errorf("%w (body: %s)", err, snippet(body))
		}
		return resp, nil
	}

	var resp mcp.Response
	if err := json.Unmarshal(bytes.TrimSpace(body), &resp); err != nil {
		return nil, fmt.Errorf("client: decode response: %w (body: %s)", err, snippet(body))
	}
	return &resp, nil
}

func snippet(b []byte) string {
	const limit = 500
	if len(b) > limit {
		return string(b[:limit]) + "..."
	}
	return string(b)
}

// logSafeHeaders is the allow-list of outbound header names whose values
// may appear in the clear in a debug log.
//
// It is deny-by-default on purpose. Connector header configs are
// operator-authored and any resolver can mint a header of ANY name
// carrying a live credential, so a deny-list of "names that look secret"
// can only ever be incomplete -- an AWS-backed connector, for instance,
// sends X-AWS-Access-Key-Id, which no such list would have caught. Only
// add a name here if its value can never carry a secret for ANY
// connector.
//
// Mcp-Session-Id is deliberately absent: the spec makes session ids
// cryptographically secure precisely because they are replayable handles,
// and we cannot promise no backend treats one as authenticated state.
var logSafeHeaders = map[string]struct{}{
	http.CanonicalHeaderKey("Accept"):                  {},
	http.CanonicalHeaderKey("Content-Type"):            {},
	http.CanonicalHeaderKey("Content-Length"):          {},
	http.CanonicalHeaderKey(mcp.HeaderProtocolVersion): {},
	http.CanonicalHeaderKey("User-Agent"):              {},
	http.CanonicalHeaderKey(trace.Header):              {},
	http.CanonicalHeaderKey("Tracestate"):              {},
	http.CanonicalHeaderKey(HeaderTenantID):            {},
	http.CanonicalHeaderKey("X-Agent-Profile-Name"):    {},
	http.CanonicalHeaderKey("X-Correlation-Id"):        {},
}

// maskHeaders renders an outbound header set for a debug log: names
// always survive (knowing which headers were sent is the whole debugging
// signal), values only if the name is allow-listed.
func maskHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for name, values := range h {
		if len(values) == 0 {
			continue
		}
		if _, safe := logSafeHeaders[http.CanonicalHeaderKey(name)]; safe {
			out[name] = values[0]
			continue
		}
		out[name] = maskValue(values[0])
	}
	return out
}

// maskValue renders a sensitive value as a short digest prefix. No
// plaintext survives -- not even a prefix, because the header set can
// include shared secrets whose issuers promise never to echo "even a
// length or a prefix" -- while an operator can still tell two values
// apart and see when one changed between requests. The digest is unkeyed:
// it is a correlation handle inside our own logs, not an authentication
// tag.
func maskValue(v string) string {
	if v == "" {
		return "****"
	}
	sum := sha256.Sum256([]byte(v))
	return "sha256:" + hex.EncodeToString(sum[:])[:8] + "****"
}
