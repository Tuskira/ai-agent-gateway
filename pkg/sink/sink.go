// Package sink defines the gateway's logging contract: the two record types
// written for every request (AccessLog for the MCP plane, LLMCall for the
// LLM plane) and the LogSink interface that persists them.
//
// Sinks never block the request path: WriteAccess and WriteLLMCall must
// return immediately, doing any I/O asynchronously. Concrete sinks (stdout,
// otel, clickhouse) live in sibling packages; this package defines the
// contract and a Multi combinator only.
package sink

import (
	"context"
	"errors"
	"time"
)

// AccessLog is written once per MCP-plane request (tools/list, tools/call,
// initialize, ...). Header and payload capture are opt-in: callers only
// populate Headers/RequestBody/ResponseBody when the deployment's capture
// config asks for them; usage, latency, and identifiers are always recorded.
type AccessLog struct {
	Timestamp time.Time `json:"timestamp"`

	// Identity & correlation.
	TenantID      string `json:"tenant_id"`
	Principal     string `json:"principal"` // Principal.Subject
	KeyID         string `json:"key_id"`
	SessionID     string `json:"session_id"`
	RequestID     string `json:"request_id"`
	CorrelationID string `json:"correlation_id"`
	TraceID       string `json:"trace_id"`
	// ClientSessionID is the caller-supplied X-Session-Id (or
	// X-Claude-Code-Session-Id) header, if any -- NEVER used as SessionID
	// (the gateway-negotiated Mcp-Session-Id) since a caller could then
	// write its calls into another session's timeline. Recorded purely for
	// the Session Timeline ownership rule: an LLM-plane event whose own
	// session_id matches a ClientSessionID seen on this session's MCP
	// events is treated as belonging to it. Max 128 chars, sanitized.
	ClientSessionID string `json:"client_session_id,omitempty"`

	// Source tells apart a row written by the gateway itself proxying a
	// live connector call ("gateway", the default -- every row written
	// before this field existed is implicitly this) from one ingested
	// after the fact from another client's own traffic ("interceptor":
	// POST /api/v1/ingest, fed by a companion capture component --
	// see docs/api.md). Never trusted from a caller-supplied payload
	// field of the same name; the ingest handler stamps it itself.
	Source string `json:"source,omitempty"`
	// User is a self-reported identity label for the caller that produced
	// this row, independent of Principal/KeyID (which identify the
	// gateway API key, not the human or account behind it): today only
	// ingested rows set it, to the Claude account email the interceptor
	// captured. Capped at 256 chars; "" on every gateway-proxied row.
	User string `json:"user,omitempty"`

	// What happened.
	Method      string `json:"method"` // JSON-RPC method: initialize, tools/list, tools/call, ...
	JSONRPCID   string `json:"json_rpc_id"`
	ConnectorID string `json:"connector_id"`
	ToolName    string `json:"tool_name"`
	// SkillName is set when this request loaded a skill or rendered a
	// command from the skills & commands registry: a gateway__skill
	// tools/call (SkillName = the skill's name) or a native command's
	// prompts/get (SkillName = the command's name). Empty for every
	// other request, including a normal connector-routed tools/call.
	SkillName string `json:"skill_name,omitempty"`
	// Profile is the agent profile that scoped this call, from the
	// inbound X-Agent-Profile-Name header (see internal/dataplane/profile).
	// Empty when the caller sent no profile header. Used only for
	// analytics (top-profiles ranking); it is not enforced here.
	Profile    string `json:"profile,omitempty"`
	StatusCode int    `json:"status_code"`
	ErrorCode  string `json:"error_code"` // JSON-RPC error code, e.g. "-32003"
	DurationMS int64  `json:"duration_ms"`
	// BytesIn is the request body size; Bytes is the response body
	// size. They are separate because a tools/call's cost is
	// asymmetric -- a 200-byte query can return a megabyte -- and one
	// combined figure hides which side of a call is expensive.
	BytesIn   int64  `json:"bytes_in"`
	Bytes     int64  `json:"bytes"`
	ClientIP  string `json:"client_ip"`
	UserAgent string `json:"user_agent"`

	// Opt-in capture. Headers are always masked by an allowlist before
	// being set here; bodies are only populated when capture.store_bodies
	// is enabled, and are bounded by the configured max size.
	Headers      map[string]string `json:"headers,omitempty"`
	RequestBody  []byte            `json:"request_body,omitempty"`
	ResponseBody []byte            `json:"response_body,omitempty"`
	Truncated    bool              `json:"truncated,omitempty"`
}

