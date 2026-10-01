// Package anthropic is the Anthropic Messages API adapter for pkg/llm: the
// "anthropic" Dialect (a client speaking /v1/messages) and the "anthropic"
// Provider (a vendor speaking it). Both register from init(); blank-import
// the package to use them.
package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// Version is the anthropic-version header the Provider sends.
const Version = "2023-06-01"

// --- requests ---

// parseRequest decodes an Anthropic Messages request body. It is lenient
// where clients are sloppy (content may be a string, a block array or one
// block object) and strict where the shape is wrong (content must be one of
// those).
func parseRequest(body []byte) (*llm.Request, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		if err == nil {
			err = errors.New("not a JSON object")
		}
		return nil, fmt.Errorf("invalid request body: %v", err)
	}
	req := &llm.Request{}
	take := func(key string, dst any) error {
		raw, ok := top[key]
		if !ok {
			return nil
		}
		delete(top, key)
		if isNull(raw) {
			return nil
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("%s: %v", key, err)
		}
		return nil
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	var system json.RawMessage
	var tools []json.RawMessage
	var outputConfig map[string]json.RawMessage
	var thinking map[string]json.RawMessage
	for _, f := range []struct {
		key string
		dst any
	}{
		{"model", &req.Model}, {"messages", &messages}, {"system", &system}, {"tools", &tools},
		{"tool_choice", &req.ToolChoice}, {"max_tokens", &req.MaxTokens}, {"temperature", &req.Temperature},
		{"top_p", &req.TopP}, {"top_k", &req.TopK}, {"stop_sequences", &req.StopSequences},
		{"stream", &req.Stream}, {"thinking", &thinking}, {"output_config", &outputConfig},
	} {
		if err := take(f.key, f.dst); err != nil {
			return nil, err
		}
	}
	if raw, ok := top["metadata"]; ok {
		delete(top, "metadata")
		if !isNull(raw) {
			req.Metadata = raw
		}
	}
	if len(system) > 0 {
		blocks, err := parseContent(system)
		if err != nil {
			return nil, fmt.Errorf("system: %v", err)
		}
		req.System = blocks
	}
	for i, m := range messages {
		blocks, err := parseContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages.%d.content: %v", i, err)
		}
		req.Messages = append(req.Messages, llm.Message{Role: m.Role, Content: blocks})
	}
	for i, raw := range tools {
		t, err := parseTool(raw)
		if err != nil {
			return nil, fmt.Errorf("tools.%d: %v", i, err)
		}
		req.Tools = append(req.Tools, t)
	}
	if thinking != nil {
		th := &llm.Thinking{}
		_ = json.Unmarshal(thinking["type"], &th.Type)
		_ = json.Unmarshal(thinking["budget_tokens"], &th.BudgetTokens)
		delete(thinking, "type")
		delete(thinking, "budget_tokens")
		th.Extra = nonEmpty(thinking)
		req.Thinking = th
	}
	if outputConfig != nil {
		_ = json.Unmarshal(outputConfig["effort"], &req.Effort)
		delete(outputConfig, "effort")
		if len(outputConfig) > 0 {
			top["output_config"], _ = json.Marshal(outputConfig)
		}
	}
	req.Extra = nonEmpty(top)
	return req, nil
}

// renderRequest is parseRequest's inverse: the Anthropic body for req, with
// model replaced.
func renderRequest(req *llm.Request, model string) ([]byte, error) {
	out := map[string]any{}
	for k, v := range req.Extra {
		out[k] = v
	}
	out["model"] = model
	msgs := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]any{"role": m.Role, "content": renderBlocks(m.Content)})
	}
	out["messages"] = msgs
	if len(req.System) > 0 {
		out["system"] = renderBlocks(req.System)
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, renderTool(t))
		}
		out["tools"] = tools
	}
	set := func(k string, v any, ok bool) {
		if ok {
			out[k] = v
		}
	}
	set("tool_choice", req.ToolChoice, req.ToolChoice != nil)
	set("max_tokens", req.MaxTokens, req.MaxTokens != nil)
	set("temperature", req.Temperature, req.Temperature != nil)
	set("top_p", req.TopP, req.TopP != nil)
	set("top_k", req.TopK, req.TopK != nil)
	set("stop_sequences", req.StopSequences, len(req.StopSequences) > 0)
	set("stream", true, req.Stream)
	set("metadata", req.Metadata, len(req.Metadata) > 0)
	if th := req.Thinking; th != nil {
		m := map[string]any{"type": th.Type}
		for k, v := range th.Extra {
			m[k] = v
		}
		if th.BudgetTokens > 0 {
			m["budget_tokens"] = th.BudgetTokens
		}
		out["thinking"] = m
	}
	if req.Effort != "" {
		oc := map[string]any{}
		if raw, ok := req.Extra["output_config"]; ok {
			_ = json.Unmarshal(raw, &oc)
		}
		oc["effort"] = req.Effort
		out["output_config"] = oc
	}
	return json.Marshal(out)
}

