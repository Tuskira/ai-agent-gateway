package llmplane

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// The canonical conversation the detection tee hands the agent: a small
// projection of the neutral pkg/llm types, the same whatever the call's wire
// format. The agent mirrors these types field for field (detection/wire:
// Conversation, Message, ContentBlock, Answer, TurnItem), pinned by the
// shared fixtures in testdata/detection.

// teeConversationVersion is teeConversation.Version ("cv").
const teeConversationVersion = 1

// teeOpaqueRaw bounds teeBlock.Raw; a larger wire block is carried as a
// JSON string of its first teeOpaqueRaw bytes.
const teeOpaqueRaw = 64 << 10

// historyReaders are the Readers that mark a request whose Messages are
// not the whole conversation (llm.HistoryKey) and refuse a body that sets
// the mark itself. Another Reader keeps an unknown top-level field in
// Extra, so a client could forge it there: its requests are always full.
var historyReaders = map[string]bool{
	readerOpenAIResponses: true, readerOpenAICompletions: true,
	readerAnthropicComplete: true, readerBedrockInvoke: true,
}

// Canonical block types; anything else is "opaque".
const (
	teeBlockText       = "text"
	teeBlockToolUse    = "tool_use"
	teeBlockToolResult = "tool_result"
	teeBlockThinking   = "thinking"
	teeBlockImage      = "image"
	teeBlockDocument   = "document"
	teeBlockOpaque     = "opaque"
)

type teeConversation struct {
	Version  int          `json:"cv"`
	System   []teeBlock   `json:"system,omitempty"`
	Messages []teeMessage `json:"messages"`
	Tools    []string     `json:"tools,omitempty"`
	History  string       `json:"history"`
}

type teeMessage struct {
	Role    string     `json:"role"`
	Content []teeBlock `json:"content"`
}

// teeBlock is one content block. image keeps only its media type and
// payload size; document too, plus its text when it is a text document (an
// instruction hidden in an attached file is still read); opaque keeps its
// wire form (clipped).
type teeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   []teeBlock      `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	Bytes     int             `json:"bytes,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

type teeAnswer struct {
	Content    []teeBlock `json:"content"`
	StopReason string     `json:"stop_reason,omitempty"`
	Truncated  bool       `json:"truncated,omitempty"`
}

// teeTurnItem is one request of a batch (reserved for batch readers).
type teeTurnItem struct {
	CustomID     string          `json:"custom_id"`
	Conversation teeConversation `json:"conversation"`
}

// toConversation projects a request read by the named Reader: the system
// prompt, every message (a mid-conversation system or developer message
// stays a message), the tool names, in order, and how much of the
// conversation the request carries.
func toConversation(req *llm.Request, reader string) teeConversation {
	c := teeConversation{Version: teeConversationVersion, System: toBlocks(req.System),
		Messages: make([]teeMessage, 0, len(req.Messages)), History: historyOf(req, reader)}
	for _, m := range req.Messages {
		c.Messages = append(c.Messages, teeMessage{Role: m.Role, Content: nonNil(toBlocks(m.Content))})
	}
	for _, t := range req.Tools {
		name := t.Name
		if name == "" {
			name = t.Type // a vendor-defined tool known by its type alone (web_search_preview)
		}
		c.Tools = append(c.Tools, name)
	}
	return c
}

// historyOf is the request's llm.HistoryKey mark, llm.HistoryFull when it
// has none or its Reader cannot set one.
func historyOf(req *llm.Request, reader string) string {
	var v string
	if historyReaders[reader] && json.Unmarshal(req.Extra[llm.HistoryKey], &v) == nil &&
		(v == llm.HistoryServerSide || v == llm.HistoryPrompt) {
		return v
	}
	return llm.HistoryFull
}

// toAnswer projects a response; truncated marks one that was cut.
func toAnswer(resp *llm.Response, truncated bool) teeAnswer {
	return teeAnswer{Content: nonNil(toBlocks(resp.Content)), StopReason: resp.StopReason, Truncated: truncated}
}

func nonNil(bs []teeBlock) []teeBlock {
	if bs == nil {
		return []teeBlock{}
	}
	return bs
}

func toBlocks(bs []llm.Block) []teeBlock {
	if len(bs) == 0 {
		return nil
	}
	out := make([]teeBlock, 0, len(bs))
	for _, b := range bs {
		out = append(out, toBlock(b))
	}
	return out
}

