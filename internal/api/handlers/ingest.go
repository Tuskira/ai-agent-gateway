package handlers

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/discovery"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// Ingest implements POST /api/v1/ingest: the gateway's receiving end for
// externally captured traffic, fed by a companion capture component (see
// docs/api.md, "Ingest"). The gateway never forwards this traffic to any provider; it only stores the
// records into the SAME llm_calls/mcp_access_logs tables a normal
// gateway-proxied call writes, tagged source="interceptor" (see
// sink.AccessLog/LLMCall.Source), so it shows up on the existing console
// pages with no new page.
//
// Every route is mounted behind the ordinary auth + permission middleware
// (see internal/api/router.go: Permission: "ingest.write", which only the
// built-in "interceptor" role and admin's blanket "*" grant -- pkg/auth.
// NewRoleAuthorizer). Ingest.Ingest additionally checks Deps.IngestConfig.
// Enabled itself, AFTER that middleware has already run: an unauthenticated
// or wrong-role caller still gets the usual 401/403 regardless of whether
// ingest is enabled, so the "must be indistinguishable from an unknown
// route" requirement (the design doc's item in section 4.5) is scoped to a
// caller who already holds a valid interceptor-role key -- for that caller,
// a disabled deployment answers with the EXACT body an unmounted route
// would (writeIngestDisabled), so it learns nothing about whether the
// feature exists on this deployment versus simply not being rolled out.
// Bypassing auth entirely to hide the route from anonymous probes too would
// require a much larger change to how this router authenticates (every
// other route assumes Middleware always runs first); this is the narrower,
// documented reading of that requirement.
type Ingest struct{ Deps }

// Fixed limits from the ingest API contract (v1). maxIngestCompressedBytes
// is intentionally NOT configurable (unlike Deps.IngestConfig.MaxBodyBytes,
// the decompressed cap): it bounds the wire size regardless of how large an
// operator sets max_body_bytes, so a misconfigured huge decompressed cap
// still can't turn this route into an unbounded decompression amplifier.
const (
	maxIngestCompressedBytes  = 8 << 20 // 8 MiB
	maxIngestRequestIDLen     = 128
	maxIngestUserLen          = 256
	defaultIngestMaxBodyBytes = 32 << 20 // 32 MiB; mirrors config.Default()'s Ingest.MaxBodyBytes
	defaultIngestMaxRecords   = 1000     // mirrors config.Default()'s Ingest.MaxRecords
)

// sourceInterceptor is the sink.AccessLog/LLMCall.Source value stamped on
// every record this handler accepts -- never "gateway" (that value is
// implicit/default for a normally-proxied call, and is never written by
// this handler).
const sourceInterceptor = "interceptor"

// ingestRequest is the wire shape of POST /api/v1/ingest's body (schema_version
// 1). Deliberately decoded WITHOUT DisallowUnknownFields (see Ingest.Ingest):
// the contract says any tenant_id/key_id/principal-shaped field a caller
// includes is ignored, not rejected, and llmCallIn/accessLogIn simply have no
// such fields to unmarshal them into.
type ingestRequest struct {
	SchemaVersion int           `json:"schema_version"`
	User          string        `json:"user,omitempty"`
	LLMCalls      []llmCallIn   `json:"llm_calls,omitempty"`
	AccessLogs    []accessLogIn `json:"access_logs,omitempty"`
}