func parseTool(raw json.RawMessage) (llm.Tool, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return llm.Tool{}, errors.New("must be an object")
	}
	var t llm.Tool
	for key, dst := range map[string]*string{"type": &t.Type, "name": &t.Name, "description": &t.Description} {
		if v, ok := m[key]; ok {
			_ = json.Unmarshal(v, dst)
			delete(m, key)
		}
	}
	if v, ok := m["input_schema"]; ok {
		if !isNull(v) {
			t.InputSchema = v
		}
		delete(m, "input_schema")
	}
	t.Extra = nonEmpty(m)
	return t, nil
}

func renderTool(t llm.Tool) map[string]any {
	m := map[string]any{"name": t.Name}
	for k, v := range t.Extra {
		m[k] = v
	}
	if t.Type != "" {
		m["type"] = t.Type
	}
	if t.Description != "" {
		m["description"] = t.Description
	}
	if len(t.InputSchema) > 0 {
		m["input_schema"] = t.InputSchema
	}
	return m
}

// --- content blocks ---

// parseContent normalizes message or system content: a string is one text
// block, an array is blocks, one object is one block.
func parseContent(raw json.RawMessage) ([]llm.Block, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []llm.Block{{Type: llm.BlockText, Text: s}}, nil
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		out := make([]llm.Block, 0, len(arr))
		for _, b := range arr {
			blk, err := parseBlock(b)
			if err != nil {
				return nil, err
			}
			out = append(out, blk)
		}
		return out, nil
	}
	blk, err := parseBlock(raw)
	if err != nil {
		return nil, errors.New("content must be a string or an array of content blocks")
	}
	return []llm.Block{blk}, nil
}

// neutralKinds are the block types parsed field by field; any other type is
// kept whole in Block.Raw (Text is still read, for adapters that can use it).
var neutralKinds = map[llm.BlockType]bool{
	llm.BlockText: true, llm.BlockImage: true, llm.BlockDocument: true, llm.BlockToolUse: true,
	llm.BlockToolResult: true, llm.BlockThinking: true, llm.BlockRedactedThinking: true, llm.BlockToolReference: true,
}

func parseBlock(raw json.RawMessage) (llm.Block, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return llm.Block{}, errors.New("content must be a string or an array of content blocks")
	}
	var b llm.Block
	str := func(key string, dst *string) {
		if v, ok := m[key]; ok {
			_ = json.Unmarshal(v, dst)
			delete(m, key)
		}
	}
	var typ string
	str("type", &typ)
	b.Type = llm.BlockType(typ)
	if !neutralKinds[b.Type] {
		_ = json.Unmarshal(m["text"], &b.Text)
		b.Raw = raw
		return b, nil
	}
	switch b.Type {
	case llm.BlockText:
		str("text", &b.Text)
	case llm.BlockImage, llm.BlockDocument:
		if v, ok := m["source"]; ok {
			var src llm.Source
			if json.Unmarshal(v, &src) == nil {
				b.Source = &src
			}
			delete(m, "source")
		}
	case llm.BlockToolUse:
		str("id", &b.ID)
		str("name", &b.Name)
		if v, ok := m["input"]; ok {
			if !isNull(v) {
				b.Input = v
			}
			delete(m, "input")
		}
	case llm.BlockToolResult:
		str("tool_use_id", &b.ToolUseID)
		if v, ok := m["is_error"]; ok {
			_ = json.Unmarshal(v, &b.IsError)
			delete(m, "is_error")
		}
		if v, ok := m["content"]; ok {
			delete(m, "content")
			var s string
			var arr []json.RawMessage
			switch {
			case isNull(v):
			case json.Unmarshal(v, &s) == nil:
				b.Content = []llm.Block{{Type: llm.BlockText, Text: s}}
			case json.Unmarshal(v, &arr) == nil:
				blocks, err := parseContent(v)
				if err != nil {
					return llm.Block{}, err
				}
				b.Content = blocks
			default:
				// A scalar or a bare object: keep its JSON text.
				b.Content = []llm.Block{{Type: llm.BlockText, Text: string(v)}}
			}
		}
	case llm.BlockThinking:
		str("thinking", &b.Thinking)
		str("signature", &b.Signature)
	case llm.BlockRedactedThinking:
		str("data", &b.Data)
	case llm.BlockToolReference:
		str("tool_name", &b.ToolName)
	}
	b.Extra = nonEmpty(m)
	return b, nil
}

func renderBlocks(bs []llm.Block) []any {
	out := make([]any, 0, len(bs))
	for _, b := range bs {
		out = append(out, renderBlock(b))
	}
	return out
}

