// Package wire is the JSON contract between the three processes of a
// detection:
//
//	OSS gateway  --Turn (202)-->    detection agent (sidecar, localhost)
//	agent        --DetectRequest--> detection engine (remote)
//	             <--DetectResponse-
//	agent        --GET /v1/policy-> engine: PolicyResponse
//
// The gateway does not import this package; it mirrors Turn field for field
// (docs/llm-plane.md, "Detection agent"), pinned by the fixtures in
// testdata/gateway. TurnRequest and Verdict are the older inline contract
// (/v1/turns/request and /v1/turns/response), which the gateway's current
// tee does not use. Every id is the gateway's request id, which also keys
// the gateway's own logs.
package wire

import (
	"encoding/json"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/turn"
)

// Paths served by the agent (to the gateway) and the engine (to the agent).
const (
	PathTurns        = "/v1/turns"          // agent: one complete turn, accepted at once (202)
	PathTurnRequest  = "/v1/turns/request"  // agent, inline contract: synchronous verdict
	PathTurnResponse = "/v1/turns/response" // agent, inline contract: accepted at once (202)
	PathDetect       = "/v1/detect"         // engine
	PathPolicy       = "/v1/policy"         // engine, ?tenant_id=
)

// TurnVersion is the gateway-to-agent contract's version (Turn.V). The agent
// takes an absent or zero V as 1 and refuses a higher one.
const TurnVersion = 1

// Turn is one completed call as the gateway's detection tee posts it to
// POST /v1/turns. Request is the client's raw request body in full; Response
// is the first 1 MiB of what the client received and is present only when
// the upstream answered 2xx and the relay completed. Byte fields are base64
// on the wire.
//
// Op is what the route does (OpGenerate or OpBatch; the gateway sends no
// other). Dialect names the wire format the gateway read the bodies in
// ("anthropic", "openai_chat", ...), "" when the route has none. When it
// could read them, Conversation is the request and Answer the response
// (when there is one), in one canonical shape whatever the dialect; when it
// could not, NormalizeError says why and only the raw bodies are there.
// Items is reserved for batches and not sent yet.
type Turn struct {
	V              int           `json:"v,omitempty"`
	ID             string        `json:"id"`
	TenantID       string        `json:"tenant_id"`
	SessionID      string        `json:"session_id,omitempty"`
	KeyID          string        `json:"key_id,omitempty"`
	Principal      string        `json:"principal,omitempty"`
	Model          string        `json:"model,omitempty"`
	Path           string        `json:"path,omitempty"`
	At             time.Time     `json:"at"`
	StatusCode     int           `json:"status_code,omitempty"`
	Request        []byte        `json:"request"`
	Response       []byte        `json:"response,omitempty"`
	Dialect        string        `json:"dialect,omitempty"`
	Op             string        `json:"op,omitempty"`
	Conversation   *Conversation `json:"conversation,omitempty"`
	Answer         *Answer       `json:"answer,omitempty"`
	NormalizeError string        `json:"normalize_error,omitempty"`
	Items          []TurnItem    `json:"items,omitempty"`
}

// Turn.Op values.
const (
	OpGenerate = "generate" // one model call
	OpBatch    = "batch"    // a batch of model calls submitted at once
)

// ConversationVersion is Conversation.Version ("cv"): bumped on a change an
// older reader would misread. Fields are only ever added.
const ConversationVersion = 1

// Conversation.History values: how much of the conversation the request
// carries.
const (
	HistoryFull       = "full"        // the whole conversation (messages APIs)
	HistoryServerSide = "server_side" // earlier turns live on the vendor
	HistoryPrompt     = "prompt"      // a bare prompt, no turns
)

// Conversation is a request in the canonical shape: the system prompt, the
// messages oldest first, and the names of the tools offered. Message roles
// are "user", "assistant" and "system" (a system or developer message after
// the first turn).
type Conversation struct {
	Version  int            `json:"cv"`
	System   []ContentBlock `json:"system,omitempty"`
	Messages []Message      `json:"messages"`
	Tools    []string       `json:"tools,omitempty"`
	History  string         `json:"history"`
}

// Message is one conversation turn.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock types.
const (
	ContentText       = "text"
	ContentToolUse    = "tool_use"
	ContentToolResult = "tool_result"
	ContentThinking   = "thinking"
	ContentImage      = "image"
	ContentDocument   = "document"
	ContentOpaque     = "opaque"
)

// MaxOpaqueRaw bounds ContentBlock.Raw. A block whose wire form is larger
// carries its first MaxOpaqueRaw bytes as a JSON string instead.
const MaxOpaqueRaw = 64 << 10

