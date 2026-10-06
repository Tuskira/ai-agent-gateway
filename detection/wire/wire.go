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
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/turn"
	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire/conv"
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
//
// A batch (OpBatch) has no Conversation and no Answer (its response is the
// batch object, which holds no generation): Items holds each request of
// the batch, at most MaxBatchItems of them, under the id the client gave
// it. A batch the gateway could not read has no Items and says why in
// NormalizeError; one it read only in part (more than MaxBatchItems
// requests) has both.
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

// The canonical conversation types and constants live in package conv,
// which package turn reads too; they are re-exported here as the contract.
type (
	Conversation = conv.Conversation
	Message      = conv.Message
	ContentBlock = conv.ContentBlock
	Answer       = conv.Answer
)

// ConversationVersion is Conversation.Version ("cv").
const ConversationVersion = conv.ConversationVersion

// Conversation.History values.
const (
	HistoryFull       = conv.HistoryFull
	HistoryServerSide = conv.HistoryServerSide
	HistoryPrompt     = conv.HistoryPrompt
)

// ContentBlock types.
const (
	ContentText       = conv.ContentText
	ContentToolUse    = conv.ContentToolUse
	ContentToolResult = conv.ContentToolResult
	ContentThinking   = conv.ContentThinking
	ContentImage      = conv.ContentImage
	ContentDocument   = conv.ContentDocument
	ContentOpaque     = conv.ContentOpaque
)

// MaxOpaqueRaw bounds ContentBlock.Raw.
const MaxOpaqueRaw = conv.MaxOpaqueRaw

// MaxBatchItems bounds Turn.Items: the gateway reads at most this many
// requests of a batch, and the agent judges at most this many.
const MaxBatchItems = 1000

// TurnItem is one request of a batch: CustomID is the id the client gave
// it (custom_id; for a Gemini batch, its metadata key or else its
// position), Conversation the request in the canonical shape.
type TurnItem struct {
	CustomID     string       `json:"custom_id"`
	Conversation Conversation `json:"conversation"`
}

// Call is the turn as package turn prepares it: the raw bodies, with the
// canonical conversation and answer only when the gateway read them
// without error (NormalizeError empty) in a version this agent knows.
// Otherwise the raw bodies are read, as from a gateway without readers.
func (t Turn) Call() turn.Call {
	c := turn.Call{Request: t.Request, Response: t.Response}
	if t.NormalizeError == "" && t.Conversation != nil && t.Conversation.Version <= ConversationVersion {
		c.Conversation, c.Answer = t.Conversation, t.Answer
	}
	return c
}

// MetaOfTurn is the Meta of a gateway turn, without History (it depends on
// what each stage is read from). Meta has no field for StatusCode, so the
// engine does not see it.
func MetaOfTurn(t Turn) Meta {
	return Meta{At: t.At, TenantID: t.TenantID, RequestID: t.ID, SessionID: t.SessionID,
		KeyID: t.KeyID, Principal: t.Principal, Model: t.Model, Path: t.Path, Dialect: t.Dialect, Op: t.Op}
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
	// Dialect and Op are the gateway turn's (Turn.Dialect, Turn.Op): the
	// wire format the call was made in and what the route does.
	Dialect string `json:"dialect,omitempty"`
	Op      string `json:"op,omitempty"`
	// Item is, for one request of a batch, its id within the batch
	// (TurnItem.CustomID, clipped and redacted): each request is judged
	// as its own call, all under the batch call's RequestID.
	Item string `json:"item,omitempty"`
	// History is how much of the conversation the judged request carries
	// (Conversation.History: full, server_side or prompt), when the stage
	// was read from a canonical conversation; empty means full.
	History string `json:"history,omitempty"`
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
