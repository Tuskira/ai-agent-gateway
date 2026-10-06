// Package conv holds the canonical conversation the gateway's detection
// tee sends with a turn (wire.Turn's Conversation and Answer): a request
// and its response in one shape whatever the call's wire format. It is a
// leaf, so both package wire (the contract) and package turn (which reads
// it) can import it; wire re-exports every name.
package conv

import "encoding/json"

// ConversationVersion is Conversation.Version ("cv"): bumped on a change an
// older reader would misread. Fields are only ever added.
const ConversationVersion = 1

// Conversation.History values: how much of the conversation the request
// carries.
const (
	HistoryFull       = "full"        // the whole conversation (messages APIs)
	HistoryServerSide = "server_side" // earlier turns live on the vendor
	HistoryPrompt     = "prompt"      // one flat prompt text; any turns were split out of it best effort
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
