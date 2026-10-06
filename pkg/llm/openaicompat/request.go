// Package openaicompat is the pkg/llm Provider for OpenAI-compatible Chat
// Completions endpoints (OpenAI, xAI, DeepSeek, GLM, Kimi, Mistral, Groq,
// Gemini's OpenAI endpoint, Ollama, vLLM, ...), the Reader of that wire
// (ChatReader), and Readers of OpenAI's Responses API (ResponsesReader) and
// legacy Completions (CompletionsReader). They register as "openai_compat",
// "openai_chat", "openai_responses" and "openai_completions" from init();
// blank-import the package to use them.
//
// One Provider serves every such vendor, so it sends only the fields they all
// share. What it cannot carry is declared in Capabilities, and the engine
// refuses a request that needs it rather than letting this package drop it.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// Name is the registry name.
const Name = "openai_compat"

func init() {
	llm.RegisterProvider(Provider{})
	llm.RegisterReader(ChatReader{})
}

// Provider calls an OpenAI-compatible Chat Completions endpoint at
// {Target.BaseURL}/chat/completions with "Authorization: Bearer
// {Target.Auth.APIKey}".
type Provider struct{}

func (Provider) Name() string { return Name }

// Capabilities: images (inline and URL), plain-text documents (as text),
// thinking (the vendor's reasoning comes back as unsigned thinking blocks; a
// thinking budget is advisory), tool_choice none, parallel-call control,
// tool_reference results (as text) and stop sequences. Not PDF/URL/file
// documents, vendor-defined tools, or anything only Anthropic's wire carries.
func (Provider) Capabilities() llm.Capabilities {
	return llm.Capabilities{
		ImagesBase64: true, ImagesURL: true, TextDocuments: true, Thinking: true, ToolChoiceNone: true,
		ParallelToolControl: true, ToolReferences: true, StopSequences: true,
	}
}

