// Package llm is the public seam for translating LLM calls between wire
// formats. A client speaks one format (its Dialect: the Anthropic Messages
// API, say) and the model it names may live behind a vendor that speaks
// another (a Provider: an OpenAI-compatible Chat Completions endpoint, say).
// The gateway parses the client's request into the neutral Request defined
// here, checks the Provider can carry every field of it, lets the Provider
// build the vendor's request, and turns the vendor's answer (a Response, or a
// stream of Events) back into the client's format.
//
// The neutral types are a superset of Anthropic's content blocks and stream
// events: Anthropic's model is the richest of the formats the gateway
// speaks, so it loses nothing, and every other format maps onto a subset of
// it. Fields a format has no neutral slot for ride in Extra maps, so a
// Provider that speaks the client's own wire can pass them through, and the
// engine can refuse them for a Provider that cannot.
//
// Two adapters ship in this module and register themselves from an init():
// pkg/llm/anthropic (Dialect "anthropic" and Provider "anthropic") and
// pkg/llm/openaicompat (Provider "openai_compat"). A binary blank-imports
// the adapters it wants, exactly as cmd/gateway does. A new vendor is one
// package that implements Provider, registers it, and passes
// pkg/llm/llmtest -- see CONTRIBUTING.md, "Adding an LLM provider adapter".
package llm

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Buffer caps. A translating gateway must not hold more of a vendor's answer
// in memory than it needs: the byte-for-byte passthrough holds nothing, so a
// runaway vendor must not make translation hold unbounded data either.
const (
	// MaxFrameBytes bounds one line of an upstream stream and everything a
	// StreamDecoder holds for one content block it has not emitted yet (for
	// example a tool call buffered until the stream ends). Exceeding it ends
	// the stream with an error event.
	MaxFrameBytes = 1 << 20
	// MaxBufferedBytes bounds everything a StreamDecoder holds at once,
	// across all its pending blocks.
	MaxBufferedBytes = 16 << 20
	// MaxBodyBytes bounds a whole non-stream upstream response body.
	MaxBodyBytes = 16 << 20
)

// ErrFrameTooLarge is returned when an upstream stream line or a pending
// block exceeds MaxFrameBytes (or the decoder's total exceeds
// MaxBufferedBytes), and when a non-stream body exceeds MaxBodyBytes.
var ErrFrameTooLarge = errors.New("upstream frame too large")