// llmCallIn is one entry of ingestRequest.LLMCalls. Required: Timestamp,
// RequestID, StatusCode (see buildLLMCall's validation). Every other field
// is optional and, if empty/zero, simply leaves the corresponding
// sink.LLMCall field at its zero value -- the contract in docs/api.md,
// "Ingest".
type llmCallIn struct {
	Timestamp           string `json:"timestamp"`
	RequestID           string `json:"request_id"`
	StatusCode          int    `json:"status_code"`
	SessionID           string `json:"session_id,omitempty"`
	ClientIP            string `json:"client_ip,omitempty"`
	ClientName          string `json:"client_name,omitempty"`
	UserAgent           string `json:"user_agent,omitempty"`
	Provider            string `json:"provider,omitempty"`
	UpstreamHost        string `json:"upstream_host,omitempty"`
	Model               string `json:"model,omitempty"`
	RequestedModel      string `json:"requested_model,omitempty"`
	Path                string `json:"path,omitempty"`
	DurationMS          int64  `json:"duration_ms,omitempty"`
	Stream              bool   `json:"stream,omitempty"`
	InputTokens         int64  `json:"input_tokens,omitempty"`
	OutputTokens        int64  `json:"output_tokens,omitempty"`
	CacheReadTokens     int64  `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64  `json:"cache_creation_tokens,omitempty"`
	StopReason          string `json:"stop_reason,omitempty"`
	ProviderRequestID   string `json:"provider_request_id,omitempty"`
	Error               string `json:"error,omitempty"`
	Messages            string `json:"messages,omitempty"`
	RequestBody         string `json:"request_body,omitempty"`
	ResponseBody        string `json:"response_body,omitempty"`
	Truncated           bool   `json:"truncated,omitempty"`
}

// accessLogIn is one entry of ingestRequest.AccessLogs. Required:
// Timestamp, RequestID, StatusCode (see buildAccessLog's validation).
type accessLogIn struct {
	Timestamp       string `json:"timestamp"`
	RequestID       string `json:"request_id"`
	StatusCode      int    `json:"status_code"`
	SessionID       string `json:"session_id,omitempty"`
	ClientSessionID string `json:"client_session_id,omitempty"`
	Method          string `json:"method,omitempty"`
	JSONRPCID       string `json:"json_rpc_id,omitempty"`
	ConnectorID     string `json:"connector_id,omitempty"`
	ToolName        string `json:"tool_name,omitempty"`
	SkillName       string `json:"skill_name,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
	DurationMS      int64  `json:"duration_ms,omitempty"`
	BytesIn         int64  `json:"bytes_in,omitempty"`
	Bytes           int64  `json:"bytes,omitempty"`
	ClientIP        string `json:"client_ip,omitempty"`
	UserAgent       string `json:"user_agent,omitempty"`
	RequestBody     string `json:"request_body,omitempty"`
	ResponseBody    string `json:"response_body,omitempty"`
	Truncated       bool   `json:"truncated,omitempty"`
}

// ingestResponse is POST /api/v1/ingest's 200 body, matching the design
// doc's section 3 response shape exactly.
type ingestResponse struct {
	Accepted   ingestCounts     `json:"accepted"`
	Duplicates ingestCounts     `json:"duplicates"`
	Rejected   []ingestRejected `json:"rejected"`
}

type ingestCounts struct {
	LLMCalls   int `json:"llm_calls"`
	AccessLogs int `json:"access_logs"`
}

// ingestRejected is one record the handler refused before ever handing it
// to the sink (a validation failure), as opposed to a duplicate (which the
// sink itself detects and counts separately -- see sink.IngestResult).
type ingestRejected struct {
	Kind   string `json:"kind"` // "llm_call" | "access_log"
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// Ingest handles POST /api/v1/ingest. See the Ingest type's doc comment
// for the auth/enabled-check ordering and docs/api.md, "Ingest", for the
// full API contract.
func (h Ingest) Ingest(w http.ResponseWriter, r *http.Request) {
	if !h.IngestConfig.Enabled {
		writeIngestDisabled(w)
		return
	}

	p, ok := pkgauth.PrincipalFrom(r.Context())
	if !ok || p == nil || p.TenantID == "" {
		// Unreachable in practice: this route runs behind
		// internalauth.Middleware, which always attaches a Principal on
		// success (see router.go). Fail loudly rather than silently.
		httpx.Internal(w, "no authenticated principal in context")
		return
	}

	if h.IngestRateLimiter != nil {
		if allowed, retryAfter := h.IngestRateLimiter.Allow(p.KeyID); !allowed {
			httpx.RateLimited(w, retryAfter)
			return
		}
	}

	if h.IngestSink == nil {
		httpx.Unavailable(w, "ingest storage is not configured on this instance")
		return
	}

	maxBodyBytes := h.IngestConfig.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultIngestMaxBodyBytes
	}
	body, status, message := readIngestBody(r, maxBodyBytes)
	if status != 0 {
		writeIngestBodyErr(w, status, message)
		return
	}

	var req ingestRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	// No DisallowUnknownFields: see ingestRequest's doc comment -- a
	// tenant_id/key_id/principal-shaped field the caller sends is silently
	// ignored, per the API contract's "Gateway rules", not rejected.
	if err := dec.Decode(&req); err != nil {
		httpx.ValidationError(w, "invalid request body: "+err.Error())
		return
	}
	if dec.More() {
		httpx.ValidationError(w, "request body must contain a single JSON object")
		return
	}

	if req.SchemaVersion != 1 {
		httpx.ValidationError(w, fmt.Sprintf("unsupported schema_version %d (want 1)", req.SchemaVersion))
		return
	}

	maxRecords := h.IngestConfig.MaxRecords
	if maxRecords <= 0 {
		maxRecords = defaultIngestMaxRecords
	}
	total := len(req.LLMCalls) + len(req.AccessLogs)
	if total > maxRecords {
		httpx.ValidationError(w, fmt.Sprintf("batch carries %d records (llm_calls + access_logs), exceeds the %d-record limit", total, maxRecords))
		return
	}

	user := capASCII(req.User, maxIngestUserLen)

	var rejected []ingestRejected
	access := make([]*sink.AccessLog, 0, len(req.AccessLogs))
	for i, in := range req.AccessLogs {
		a, reason := h.buildAccessLog(in, p, user)
		if reason != "" {
			rejected = append(rejected, ingestRejected{Kind: "access_log", Index: i, Reason: reason})
			continue
		}
		access = append(access, a)
	}
	llm := make([]*sink.LLMCall, 0, len(req.LLMCalls))
	for i, in := range req.LLMCalls {
		l, reason := h.buildLLMCall(in, p, user)
		if reason != "" {
			rejected = append(rejected, ingestRejected{Kind: "llm_call", Index: i, Reason: reason})
			continue
		}
		llm = append(llm, l)
	}

	result, err := h.IngestSink.WriteIngestBatch(r.Context(), p.TenantID, access, llm)
	if err != nil {
		applog.From(r.Context()).Error("ingest batch write failed", "error", err, "tenant_id", p.TenantID)
		httpx.Unavailable(w, "ingest storage failed, retry")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, ingestResponse{
		Accepted:   ingestCounts{LLMCalls: result.AcceptedLLM, AccessLogs: result.AcceptedAccess},
		Duplicates: ingestCounts{LLMCalls: result.DuplicateLLM, AccessLogs: result.DuplicateAccess},
		Rejected:   rejected,
	})
}