// BuildRequest POSTs the Chat Completions body with a FRESH header set:
// Content-Type, Accept and the target's Bearer key, nothing else.
func (Provider) BuildRequest(ctx context.Context, req *llm.Request, t llm.Target) (*http.Request, error) {
	body, err := chatBody(req, t.Model, WantsReasoningBack(t.Vendor, t.BaseURL, t.Model))
	if err != nil {
		return nil, err
	}
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(t.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Accept", "application/json")
	if req.Stream {
		up.Header.Set("Accept", "text/event-stream")
	}
	if t.Auth.APIKey != "" {
		up.Header.Set("Authorization", "Bearer "+t.Auth.APIKey)
	}
	return up, nil
}

// WantsReasoningBack reports the vendors whose thinking models expect their
// earlier reasoning_content on later turns (DeepSeek, Kimi, GLM), by vendor
// label or base URL host, and Kimi and GLM models by id on any other host
// (Nebius, Together, ...). Everyone else gets none: strict-schema vendors
// (Groq) reject the field outright, whatever model they serve.
func WantsReasoningBack(vendor, baseURL, model string) bool {
	switch vendor {
	case "deepseek", "moonshot", "kimi", "zhipu":
		return true
	case "groq":
		return false
	}
	var h string
	if u, err := url.Parse(baseURL); err == nil {
		h = u.Hostname()
	}
	hostIs := func(d string) bool { return h == d || strings.HasSuffix(h, "."+d) }
	if hostIs("groq.com") {
		return false
	}
	for _, d := range []string{"deepseek.com", "moonshot.ai", "moonshot.cn", "z.ai", "bigmodel.cn"} {
		if hostIs(d) {
			return true
		}
	}
	m := strings.ToLower(model[strings.LastIndex(model, "/")+1:])
	return strings.HasPrefix(m, "kimi-") || strings.HasPrefix(m, "glm-")
}

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	MaxTokens           *int64        `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int64        `json:"max_completion_tokens,omitempty"`
	Temperature         *float64      `json:"temperature,omitempty"`
	TopP                *float64      `json:"top_p,omitempty"`
	Stop                []string      `json:"stop,omitempty"`
	Stream              bool          `json:"stream,omitempty"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	Tools             []chatTool `json:"tools,omitempty"`
	ToolChoice        any        `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool      `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   string     `json:"reasoning_effort,omitempty"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content"` // string or []chatPart
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type chatPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

// chatBody converts a neutral request into a Chat Completions body for
// model. echoReasoning sends earlier unsigned thinking back as
// reasoning_content, which only some vendors want.
//
// Text is sent byte for byte. Separate text blocks of one turn are joined
// with "\n" (several of these vendors accept only a string for system and
// assistant content), and a block that is only whitespace is skipped
// (Anthropic itself rejects one).
func chatBody(in *llm.Request, model string, echoReasoning bool) ([]byte, error) {
	sys := systemText(in.System)
	if len(in.Messages) == 0 && sys == "" {
		return nil, &llm.RequestError{Err: errors.New("messages: at least one message is required")}
	}

	req := chatRequest{Model: model, Stop: in.StopSequences, Stream: in.Stream}
	if sys != "" {
		req.Messages = append(req.Messages, chatMessage{Role: "system", Content: sys})
	}
	for _, m := range in.Messages {
		if m.Role == "assistant" {
			if msg, ok := assistantMessage(m.Content, echoReasoning); ok {
				req.Messages = append(req.Messages, msg)
			}
			continue
		}
		// Everything else is user, including the role:"system" entries Claude
		// Code appends mid-conversation: not every vendor accepts a late system.
		req.Messages = append(req.Messages, userMessages(m.Content)...)
	}
	if len(req.Messages) == 0 {
		req.Messages = []chatMessage{{Role: "user", Content: ""}}
	}

	// max_tokens is the field every compatible vendor accepts; only OpenAI's
	// reasoning families reject it (and non-default sampling) on Chat.
	if openaiReasoningModel(model) {
		// Their only accepted temperature/top_p is the default 1: that value is
		// omitted, any other is refused rather than silently changed.
		for name, v := range map[string]*float64{"temperature": in.Temperature, "top_p": in.TopP} {
			if v != nil && *v != 1 {
				return nil, &llm.ErrUnsupported{Field: fmt.Sprintf("%s (model %s accepts only the default 1)", name, model)}
			}
		}
		req.MaxCompletionTokens = in.MaxTokens
		req.ReasoningEffort = reasoningEffort(in.Effort)
	} else {
		req.MaxTokens, req.Temperature, req.TopP = in.MaxTokens, in.Temperature, in.TopP
	}
	if in.Stream {
		// Without include_usage a Chat stream reports no tokens at all.
		req.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{true}
	}

	for _, t := range in.Tools {
		// Vendor-defined tools never reach here (the engine refuses them);
		// skip them anyway rather than send a schema-less function.
		if (t.Type != "" && t.Type != "custom") || t.Name == "" {
			continue
		}
		ct := chatTool{Type: "function"}
		ct.Function.Name, ct.Function.Description = t.Name, t.Description
		ct.Function.Parameters = toolParameters(t.InputSchema)
		req.Tools = append(req.Tools, ct)
	}
	// Vendors reject tool_choice/parallel_tool_calls without tools.
	if len(req.Tools) > 0 && in.ToolChoice != nil {
		switch in.ToolChoice.Type {
		case "auto", "none":
			req.ToolChoice = in.ToolChoice.Type
		case "any":
			req.ToolChoice = "required"
		case "tool":
			req.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": in.ToolChoice.Name}}
		}
		if in.ToolChoice.DisableParallelToolUse {
			f := false
			req.ParallelToolCalls = &f
		}
	}
	return json.Marshal(req)
}

// toolParameters returns the tool schema without its top-level $schema: Claude
// Code always sends one, it means nothing to a Chat vendor, and vendors that map
// parameters onto an OpenAPI subset (Gemini) reject unknown keywords.
func toolParameters(schema json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(schema, &m) != nil || m == nil {
		return emptySchema
	}
	if _, ok := m["$schema"]; !ok {
		return schema
	}
	delete(m, "$schema")
	b, _ := json.Marshal(m)
	return b
}

// openaiReasoningModel reports OpenAI's o-series and gpt-5+ families (also behind
// a vendor prefix such as "openai/"), which take max_completion_tokens and
// reasoning_effort and reject temperature/top_p.
func openaiReasoningModel(model string) bool {
	m := strings.ToLower(model[strings.LastIndex(model, "/")+1:])
	if len(m) >= 2 && m[0] == 'o' && m[1] >= '1' && m[1] <= '9' {
		return true
	}
	return len(m) > 4 && strings.HasPrefix(m, "gpt-") && m[4] >= '5' && m[4] <= '9'
}

// reasoningEffort maps the neutral effort onto the reasoning_effort values
// every OpenAI reasoning model accepts.
func reasoningEffort(e string) string {
	switch e {
	case "low", "medium", "high":
		return e
	case "xhigh", "max":
		return "high"
	}
	return ""
}

// blank reports a text that carries nothing.
func blank(s string) bool { return strings.TrimSpace(s) == "" }

// systemText joins the system prompt's text blocks.
func systemText(blocks []llm.Block) string {
	var parts []string
	for _, b := range blocks {
		// Claude Code's attribution block is client metadata, not instructions,
		// and leading the prompt it would defeat vendors' prefix caching.
		if blank(b.Text) || strings.HasPrefix(strings.TrimSpace(b.Text), "x-anthropic-billing-header:") {
			continue
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n")
}

func assistantMessage(blocks []llm.Block, echoReasoning bool) (chatMessage, bool) {
	var texts, reasoning []string
	var calls []chatToolCall
	for _, b := range blocks {
		switch b.Type {
		case llm.BlockText:
			if !blank(b.Text) {
				texts = append(texts, b.Text)
			}
		case llm.BlockToolUse:
			if b.ID == "" || b.Name == "" {
				continue
			}
			c := chatToolCall{ID: b.ID, Type: "function"}
			c.Function.Name, c.Function.Arguments = b.Name, toolArguments(b.Input)
			calls = append(calls, c)
		case llm.BlockThinking:
			// Unsigned thinking is what this Provider produced from a vendor's
			// reasoning; DeepSeek, Kimi and GLM want it back on later turns.
			// A signed block came from a real Claude turn: not this vendor's.
			if !blank(b.Thinking) && b.Signature == "" && echoReasoning {
				reasoning = append(reasoning, b.Thinking)
			}
		}
		// redacted_thinking: opaque to every other vendor.
	}
	if len(texts) == 0 && len(calls) == 0 {
		return chatMessage{}, false
	}
	return chatMessage{
		Role:             "assistant",
		Content:          strings.Join(texts, "\n"),
		ToolCalls:        calls,
		ReasoningContent: strings.Join(reasoning, "\n"),
	}, true
}

// toolArguments renders a history tool_use input as Chat arguments: a JSON
// string's value, else the object's own JSON ("{}" for none).
func toolArguments(input json.RawMessage) string {
	var s string
	if json.Unmarshal(input, &s) == nil {
		return s
	}
	if !isJSONObject(input) {
		return "{}"
	}
	return string(input)
}

// userMessages emits one role:"tool" message per tool_result, in block order,
// and then the remaining text and images as one user message: Chat requires tool
// results to follow the assistant tool_calls directly.
func userMessages(blocks []llm.Block) []chatMessage {
	var msgs []chatMessage
	var parts []chatPart
	for _, b := range blocks {
		switch b.Type {
		case llm.BlockImage:
			if p, ok := imagePart(b); ok {
				parts = append(parts, p)
			}
		case llm.BlockDocument:
			if b.Source != nil && b.Source.Type == "text" {
				parts = appendText(parts, b.Source.Data)
			}
		case llm.BlockToolResult:
			if b.ToolUseID == "" {
				continue
			}
			text, images := toolResultContent(b)
			msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: text})
			parts = append(parts, images...)
		default:
			parts = appendText(parts, b.Text) // text, and other blocks that carry text
		}
	}
	if len(parts) == 0 {
		return msgs
	}
	var texts []string
	for _, p := range parts {
		if p.Type != "text" {
			return append(msgs, chatMessage{Role: "user", Content: parts})
		}
		texts = append(texts, p.Text)
	}
	return append(msgs, chatMessage{Role: "user", Content: strings.Join(texts, "\n")})
}

func appendText(parts []chatPart, s string) []chatPart {
	if blank(s) {
		return parts
	}
	return append(parts, chatPart{Type: "text", Text: s})
}

func imagePart(b llm.Block) (chatPart, bool) {
	if b.Source == nil {
		return chatPart{}, false
	}
	url := b.Source.URL
	if url == "" {
		media, data := b.Source.MediaType, strings.TrimSpace(b.Source.Data)
		if media == "" {
			media = "image/png"
		}
		if data == "" || !strings.HasPrefix(media, "image/") {
			return chatPart{}, false
		}
		url = "data:" + media + ";base64," + data
	}
	p := chatPart{Type: "image_url"}
	p.ImageURL = &struct {
		URL string `json:"url"`
	}{url}
	return p, true
}

// toolResultContent flattens a tool_result to the string Chat tool messages
// carry. Images cannot ride in a tool message, so they are returned to be sent
// in the user message that follows (Claude Code's Read tool returns images).
func toolResultContent(b llm.Block) (string, []chatPart) {
	var texts, tools []string
	var images []chatPart
	for _, c := range b.Content {
		switch c.Type {
		case llm.BlockImage:
			if p, ok := imagePart(c); ok {
				images = append(images, p)
			}
		case llm.BlockToolReference:
			tools = append(tools, c.ToolName)
		default:
			// One text block (a plain-string result) is kept as is, even blank.
			if len(b.Content) == 1 || !blank(c.Text) {
				texts = append(texts, c.Text)
			}
		}
	}
	if len(tools) > 0 {
		texts = append(texts, "Tools loaded: "+strings.Join(tools, ", "))
	}
	text := strings.Join(texts, "\n")
	if text == "" && len(images) > 0 {
		text = "(image attached in the next message)"
	}
	if b.IsError {
		text = "Error: " + text // Chat has no is_error flag
	}
	return text, images
}

func isJSONObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}