// Request is the neutral form of one model call. A Dialect fills it from the
// client's request; a Provider builds the vendor's request from it.
type Request struct {
	// Model is the model id the client asked for (its alias). The engine
	// echoes it back in the Response; the vendor gets Target.Model instead.
	Model string `json:"model"`
	// System is the system prompt as text blocks, in order.
	System []Block `json:"system,omitempty"`
	// Messages is the conversation, oldest first.
	Messages []Message `json:"messages"`
	// Tools the model may call.
	Tools []Tool `json:"tools,omitempty"`
	// ToolChoice constrains tool use; nil means the vendor default (auto).
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`
	// MaxTokens caps the generated tokens; nil when the client sent none.
	MaxTokens *int64 `json:"max_tokens,omitempty"`
	// Sampling parameters; nil when the client sent none.
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	TopK        *int64   `json:"top_k,omitempty"`
	// StopSequences end generation when one is produced.
	StopSequences []string `json:"stop_sequences,omitempty"`
	// Stream asks for a stream of Events instead of one Response.
	Stream bool `json:"stream,omitempty"`
	// Metadata is the client's opaque request metadata (Anthropic's
	// metadata.user_id). It never changes the answer.
	Metadata json.RawMessage `json:"metadata,omitempty"`
	// Thinking asks the model to reason before answering; nil = default.
	Thinking *Thinking `json:"thinking,omitempty"`
	// Effort is the client's reasoning-effort hint (low|medium|high|xhigh|
	// max): Anthropic's output_config.effort, OpenAI's reasoning_effort.
	Effort string `json:"effort,omitempty"`
	// Extra holds the top-level request fields the neutral form has no slot
	// for, keyed by their wire name, verbatim. A Provider that speaks the
	// client's own wire passes them through; for any other the engine
	// refuses them unless they are on its documented drop list.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// Message is one conversation turn.
type Message struct {
	// Role is "user" or "assistant". Some clients also send "system" turns
	// mid-conversation; a Provider may carry them as user turns.
	Role string `json:"role"`
	// Content is the turn's blocks, in order. A plain-string turn is one
	// text block.
	Content []Block `json:"content"`
}

// BlockType names a content block kind.
type BlockType string

// The neutral content block kinds. Any other kind a Dialect meets is kept
// verbatim in Block.Raw with Block.Type set to its wire name.
const (
	BlockText             BlockType = "text"
	BlockImage            BlockType = "image"
	BlockDocument         BlockType = "document"
	BlockToolUse          BlockType = "tool_use"
	BlockToolResult       BlockType = "tool_result"
	BlockThinking         BlockType = "thinking"
	BlockRedactedThinking BlockType = "redacted_thinking"
	// BlockToolReference names a tool a tool-search result loaded (Anthropic
	// tool search); it appears inside tool_result content.
	BlockToolReference BlockType = "tool_reference"
)

// Block is one content block. Which fields are meaningful depends on Type.
type Block struct {
	Type BlockType `json:"type"`

	// Text is the text of a text block.
	Text string `json:"text,omitempty"`

	// Source is the payload of an image or document block.
	Source *Source `json:"source,omitempty"`

	// ID, Name and Input describe a tool_use block: the call id, the tool's
	// name and its arguments (a JSON object, verbatim).
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// ToolUseID, Content and IsError describe a tool_result block: the
	// tool_use it answers, the result (text, image and tool_reference
	// blocks; a plain-string result is one text block) and whether the tool
	// failed.
	ToolUseID string  `json:"tool_use_id,omitempty"`
	Content   []Block `json:"content,omitempty"`
	IsError   bool    `json:"is_error,omitempty"`

	// Thinking and Signature describe a thinking block: the reasoning text
	// and the vendor's signature over it ("" when unsigned; only the vendor
	// that signed a block can verify it).
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// Data is the opaque payload of a redacted_thinking block.
	Data string `json:"data,omitempty"`

	// ToolName is the tool a tool_reference block names.
	ToolName string `json:"tool_name,omitempty"`

	// Extra holds the block's fields the neutral form has no slot for
	// (cache_control, citations, a document's title, ...), verbatim.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`

	// Raw is the whole wire block for a kind outside the neutral set
	// (server_tool_use, web_search_tool_result, search_result, ...). Only a
	// Provider that speaks the client's own wire can carry it.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// Source is the payload of an image or document block.
type Source struct {
	// Type is how the payload is given: "base64" (Data), "url" (URL),
	// "text" (Data is plain text; documents only), "file" (FileID, a
	// vendor-side upload) or "content" (Content; documents only).
	Type      string          `json:"type"`
	MediaType string          `json:"media_type,omitempty"`
	Data      string          `json:"data,omitempty"`
	URL       string          `json:"url,omitempty"`
	FileID    string          `json:"file_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// Tool is a tool the model may call.
type Tool struct {
	// Type is "" (or "custom") for a client-executed tool described by
	// InputSchema; anything else names a vendor-defined tool
	// ("web_search_20250305", "bash_20250124", ...) the vendor runs or
	// defines itself.
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// Extra holds the tool's other fields (cache_control, strict,
	// defer_loading, a typed tool's max_uses, ...), verbatim.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// ToolChoice constrains tool use.
type ToolChoice struct {
	// Type is "auto" (the model decides), "any" (must call some tool),
	// "tool" (must call Name) or "none" (must not call a tool).
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	// DisableParallelToolUse asks for at most one tool call per turn.
	DisableParallelToolUse bool `json:"disable_parallel_tool_use,omitempty"`
}

// Thinking configures extended reasoning.
type Thinking struct {
	// Type is "enabled" (with BudgetTokens), "adaptive" (the model decides)
	// or "disabled".
	Type         string `json:"type"`
	BudgetTokens int64  `json:"budget_tokens,omitempty"`
	// Extra holds the config's other fields, verbatim.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// Response is the neutral form of one complete (non-stream) answer, and the
// message header of a stream's message_start event.
type Response struct {
	// ID is the vendor's response id.
	ID string `json:"id"`
	// Model is the model id reported to the client. The engine sets it to
	// Request.Model (the client's alias) before rendering.
	Model string `json:"model"`
	// Role is always "assistant".
	Role string `json:"role"`
	// Content is the answer's blocks, in order (empty in message_start).
	Content []Block `json:"content"`
	// StopReason is why generation ended: end_turn, max_tokens,
	// stop_sequence, tool_use, pause_turn or refusal ("" in message_start).
	StopReason string `json:"stop_reason,omitempty"`
	// StopSequence is the stop sequence that matched, when the vendor says.
	StopSequence string `json:"stop_sequence,omitempty"`
	// Usage is the token accounting.
	Usage Usage `json:"usage"`
}

// Usage is token accounting in Anthropic's convention: InputTokens EXCLUDES
// the cached prefix (CacheReadTokens) and the tokens written to the cache
// (CacheWriteTokens). A Provider whose vendor folds cached tokens into its
// prompt count subtracts them.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	// CacheWrite1hTokens is the part of CacheWriteTokens written with a
	// one-hour TTL.
	CacheWrite1hTokens int64 `json:"cache_write_1h_tokens,omitempty"`
	// WebSearchRequests counts server-side web searches (billed per call).
	WebSearchRequests int64 `json:"web_search_requests,omitempty"`
}

// EventType names a stream event. The set and its order mirror Anthropic's
// Messages stream: message_start, then per content block a
// content_block_start, its content_block_deltas and a content_block_stop
// (one block open at a time, indices 0, 1, 2, ...), then message_delta and
// message_stop. ping may appear anywhere after message_start; error ends
// the stream.
type EventType string

const (
	EventMessageStart      EventType = "message_start"
	EventContentBlockStart EventType = "content_block_start"
	EventContentBlockDelta EventType = "content_block_delta"
	EventContentBlockStop  EventType = "content_block_stop"
	EventMessageDelta      EventType = "message_delta"
	EventMessageStop       EventType = "message_stop"
	EventPing              EventType = "ping"
	EventError             EventType = "error"
)

// Event is one stream event. Which fields are set depends on Type.
type Event struct {
	Type EventType `json:"type"`
	// Message is the message header of message_start (Content empty; Usage
	// carries whatever input accounting is known up front).
	Message *Response `json:"message,omitempty"`
	// Index is the content block index of a content_block_* event.
	Index int `json:"index"`
	// Block is the opening state of content_block_start: a text block with
	// empty Text, a thinking block with empty Thinking, a tool_use block
	// with ID and Name and no Input.
	Block *Block `json:"block,omitempty"`
	// Delta is the increment of content_block_delta.
	Delta *Delta `json:"delta,omitempty"`
	// StopReason and StopSequence are message_delta's final stop state.
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
	// Usage is message_delta's cumulative usage.
	Usage *Usage `json:"usage,omitempty"`
	// Error is the failure of an error event.
	Error *Error `json:"error,omitempty"`
}

// DeltaType names a content_block_delta increment.
type DeltaType string

const (
	DeltaText      DeltaType = "text_delta"
	DeltaThinking  DeltaType = "thinking_delta"
	DeltaSignature DeltaType = "signature_delta"
	DeltaInputJSON DeltaType = "input_json_delta"
)

// Delta is one increment of a content block.
type Delta struct {
	Type DeltaType `json:"type"`
	// Text extends a text block.
	Text string `json:"text,omitempty"`
	// Thinking extends a thinking block.
	Thinking string `json:"thinking,omitempty"`
	// Signature sets a thinking block's signature.
	Signature string `json:"signature,omitempty"`
	// PartialJSON extends a tool_use block's input; the concatenation of a
	// block's fragments is its JSON input object.
	PartialJSON string `json:"partial_json,omitempty"`
}

// Error types, in Anthropic's taxonomy (the neutral one). A Provider maps its
// vendor's errors onto these; a Dialect maps them onto its client's.
const (
	ErrorTypeInvalidRequest = "invalid_request_error"
	ErrorTypeAuthentication = "authentication_error"
	ErrorTypePermission     = "permission_error"
	ErrorTypeNotFound       = "not_found_error"
	ErrorTypeRequestTooBig  = "request_too_large"
	ErrorTypeRateLimit      = "rate_limit_error"
	ErrorTypeBilling        = "billing_error"
	ErrorTypeTimeout        = "timeout_error"
	ErrorTypeOverloaded     = "overloaded_error"
	ErrorTypeAPI            = "api_error"
)

// CodeContextLength marks an Error caused by a prompt longer than the
// model's context window, so a Dialect can phrase it the way its clients
// recognize (Claude Code compacts on Anthropic's "prompt is too long").
const CodeContextLength = "context_length_exceeded"

// Error is a failure to report to the client: a vendor error, or one the
// gateway raises itself.
type Error struct {
	// Status is the HTTP status the client gets (0 inside a stream).
	Status int `json:"status,omitempty"`
	// Type is one of the ErrorType* constants.
	Type    string `json:"type"`
	Message string `json:"message"`
	// Code is a machine-readable cause (CodeContextLength), if known.
	Code string `json:"code,omitempty"`
	// RequestID is the gateway's request id, echoed in the envelope.
	RequestID string `json:"request_id,omitempty"`
}

func (e *Error) Error() string { return e.Type + ": " + e.Message }

// ErrorTypeForStatus maps an HTTP status onto an error type.
func ErrorTypeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return ErrorTypeInvalidRequest
	case http.StatusUnauthorized:
		return ErrorTypeAuthentication
	case http.StatusPaymentRequired:
		return ErrorTypeBilling
	case http.StatusForbidden:
		return ErrorTypePermission
	case http.StatusNotFound:
		return ErrorTypeNotFound
	case http.StatusRequestEntityTooLarge:
		return ErrorTypeRequestTooBig
	case http.StatusTooManyRequests:
		return ErrorTypeRateLimit
	case http.StatusGatewayTimeout:
		return ErrorTypeTimeout
	case http.StatusServiceUnavailable, 529:
		return ErrorTypeOverloaded
	}
	if status >= 400 && status < 500 {
		return ErrorTypeInvalidRequest
	}
	return ErrorTypeAPI
}

// ErrUnsupported reports a request field a Provider cannot carry. The engine
// answers it with a 400 whose message is Error(); a Provider's BuildRequest
// may also return it for a limit that depends on the target model.
type ErrUnsupported struct {
	// Field locates the field, e.g. "messages[0].content[1] (document)".
	Field string
}

func (e *ErrUnsupported) Error() string { return "unsupported_by_route: " + e.Field }

// RequestError marks a Dialect or Provider failure caused by the client's
// request itself (malformed JSON, an empty conversation): the engine answers
// 400 with its message. Any other BuildRequest error is the gateway's (502).
type RequestError struct{ Err error }

func (e *RequestError) Error() string { return e.Err.Error() }
func (e *RequestError) Unwrap() error { return e.Err }

// Target is where a Provider sends one call.
type Target struct {
	// Vendor is the operator's label for the vendor behind BaseURL
	// ("openai", "deepseek", "ollama", ...). It keys pricing and capture, and
	// a Provider may use it to pick vendor quirks.
	Vendor string `json:"vendor"`
	// BaseURL is the vendor API root; the Provider appends its own path.
	BaseURL string `json:"base_url"`
	// Model is the vendor's model id, sent upstream in place of
	// Request.Model.
	Model string `json:"model"`
	// Auth is the credential to present.
	Auth Auth `json:"-"`
}

// Auth is the credential a Provider presents to the vendor.
type Auth struct {
	// APIKey is the vendor key (sent as the vendor expects: a Bearer token,
	// an x-api-key header, ...).
	APIKey string
}

// Capabilities lists what a Provider can carry. The engine refuses any
// request that uses a feature whose flag is false, instead of silently
// dropping it; see the engine's documented drop list for the few
// non-semantic fields that are dropped instead.
type Capabilities struct {
	// Passthrough means the Provider speaks the client's own wire and
	// carries every field and block verbatim, Extra and Raw included. Every
	// other flag is then implied.
	Passthrough bool
	// ImagesBase64 / ImagesURL: image blocks given inline / by URL.
	ImagesBase64 bool
	ImagesURL    bool
	// TextDocuments: document blocks with a plain-text source.
	TextDocuments bool
	// Documents: document blocks with any other source (PDF, URL, file).
	Documents bool
	// Thinking: a thinking request, and thinking blocks in the answer.
	Thinking bool
	// ToolChoiceNone: tool_choice "none".
	ToolChoiceNone bool
	// ParallelToolControl: tool_choice.disable_parallel_tool_use.
	ParallelToolControl bool
	// ToolReferences: tool_reference blocks inside tool results.
	ToolReferences bool
	// StopSequences: client stop sequences.
	StopSequences bool
	// ServerTools: vendor-defined (typed) tools such as web_search.
	ServerTools bool
}