// writeIngestDisabled writes the EXACT body internal/api/router.go's
// r.NotFound writes for an unmounted /api/v1/* route -- see the Ingest
// type's doc comment for why this must match byte for byte.
func writeIngestDisabled(w http.ResponseWriter) {
	httpx.NotFound(w, "not found")
}

// writeIngestBodyErr maps readIngestBody's (status, message) onto the
// gateway's shared error envelope.
func writeIngestBodyErr(w http.ResponseWriter, status int, message string) {
	switch status {
	case http.StatusRequestEntityTooLarge:
		httpx.TooLarge(w, message)
	default:
		httpx.ValidationError(w, message)
	}
}

// readIngestBody reads r's body, decompressing it first when
// Content-Encoding: gzip is set, and enforces the ingest endpoint's own
// size limits -- httpx.Decode's shared 1 MiB cap (internal/api/httpx.
// maxBodyBytes) is far too small for a batch of up to maxRecords
// LLM/access-log records, each potentially carrying full message bodies,
// so this route reads its own body rather than calling httpx.Decode (see
// the design doc, section 4 item 5). The compressed size on the wire is
// capped by the fixed maxIngestCompressedBytes regardless of config; the
// decompressed size is capped by maxBodyBytes (Deps.IngestConfig.
// MaxBodyBytes, defaulted by the caller). Returns the raw decompressed
// bytes, or a non-zero HTTP status and message to write back on failure.
func readIngestBody(r *http.Request, maxBodyBytes int) (body []byte, status int, message string) {
	if r.ContentLength > maxIngestCompressedBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds the %d byte compressed limit", maxIngestCompressedBytes)
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxIngestCompressedBytes+1))
	if err != nil {
		return nil, http.StatusBadRequest, "failed to read request body"
	}
	if len(raw) > maxIngestCompressedBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds the %d byte compressed limit", maxIngestCompressedBytes)
	}

	if r.Header.Get("Content-Encoding") != "gzip" {
		if len(raw) > maxBodyBytes {
			return nil, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds the %d byte limit", maxBodyBytes)
		}
		return raw, 0, ""
	}

	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusBadRequest, "invalid gzip body: " + err.Error()
	}
	defer gz.Close()

	out, err := io.ReadAll(io.LimitReader(gz, int64(maxBodyBytes)+1))
	if err != nil {
		return nil, http.StatusBadRequest, "failed to decompress request body: " + err.Error()
	}
	if len(out) > maxBodyBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Sprintf("decompressed body exceeds the %d byte limit", maxBodyBytes)
	}
	return out, 0, ""
}