// renderBlock is parseBlock's inverse. A content_block_start of the stream
// uses it too, so an empty text/thinking field is still written.
func renderBlock(b llm.Block) any {
	if len(b.Raw) > 0 {
		return b.Raw
	}
	m := map[string]any{}
	for k, v := range b.Extra {
		m[k] = v
	}
	m["type"] = b.Type
	switch b.Type {
	case llm.BlockText:
		m["text"] = b.Text
	case llm.BlockImage, llm.BlockDocument:
		if b.Source != nil {
			m["source"] = b.Source
		}
	case llm.BlockToolUse:
		m["id"], m["name"] = b.ID, b.Name
		m["input"] = b.Input
		if len(b.Input) == 0 {
			m["input"] = json.RawMessage(`{}`)
		}
	case llm.BlockToolResult:
		m["tool_use_id"] = b.ToolUseID
		if len(b.Content) > 0 {
			m["content"] = renderBlocks(b.Content)
		}
		if b.IsError {
			m["is_error"] = true
		}
	case llm.BlockThinking:
		m["thinking"], m["signature"] = b.Thinking, b.Signature
	case llm.BlockRedactedThinking:
		m["data"] = b.Data
	case llm.BlockToolReference:
		m["tool_name"] = b.ToolName
	}
	return m
}

// --- responses ---

type wireUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheCreation            struct {
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	ServerToolUse struct {
		WebSearchRequests int64 `json:"web_search_requests"`
	} `json:"server_tool_use"`
}

func (u wireUsage) neutral() llm.Usage {
	return llm.Usage{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens, CacheWrite1hTokens: u.CacheCreation.Ephemeral1h,
		WebSearchRequests: u.ServerToolUse.WebSearchRequests,
	}
}

// renderUsage writes input, output and cache-read tokens always, and the
// cache-write and server-tool counts only when non-zero.
func renderUsage(u llm.Usage) map[string]any {
	m := map[string]any{
		"input_tokens":            u.InputTokens,
		"output_tokens":           u.OutputTokens,
		"cache_read_input_tokens": u.CacheReadTokens,
	}
	if u.CacheWriteTokens > 0 {
		m["cache_creation_input_tokens"] = u.CacheWriteTokens
		m["cache_creation"] = map[string]int64{
			"ephemeral_5m_input_tokens": u.CacheWriteTokens - u.CacheWrite1hTokens,
			"ephemeral_1h_input_tokens": u.CacheWrite1hTokens,
		}
	}
	if u.WebSearchRequests > 0 {
		m["server_tool_use"] = map[string]int64{"web_search_requests": u.WebSearchRequests}
	}
	return m
}

type wireMessage struct {
	ID           string            `json:"id"`
	Model        string            `json:"model"`
	Role         string            `json:"role"`
	Content      []json.RawMessage `json:"content"`
	StopReason   string            `json:"stop_reason"`
	StopSequence string            `json:"stop_sequence"`
	Usage        wireUsage         `json:"usage"`
}

func (w wireMessage) neutral() (*llm.Response, error) {
	r := &llm.Response{ID: w.ID, Model: w.Model, Role: firstNonEmpty(w.Role, "assistant"), StopReason: w.StopReason,
		StopSequence: w.StopSequence, Usage: w.Usage.neutral(), Content: []llm.Block{}}
	for _, raw := range w.Content {
		b, err := parseBlock(raw)
		if err != nil {
			return nil, err
		}
		r.Content = append(r.Content, b)
	}
	return r, nil
}

func renderMessage(r *llm.Response) map[string]any {
	var stopReason, stopSeq any
	if r.StopReason != "" {
		stopReason = r.StopReason
	}
	if r.StopSequence != "" {
		stopSeq = r.StopSequence
	}
	return map[string]any{
		"id":            r.ID,
		"type":          "message",
		"role":          firstNonEmpty(r.Role, "assistant"),
		"model":         r.Model,
		"content":       renderBlocks(r.Content),
		"stop_reason":   stopReason,
		"stop_sequence": stopSeq,
		"usage":         renderUsage(r.Usage),
	}
}

// --- errors ---

// renderError is the Anthropic error envelope; request_id only when set. A
// context-window overflow is worded the way Claude Code recognizes (it
// compacts and retries only on "prompt is too long").
func renderError(e *llm.Error) []byte {
	msg := e.Message
	if e.Code == llm.CodeContextLength && !strings.HasPrefix(strings.ToLower(msg), "prompt is too long") {
		msg = "prompt is too long: " + msg
	}
	body := map[string]any{
		"type":  "error",
		"error": map[string]string{"type": firstNonEmpty(e.Type, llm.ErrorTypeAPI), "message": msg},
	}
	if e.RequestID != "" {
		body["request_id"] = e.RequestID
	}
	b, _ := json.Marshal(body)
	return b
}

func parseError(status int, body []byte) *llm.Error {
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	e := &llm.Error{Status: status, Type: llm.ErrorTypeForStatus(status)}
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		e.Type, e.Message = firstNonEmpty(env.Error.Type, e.Type), env.Error.Message
	} else if e.Message = strings.TrimSpace(string(body)); len(e.Message) > 1024 {
		e.Message = e.Message[:1024]
	}
	if strings.HasPrefix(strings.ToLower(e.Message), "prompt is too long") {
		e.Code = llm.CodeContextLength
	}
	return e
}

// --- helpers ---

func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

func nonEmpty(m map[string]json.RawMessage) map[string]json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	return m
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