func toBlock(b llm.Block) teeBlock {
	switch {
	case b.Raw != nil:
		return opaqueBlock(b.Raw)
	case b.Type == llm.BlockText:
		return teeBlock{Type: teeBlockText, Text: b.Text}
	case b.Type == llm.BlockThinking:
		return teeBlock{Type: teeBlockThinking, Text: b.Thinking}
	case b.Type == llm.BlockToolUse:
		return teeBlock{Type: teeBlockToolUse, ID: b.ID, Name: b.Name, Input: b.Input}
	case b.Type == llm.BlockToolResult:
		return teeBlock{Type: teeBlockToolResult, ToolUseID: b.ToolUseID, Content: toBlocks(b.Content), IsError: b.IsError}
	case b.Type == llm.BlockImage || b.Type == llm.BlockDocument:
		out := teeBlock{Type: teeBlockImage}
		if b.Type == llm.BlockDocument {
			out.Type = teeBlockDocument
		}
		if s := b.Source; s != nil {
			out.MediaType = s.MediaType
			if s.Type == "text" && out.MediaType == "" {
				out.MediaType = "text/plain"
			}
			out.Bytes = len(s.Data) + len(s.Content)
			if b.Type == llm.BlockDocument {
				out.Text = documentText(s)
			}
		}
		return out
	}
	// A neutral kind with no canonical slot (redacted_thinking,
	// tool_reference): its neutral form, without a redacted payload.
	b.Data = ""
	raw, _ := json.Marshal(b)
	return opaqueBlock(raw)
}

// documentText is the text of a text document: plain text, base64 of a
// text/* media type (valid UTF-8 only), or the text blocks of a content
// source. "" for anything else (a PDF, a reference such as a URL or file
// id).
func documentText(s *llm.Source) string {
	switch s.Type {
	case "text":
		return s.Data
	case "base64":
		if !strings.HasPrefix(s.MediaType, "text/") {
			return ""
		}
		b, err := base64.StdEncoding.DecodeString(s.Data)
		if err != nil || !utf8.Valid(b) {
			return ""
		}
		return string(b)
	case "content":
		var str string
		if json.Unmarshal(s.Content, &str) == nil {
			return str
		}
		var bs []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(s.Content, &bs)
		var parts []string
		for _, b := range bs {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// opaqueBlock carries raw verbatim, or, past teeOpaqueRaw, its first
// teeOpaqueRaw bytes (cut on a rune boundary) as a JSON string.
func opaqueBlock(raw json.RawMessage) teeBlock {
	out := teeBlock{Type: teeBlockOpaque, Raw: raw, Bytes: len(raw)}
	if len(raw) > teeOpaqueRaw {
		cut := teeOpaqueRaw
		for cut > 0 && !utf8.RuneStart(raw[cut]) {
			cut--
		}
		out.Raw, _ = json.Marshal(string(raw[:cut]))
	}
	return out
}

// foldEvents assembles stream events, as a Reader decodes them, into the
// response they describe. A block left open by a cut stream keeps what it
// got; a tool call's partial arguments that are not yet valid JSON are kept
// as a JSON string.
func foldEvents(evs []llm.Event) *llm.Response {
	resp := &llm.Response{}
	var blocks []llm.Block
	var args []*strings.Builder // per block: its tool call's argument fragments
	for _, ev := range evs {
		switch ev.Type {
		case llm.EventMessageStart:
			if ev.Message != nil {
				resp.ID, resp.Model, resp.Role = ev.Message.ID, ev.Message.Model, ev.Message.Role
			}
		case llm.EventContentBlockStart:
			if ev.Block == nil || ev.Index != len(blocks) {
				continue // out of order: the Reader's contract says it cannot be
			}
			blocks = append(blocks, *ev.Block)
			args = append(args, &strings.Builder{})
		case llm.EventContentBlockDelta:
			i := ev.Index
			if i < 0 || i >= len(blocks) || ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case llm.DeltaText:
				blocks[i].Text += ev.Delta.Text
			case llm.DeltaThinking:
				blocks[i].Thinking += ev.Delta.Thinking
			case llm.DeltaSignature:
				blocks[i].Signature = ev.Delta.Signature
			case llm.DeltaInputJSON:
				args[i].WriteString(ev.Delta.PartialJSON)
			}
		case llm.EventMessageDelta:
			resp.StopReason, resp.StopSequence = ev.StopReason, ev.StopSequence
		}
	}
	for i := range blocks {
		if blocks[i].Type != llm.BlockToolUse || args[i].Len() == 0 {
			continue
		}
		if in := args[i].String(); json.Valid([]byte(in)) {
			blocks[i].Input = json.RawMessage(in)
		} else {
			blocks[i].Input, _ = json.Marshal(in)
		}
	}
	resp.Content = blocks
	return resp
}