// buildAccessLog validates in and, on success, returns the sink.AccessLog
// to write, with tenant/key/principal stamped from p (never from the
// payload -- in has no such fields to read one from in the first place)
// and Source/User stamped per the contract. On a validation failure it
// returns (nil, reason).
func (h Ingest) buildAccessLog(in accessLogIn, p *pkgauth.Principal, user string) (*sink.AccessLog, string) {
	ts, reason := parseIngestTimestamp(in.Timestamp)
	if reason != "" {
		return nil, reason
	}
	if reason := validateIngestRequestID(in.RequestID); reason != "" {
		return nil, reason
	}
	if reason := validateIngestStatusCode(in.StatusCode); reason != "" {
		return nil, reason
	}

	a := &sink.AccessLog{
		Timestamp:       ts,
		TenantID:        p.TenantID,
		Principal:       p.Subject,
		KeyID:           p.KeyID,
		RequestID:       in.RequestID,
		SessionID:       in.SessionID,
		ClientSessionID: in.ClientSessionID,
		Method:          in.Method,
		JSONRPCID:       in.JSONRPCID,
		ConnectorID:     in.ConnectorID,
		ToolName:        in.ToolName,
		SkillName:       in.SkillName,
		StatusCode:      in.StatusCode,
		ErrorCode:       in.ErrorCode,
		DurationMS:      in.DurationMS,
		BytesIn:         in.BytesIn,
		Bytes:           in.Bytes,
		ClientIP:        in.ClientIP,
		UserAgent:       in.UserAgent,
		Source:          sourceInterceptor,
		User:            user,
	}
	h.applyAccessBodies(a, in)
	return a, ""
}

// applyAccessBodies fills a's body/truncated fields from in, gated on
// Deps.Capture.StoreBodies -- the SAME setting and byte caps
// (MaxRequestBytes/MaxResponseBytes) internal/dataplane/transport applies
// to a gateway-proxied MCP call's access log, via the shared
// sink.TruncateBody helper (see that function's doc comment). StoreBodies
// off means bodies are dropped entirely, not just left uncapped.
func (h Ingest) applyAccessBodies(a *sink.AccessLog, in accessLogIn) {
	if !h.Capture.StoreBodies {
		return
	}
	reqBody, reqCut := sink.TruncateBody([]byte(in.RequestBody), h.Capture.MaxRequestBytes)
	respBody, respCut := sink.TruncateBody([]byte(in.ResponseBody), h.Capture.MaxResponseBytes)
	if len(reqBody) > 0 {
		a.RequestBody = reqBody
	}
	if len(respBody) > 0 {
		a.ResponseBody = respBody
	}
	// ORs in the caller's own truncated flag: the interceptor may already
	// know a body was cut short before it ever reached the gateway (its
	// own spool/queue caps -- see the interceptor's design), and that fact
	// is worth keeping even though this gateway's own caps didn't trigger.
	a.Truncated = reqCut || respCut || in.Truncated
}

// buildLLMCall mirrors buildAccessLog for one llm_calls entry. ClientName
// is accepted from the payload as-is when non-empty (the interceptor
// already knows it's "claude-desktop"); when empty, it's classified from
// UserAgent via the same sink.ClientFamily substring rule every other
// plane uses, so an ingested row with no ClientName still classifies
// consistently with gateway-proxied traffic. CostUSD is never set here --
// the contract says ingested cost is always NULL (subscription use isn't
// billed per token), and llmCallIn carries no cost field to read one from
// even if a caller tried.
func (h Ingest) buildLLMCall(in llmCallIn, p *pkgauth.Principal, user string) (*sink.LLMCall, string) {
	ts, reason := parseIngestTimestamp(in.Timestamp)
	if reason != "" {
		return nil, reason
	}
	if reason := validateIngestRequestID(in.RequestID); reason != "" {
		return nil, reason
	}
	if reason := validateIngestStatusCode(in.StatusCode); reason != "" {
		return nil, reason
	}

	userAgent := sink.CapUserAgent(in.UserAgent)
	clientName := in.ClientName
	if clientName == "" {
		clientName = sink.ClientFamily(userAgent)
	}

	l := &sink.LLMCall{
		Timestamp:           ts,
		TenantID:            p.TenantID,
		Principal:           p.Subject,
		KeyID:               p.KeyID,
		RequestID:           in.RequestID,
		SessionID:           in.SessionID,
		ClientIP:            in.ClientIP,
		ClientName:          clientName,
		UserAgent:           userAgent,
		Provider:            in.Provider,
		UpstreamHost:        in.UpstreamHost,
		Model:               in.Model,
		RequestedModel:      in.RequestedModel,
		Path:                in.Path,
		StatusCode:          in.StatusCode,
		DurationMS:          in.DurationMS,
		Stream:              in.Stream,
		InputTokens:         in.InputTokens,
		OutputTokens:        in.OutputTokens,
		CacheReadTokens:     in.CacheReadTokens,
		CacheCreationTokens: in.CacheCreationTokens,
		StopReason:          in.StopReason,
		ProviderRequestID:   in.ProviderRequestID,
		Error:               in.Error,
		Source:              sourceInterceptor,
		User:                user,
	}
	h.applyLLMBodies(l, in)
	// Skill/MCP discovery scans the interceptor-supplied RESPONSE body (never
	// the request), whether or not bodies are stored: it keeps names only.
	// An interceptor that sends no response_body contributes nothing.
	if in.StatusCode/100 == 2 && in.ResponseBody != "" {
		found := discovery.Scan([]byte(in.ResponseBody))
		l.SkillsUsed, l.MCPToolsUsed = found.Skills, found.MCPTools
	}
	return l, ""
}