// LLMCall is written once per LLM-plane request (/v1/messages,
// /v1/messages/count_tokens, Bedrock routes). Usage, latency and
// identifiers are always recorded; bodies/messages/system/tools are opt-in,
// mirroring AccessLog's capture policy.
type LLMCall struct {
	Timestamp time.Time `json:"timestamp"`

	// Identity & correlation.
	TenantID  string `json:"tenant_id"`
	Principal string `json:"principal"`
	KeyID     string `json:"key_id"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	ClientIP  string `json:"client_ip,omitempty"`
	// ClientName is UserAgent classified by ClientFamily: "claude-code",
	// "cursor", "vscode", "codex", "claude-desktop", "other", or "" when
	// the caller sent no User-Agent at all. Mirrors the MCP plane's
	// requestsByClient grouping (pkg/sink/clickhouse/reader.go) so the same
	// caller classifies identically on both planes.
	ClientName string `json:"client_name,omitempty"`
	// UserAgent is the raw User-Agent header the LLM-plane caller sent,
	// capped at MaxUserAgentLen bytes (CapUserAgent). "" when absent.
	UserAgent string `json:"user_agent,omitempty"`

	// Source and User mirror AccessLog's fields of the same name (see
	// AccessLog's doc comments): Source tells a gateway-proxied row
	// ("gateway", the default) from an ingested one ("interceptor");
	// User is the ingested row's self-reported caller identity.
	Source string `json:"source,omitempty"`
	User   string `json:"user,omitempty"`

	// Upstream call shape. Provider is the client's dialect ("anthropic" |
	// "bedrock" | "openai" | "gemini"); Model is the vendor model actually
	// called (the registry target's id when the request resolved through
	// the model registry, else the name the client sent).
	Provider     string `json:"provider"`
	UpstreamHost string `json:"upstream_host"`
	Model        string `json:"model"`
	// RequestedModel is the "model" the client sent, verbatim -- always
	// set, registered or not. ResolvedVendor/ResolvedModel are the model
	// registry target that answered ("" when the name was not registered);
	// Translated reports whether the body was translated between wire
	// formats (internal/llmplane/translate); FallbackIndex is the 0-based
	// index of the target that answered (0 for an unregistered call, -1
	// when no target could be tried).
	RequestedModel string `json:"requested_model"`
	ResolvedVendor string `json:"resolved_vendor,omitempty"`
	ResolvedModel  string `json:"resolved_model,omitempty"`
	Translated     bool   `json:"translated"`
	FallbackIndex  int    `json:"fallback_index"`
	Path           string `json:"path"`
	StatusCode     int    `json:"status_code"`
	DurationMS     int64  `json:"duration_ms"`
	Stream         bool   `json:"stream"`

	// Usage, parsed from the (non-stream) JSON body or the SSE
	// message_start/message_delta events (stream).
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	StopReason          string `json:"stop_reason"`
	ProviderRequestID   string `json:"provider_request_id"`

	// CostUSD is the frozen dollar cost of this call, computed from the tokens
	// above and a per-model rate card at write time (see pkg/pricing). nil when
	// the model is not in the rate card (stored as SQL NULL) or the request was
	// not a billable generation (e.g. count_tokens).
	CostUSD *float64 `json:"cost_usd,omitempty"`

	// Opt-in capture, gated by capture.store_bodies.
	Headers      map[string]string `json:"headers,omitempty"`
	RequestBody  []byte            `json:"request_body,omitempty"`
	ResponseBody []byte            `json:"response_body,omitempty"`
	Messages     []byte            `json:"messages,omitempty"` // raw JSON, opt-in
	System       []byte            `json:"system,omitempty"`   // raw JSON, opt-in
	Tools        []byte            `json:"tools,omitempty"`    // raw JSON, opt-in
	Truncated    bool              `json:"truncated,omitempty"`
	// BodyRef locates RequestBody/ResponseBody in a BodyStore when they were
	// offloaded there (both are then nil on the record). Empty = inline.
	BodyRef string `json:"body_ref,omitempty"`

	// SkillsUsed and MCPToolsUsed are the skills and MCP tools the model's
	// RESPONSE asked to use (internal/discovery): lowercased skill names
	// from the "Skill" tool, and "server__tool" values from tools named
	// mcp__<server>__<tool>. Deduplicated per call; names only. They feed
	// the "discovered (used but not registered)" analytics.
	SkillsUsed   []string `json:"skills_used,omitempty"`
	MCPToolsUsed []string `json:"mcp_tools_used,omitempty"`

	// Error, if the call failed before or during the upstream exchange
	// (e.g. "client_closed" on a mid-stream disconnect).
	Error string `json:"error,omitempty"`
}

// LogSink persists AccessLog and LLMCall records. Implementations must not
// block the caller: WriteAccess/WriteLLMCall should enqueue and return.
// Close drains and releases any resources; callers must call it once,
// typically on shutdown.
type LogSink interface {
	WriteAccess(*AccessLog)
	WriteLLMCall(*LLMCall)
	Close() error
}

// BatchSink is the LLM-capture destination. Unlike LogSink (which is
// best-effort and never returns an error), WriteBatch is synchronous and
// returns an error, so the recorder can surface a failed capture instead of
// silently dropping it. Writing to Postgres synchronously (row committed before
// the request completes) is what makes capture lossless; ClickHouse is an
// optional analytics destination.
type BatchSink interface {
	WriteBatch(ctx context.Context, calls []*LLMCall) error
	Close() error
}

// SpendReader reports what has already been spent by one API key, or by
// one tenant through one model name, read back from the durable capture
// store (the LLM plane's budget enforcement uses it). day is the sum of
// cost_usd for the matching calls at or after dayStart, month the sum at or
// after monthStart (monthStart <= dayStart). Calls whose cost is unknown
// (cost_usd NULL) count as 0. Implemented by pkg/sink/postgres.Sink.
type SpendReader interface {
	KeySpend(ctx context.Context, tenantID, keyID string, dayStart, monthStart time.Time) (day, month float64, err error)
	// ModelSpend sums over (tenant_id, requested_model): what the tenant's
	// callers spent asking for that name, whichever target answered.
	ModelSpend(ctx context.Context, tenantID, requestedModel string, dayStart, monthStart time.Time) (day, month float64, err error)
}

// BodyStore offloads captured request/response bodies to external storage
// (a filesystem, or S3-compatible object storage: AWS S3 / R2 / MinIO), so a
// capture row carries a short BodyRef instead of megabytes of payload. The
// capture Recorder calls Put before the durable write when
// llm_proxy.capture.body_store is configured; the API plane calls Get to
// resolve a ref for the console's LLM-log detail. Implementations live in
// pkg/sink/bodystore/*; a nil BodyStore means bodies stay inline.
type BodyStore interface {
	// Put stores the bodies of one call under key (the request id, unique per
	// call) and returns an opaque ref. Either body may be empty.
	Put(ctx context.Context, key string, req, resp []byte) (ref string, err error)
	// Get returns the bodies stored under ref. ErrBodyNotFound when absent.
	Get(ctx context.Context, ref string) (req, resp []byte, err error)
	Close() error
}

// ErrBodyNotFound is returned by BodyStore.Get when ref names no stored bodies.
var ErrBodyNotFound = errors.New("sink: body not found")

// Multi returns a LogSink that fans every write out to all of sinks, in
// order. Close closes every sink and joins their errors (see errors.Join).
func Multi(sinks ...LogSink) LogSink {
	return multiSink(sinks)
}

type multiSink []LogSink

func (m multiSink) WriteAccess(a *AccessLog) {
	for _, s := range m {
		s.WriteAccess(a)
	}
}

func (m multiSink) WriteLLMCall(l *LLMCall) {
	for _, s := range m {
		s.WriteLLMCall(l)
	}
}

func (m multiSink) Close() error {
	var errs []error
	for _, s := range m {
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