// ContentBlock is one content block; which fields are set depends on Type.
// text and thinking: Text. tool_use: ID, Name, Input (the arguments,
// verbatim JSON; a JSON string when a cut stream left them incomplete).
// tool_result: ToolUseID, Content, IsError. image and document: MediaType
// and Bytes (the size of the inline payload as sent; 0 for a reference such
// as a URL or file id); the payload itself is dropped, except that a text
// document (plain text, a text/* media type, or a content source's text
// blocks) keeps its text in Text. opaque (a kind the
// canonical shape has no slot for): Raw, the wire block verbatim, or its
// first MaxOpaqueRaw bytes as a JSON string when larger, and Bytes, its
// full size.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   []ContentBlock  `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	Bytes     int             `json:"bytes,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// Answer is a response in the canonical shape. Truncated is set when the
// response was cut (the 1 MiB copy ended first, or the stream ended before
// the model finished): Content is what was read up to there.
type Answer struct {
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason,omitempty"`
	Truncated  bool           `json:"truncated,omitempty"`
}

// TurnItem is one request of a batch (reserved: not sent yet).
type TurnItem struct {
	CustomID     string       `json:"custom_id"`
	Conversation Conversation `json:"conversation"`
}

// MetaOfTurn is the Meta of a gateway turn. Meta has no field for
// StatusCode, so the engine does not see it.
func MetaOfTurn(t Turn) Meta {
	return Meta{At: t.At, TenantID: t.TenantID, RequestID: t.ID, SessionID: t.SessionID,
		KeyID: t.KeyID, Principal: t.Principal, Model: t.Model, Path: t.Path}
}

// TurnRequest is one call's stage (inline contract) as the gateway hands it to the agent: the
// client's raw request body and, for the response stage, the first 1 MiB of
// what the client received.
type TurnRequest struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	SessionID string    `json:"session_id,omitempty"`
	KeyID     string    `json:"key_id,omitempty"`
	Principal string    `json:"principal,omitempty"`
	Model     string    `json:"model,omitempty"`
	Path      string    `json:"path"`
	At        time.Time `json:"at"`
	Request   []byte    `json:"request"`
	Response  []byte    `json:"response,omitempty"`
}

// Verdict is the agent's answer to a request-stage TurnRequest. Block is set
// when a rule refuses the call (Prevention only); the gateway then answers
// 400 and never forwards. WantResponse tells the gateway whether to send the
// response stage at all (some response rule is enabled).
type Verdict struct {
	Block        *Block `json:"block,omitempty"`
	WantResponse bool   `json:"want_response"`
}

// Block is what the gateway needs to refuse a call with today's message.
type Block struct {
	RuleID      string   `json:"rule_id"`
	OWASP       string   `json:"owasp"`
	Title       string   `json:"title"`
	Probability *float64 `json:"probability,omitempty"`
	Threshold   float64  `json:"threshold"`
	Detail      string   `json:"detail,omitempty"`
}

// Meta is who and what a detection is about.
type Meta struct {
	At        time.Time `json:"at"`
	TenantID  string    `json:"tenant_id"`
	RequestID string    `json:"request_id"`
	SessionID string    `json:"session_id,omitempty"`
	KeyID     string    `json:"key_id,omitempty"`
	Principal string    `json:"principal,omitempty"`
	Model     string    `json:"model,omitempty"`
	Path      string    `json:"path,omitempty"`
}

// MetaOf is the Meta of a gateway turn.
func MetaOf(t TurnRequest) Meta {
	return Meta{At: t.At, TenantID: t.TenantID, RequestID: t.ID, SessionID: t.SessionID,
		KeyID: t.KeyID, Principal: t.Principal, Model: t.Model, Path: t.Path}
}

// Version is the agent→engine contract's version, sent as DetectRequest.V.
// Fields are only ever added (both ends ignore a field they do not know), so
// a newer agent and an older engine still work together; Version goes up
// only for a change an older engine would misread, which it then refuses.
const Version = 1

// UnsupportedVersion begins the engine's 400 answer to a DetectRequest of a
// version above its own, so the agent can tell it apart from a bad request.
const UnsupportedVersion = "unsupported wire version"

// DetectRequest asks the engine to judge one prepared (redacted) stage.
// Sync is informational: the engine always records before it answers.
type DetectRequest struct {
	// V is the agent's Version; absent (an agent older than versioning)
	// means 1.
	V    int               `json:"v,omitempty"`
	Sync bool              `json:"sync"`
	Meta Meta              `json:"meta"`
	Turn turn.PreparedTurn `json:"turn"`
}

// DetectResponse is the engine's verdict on a stage: Result carries only
// Stage and Blocking (without evidence), and is nil when no rule applied.
type DetectResponse struct {
	Result *Judgment `json:"result"`
}

// Judgment is the engine's answer for a stage: the stage and the blocking
// rule, if any. (The engine's richer rules.Result never crosses the wire.)
type Judgment struct {
	Stage    turn.Stage `json:"stage"`
	Blocking *Block     `json:"blocking,omitempty"`
}

// PolicyResponse is what the agent needs to route a tenant's calls without
// asking the engine each time: Inline (Prevention with an enforcing block
// rule: judge requests synchronously) and WantResponse (judge responses).
type PolicyResponse struct {
	Inline       bool `json:"inline"`
	WantResponse bool `json:"want_response"`
}