// applyLLMBodies fills l's body/truncated fields from in, gated on
// Deps.LLMCapture.StoreBodies -- the SAME setting and byte caps
// internal/llmplane applies to a gateway-proxied LLM call. RequestBody and
// Messages are capped independently, both against MaxRequestBytes: unlike
// the async LLM-plane path (which captures one whole request body and
// parses Messages/System/Tools out of it after capping), the ingest
// contract hands request_body and messages over as two separate,
// independent strings (see llmCallIn), so there is no single "request
// body" to derive both from. Capping each against the same request-side
// limit is the simplest rule consistent with the existing cap's intent
// (bound how much of the request side is stored); this is a deliberate
// choice, not a bug, and is called out in the implementation notes.
func (h Ingest) applyLLMBodies(l *sink.LLMCall, in llmCallIn) {
	if !h.LLMCapture.StoreBodies {
		return
	}
	reqBody, reqCut := sink.TruncateBody([]byte(in.RequestBody), h.LLMCapture.MaxRequestBytes)
	messages, msgCut := sink.TruncateBody([]byte(in.Messages), h.LLMCapture.MaxRequestBytes)
	respBody, respCut := sink.TruncateBody([]byte(in.ResponseBody), h.LLMCapture.MaxResponseBytes)
	if len(reqBody) > 0 {
		l.RequestBody = reqBody
	}
	if len(messages) > 0 {
		l.Messages = messages
	}
	if len(respBody) > 0 {
		l.ResponseBody = respBody
	}
	l.Truncated = reqCut || msgCut || respCut || in.Truncated
}

// parseIngestTimestamp parses an RFC3339 timestamp, returning a non-empty
// reason on failure (empty or malformed).
func parseIngestTimestamp(raw string) (time.Time, string) {
	if raw == "" {
		return time.Time{}, "missing timestamp"
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, "invalid timestamp: must be RFC3339, e.g. 2026-01-02T15:04:05Z"
	}
	return ts, ""
}

// validateIngestRequestID enforces request_id's contract: required, at
// most maxIngestRequestIDLen chars. Uniqueness ("unique per call") is
// enforced by the sink's duplicate check (sink.IngestSink.
// WriteIngestBatch), not here -- a duplicate is a distinct outcome
// (counted separately in the response) from a validation rejection.
func validateIngestRequestID(id string) string {
	if id == "" {
		return "missing request_id"
	}
	if len(id) > maxIngestRequestIDLen {
		return fmt.Sprintf("request_id exceeds %d chars", maxIngestRequestIDLen)
	}
	return ""
}

// validateIngestStatusCode enforces status_code's contract: required (a
// zero value never arrives for a real call) and a plausible HTTP-shaped
// status.
func validateIngestStatusCode(code int) string {
	if code < 100 || code > 599 {
		return "status_code must be between 100 and 599"
	}
	return ""
}

// capASCII truncates s to at most max bytes, byte-for-byte (matching
// sink.CapUserAgent's own byte-oriented cap) -- used for the batch's top-
// level "user" field, which sink.AccessLog/LLMCall.User caps at 256 chars
// per the API contract.
func capASCII(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
