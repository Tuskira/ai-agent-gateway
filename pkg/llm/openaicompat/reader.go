package openaicompat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/sse"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/strictjson"
)

// ReaderName is the registry name of ChatReader.
const ReaderName = "openai_chat"

// ChatReader is the llm.Reader for the OpenAI Chat Completions wire: a
// client's /chat/completions request, and the response or stream it got
// back (read by this package's Provider parsers, the wire being the same on
// both sides).
//
// A request is read as the inverse of what the Provider sends. Leading
// system and developer messages are the system prompt; a later one is a
// "system" turn. Assistant tool_calls are tool_use blocks, and a run of
// consecutive tool messages is one user turn of tool_result blocks.
// Message fields with no neutral slot (name, refusal, ...) ride in the
// Extra of the message's first block, and unknown top-level fields in
// Request.Extra. Content parts: text, image_url (an image block; a base64
// data URL becomes a base64 source) and file (a document block); any other
// part is kept whole in Raw.
type ChatReader struct{}

func (ChatReader) Name() string { return ReaderName }

// DecodeRequest reads a Chat Completions request body strictly.
func (ChatReader) DecodeRequest(body []byte) (*llm.Request, error) {
	if err := strictjson.Check(body); err != nil {
		return nil, &llm.RequestError{Err: fmt.Errorf("invalid request body: %v", err)}
	}
	req, err := readRequest(body)
	if err != nil {
		return nil, &llm.RequestError{Err: err}
	}
	return req, nil
}

// DecodeResponse reads a non-stream Chat Completions response body as the
// Provider does (the first choice), except that the id and model are the
// body's own: the Provider makes up an id when there is none, and leaves the
// model to the engine.
func (ChatReader) DecodeResponse(body []byte) (*llm.Response, error) {
	if len(body) > llm.MaxBodyBytes {
		return nil, llm.ErrFrameTooLarge
	}
	if err := strictjson.Check(body); err != nil {
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	top, err := object(body)
	if err != nil {
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	out, err := parseResponse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	out.ID, out.Model = jsonString(top["id"]), jsonString(top["model"])
	return out, nil
}

// NewResponseDecoder reads Chat Completions SSE as the Provider's stream
// decoder does, over frames that passed the strict check.
func (ChatReader) NewResponseDecoder(r io.Reader) llm.StreamDecoder {
	return Provider{}.NewStreamDecoder(sse.Checked(r, llm.MaxFrameBytes, llm.ErrFrameTooLarge, strictjson.Check))
}

// object decodes a JSON object; anything else is an error.
func object(raw []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		if err == nil {
			err = errors.New("not a JSON object")
		}
		return nil, err
	}
	return m, nil
}

// take moves m[key] into dst (a null or missing value leaves dst alone).
func take(m map[string]json.RawMessage, key string, dst any) error {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	delete(m, key)
	if isNull(raw) {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%s: %v", key, err)
	}
	return nil
}

func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

func nonEmpty(m map[string]json.RawMessage) map[string]json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	return m
}

func readRequest(body []byte) (*llm.Request, error) {
	top, err := object(body)
	if err != nil {
		return nil, fmt.Errorf("invalid request body: %v", err)
	}
	req := &llm.Request{}
	var messages, tools []json.RawMessage
	var toolChoice, stop json.RawMessage
	var maxTokens, maxCompletion *int64
	var parallel *bool
	for _, f := range []struct {
		key string
		dst any
	}{
		{"model", &req.Model}, {"messages", &messages}, {"tools", &tools}, {"tool_choice", &toolChoice},
		{"max_tokens", &maxTokens}, {"max_completion_tokens", &maxCompletion}, {"temperature", &req.Temperature},
		{"top_p", &req.TopP}, {"stop", &stop}, {"stream", &req.Stream}, {"parallel_tool_calls", &parallel},
		{"reasoning_effort", &req.Effort},
	} {
		if err := take(top, f.key, f.dst); err != nil {
			return nil, err
		}
	}
	if maxTokens != nil && maxCompletion != nil {
		return nil, errors.New("max_tokens and max_completion_tokens: only one may be set")
	}
	req.MaxTokens = maxCompletion
	if maxTokens != nil {
		req.MaxTokens = maxTokens
	}
	if len(stop) > 0 {
		var s string
		if json.Unmarshal(stop, &s) == nil {
			req.StopSequences = []string{s}
		} else if err := json.Unmarshal(stop, &req.StopSequences); err != nil {
			return nil, errors.New("stop: must be a string or an array of strings")
		}
	}
	if err := readMessages(req, messages); err != nil {
		return nil, err
	}
	for i, raw := range tools {
		t, err := readTool(raw)
		if err != nil {
			return nil, fmt.Errorf("tools.%d: %v", i, err)
		}
		req.Tools = append(req.Tools, t)
	}
	if len(toolChoice) > 0 {
		tc, err := readToolChoice(toolChoice)
		if err != nil {
			return nil, fmt.Errorf("tool_choice: %v", err)
		}
		req.ToolChoice = tc
	}
	if parallel != nil && !*parallel {
		if req.ToolChoice == nil {
			req.ToolChoice = &llm.ToolChoice{Type: "auto"}
		}
		req.ToolChoice.DisableParallelToolUse = true
	}
	req.Extra = nonEmpty(top)
	return req, nil
}

// readMessages fills req.System and req.Messages.
func readMessages(req *llm.Request, messages []json.RawMessage) error {
	leading := true // still in the leading system/developer messages
	for i, raw := range messages {
		m, err := object(raw)
		if err != nil {
			return fmt.Errorf("messages.%d: must be an object", i)
		}
		var role string
		if err := take(m, "role", &role); err != nil {
			return fmt.Errorf("messages.%d.%v", i, err)
		}
		blocks, err := readMessage(role, m)
		if err != nil {
			return fmt.Errorf("messages.%d.%v", i, err)
		}
		// Fields with no neutral slot ride on the message's first block.
		if extra := nonEmpty(m); extra != nil {
			if len(blocks) == 0 {
				blocks = []llm.Block{{Type: llm.BlockText}}
			}
			if blocks[0].Extra == nil {
				blocks[0].Extra = map[string]json.RawMessage{}
			}
			for k, v := range extra {
				blocks[0].Extra[k] = v
			}
		}
		switch role {
		case "system", "developer":
			if leading {
				req.System = append(req.System, blocks...)
				continue
			}
			req.Messages = append(req.Messages, llm.Message{Role: "system", Content: blocks})
		case "tool":
			// Consecutive tool results answer one assistant turn: one user turn.
			if n := len(req.Messages); n > 0 && !leading && isToolResults(req.Messages[n-1]) {
				req.Messages[n-1].Content = append(req.Messages[n-1].Content, blocks...)
				break
			}
			req.Messages = append(req.Messages, llm.Message{Role: "user", Content: blocks})
		default:
			req.Messages = append(req.Messages, llm.Message{Role: role, Content: blocks})
		}
		leading = false
	}
	return nil
}

func isToolResults(m llm.Message) bool {
	for _, b := range m.Content {
		if b.Type != llm.BlockToolResult {
			return false
		}
	}
	return m.Role == "user" && len(m.Content) > 0
}

// readMessage reads one message's blocks, removing the fields it reads from
// m (role already removed). The error is prefixed by the caller with the
// message's path.
func readMessage(role string, m map[string]json.RawMessage) ([]llm.Block, error) {
	content, hasContent := m["content"]
	delete(m, "content")
	hasContent = hasContent && !isNull(content)
	switch role {
	case "system", "developer", "user":
		if !hasContent {
			return nil, errors.New("content: required")
		}
		return readContent(content)
	case "assistant":
		var blocks []llm.Block
		var reasoning string
		if err := take(m, "reasoning_content", &reasoning); err != nil {
			return nil, err
		}
		if reasoning != "" {
			blocks = append(blocks, llm.Block{Type: llm.BlockThinking, Thinking: reasoning})
		}
		if hasContent {
			cs, err := readContent(content)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, cs...)
		}
		var calls []json.RawMessage
		if err := take(m, "tool_calls", &calls); err != nil {
			return nil, err
		}
		for j, raw := range calls {
			b, err := readToolCall(raw)
			if err != nil {
				return nil, fmt.Errorf("tool_calls.%d: %v", j, err)
			}
			blocks = append(blocks, b)
		}
		return blocks, nil
	case "tool":
		var id string
		if err := take(m, "tool_call_id", &id); err != nil {
			return nil, err
		}
		if id == "" {
			return nil, errors.New("tool_call_id: required")
		}
		b := llm.Block{Type: llm.BlockToolResult, ToolUseID: id}
		if hasContent {
			cs, err := readContent(content)
			if err != nil {
				return nil, err
			}
			b.Content = cs
		}
		return []llm.Block{b}, nil
	}
	return nil, fmt.Errorf("role: unknown role %q", role)
}

// readContent reads message content: a string is one text block, an array
// is parts.
func readContent(raw json.RawMessage) ([]llm.Block, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []llm.Block{{Type: llm.BlockText, Text: s}}, nil
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil, errors.New("content: must be a string or an array of content parts")
	}
	out := make([]llm.Block, 0, len(parts))
	for i, p := range parts {
		b, err := readPart(p)
		if err != nil {
			return nil, fmt.Errorf("content.%d: %v", i, err)
		}
		out = append(out, b)
	}
	return out, nil
}

func readPart(raw json.RawMessage) (llm.Block, error) {
	p, err := object(raw)
	if err != nil {
		return llm.Block{}, errors.New("must be an object")
	}
	var typ string
	if err := take(p, "type", &typ); err != nil {
		return llm.Block{}, err
	}
	switch typ {
	case "text":
		b := llm.Block{Type: llm.BlockText}
		if err := take(p, "text", &b.Text); err != nil {
			return llm.Block{}, err
		}
		b.Extra = nonEmpty(p)
		return b, nil
	case "image_url":
		var img map[string]json.RawMessage
		if err := take(p, "image_url", &img); err != nil {
			return llm.Block{}, err
		}
		var url string
		if err := take(img, "url", &url); err != nil {
			return llm.Block{}, fmt.Errorf("image_url.%v", err)
		}
		if url == "" {
			return llm.Block{}, errors.New("image_url.url: required")
		}
		for k, v := range img { // detail
			p[k] = v
		}
		return llm.Block{Type: llm.BlockImage, Source: dataURLSource(url), Extra: nonEmpty(p)}, nil
	case "file":
		var file map[string]json.RawMessage
		if err := take(p, "file", &file); err != nil {
			return llm.Block{}, err
		}
		var id, data string
		if err := take(file, "file_id", &id); err != nil {
			return llm.Block{}, fmt.Errorf("file.%v", err)
		}
		if err := take(file, "file_data", &data); err != nil {
			return llm.Block{}, fmt.Errorf("file.%v", err)
		}
		var src *llm.Source
		switch {
		case id != "" && data != "":
			return llm.Block{}, errors.New("file: file_id and file_data: only one may be set")
		case id != "":
			src = &llm.Source{Type: "file", FileID: id}
		case strings.HasPrefix(data, "data:"):
			src = dataURLSource(data)
		case data != "":
			src = &llm.Source{Type: "base64", Data: data}
		default:
			return llm.Block{}, errors.New("file: file_id or file_data required")
		}
		for k, v := range file { // filename
			p[k] = v
		}
		return llm.Block{Type: llm.BlockDocument, Source: src, Extra: nonEmpty(p)}, nil
	case "":
		return llm.Block{}, errors.New("type: required")
	}
	// input_audio, refusal, ...: kept whole; the text a reader can use is
	// read when the part has one.
	b := llm.Block{Type: llm.BlockType(typ), Raw: raw}
	_ = json.Unmarshal(p["text"], &b.Text)
	if typ == "refusal" {
		_ = json.Unmarshal(p["refusal"], &b.Text)
	}
	return b, nil
}

// dataURLSource is imagePart's inverse: a base64 data URL is a base64
// source, any other URL a url source.
func dataURLSource(url string) *llm.Source {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		if media, data, ok := strings.Cut(rest, ";base64,"); ok {
			return &llm.Source{Type: "base64", MediaType: media, Data: data}
		}
	}
	return &llm.Source{Type: "url", URL: url}
}

func readToolCall(raw json.RawMessage) (llm.Block, error) {
	c, err := object(raw)
	if err != nil {
		return llm.Block{}, errors.New("must be an object")
	}
	b := llm.Block{Type: llm.BlockToolUse}
	var typ string
	var fn map[string]json.RawMessage
	for key, dst := range map[string]any{"id": &b.ID, "type": &typ, "function": &fn} {
		if err := take(c, key, dst); err != nil {
			return llm.Block{}, err
		}
	}
	if typ != "" && typ != "function" {
		return llm.Block{}, fmt.Errorf("type: unsupported tool call type %q", typ)
	}
	if b.ID == "" {
		return llm.Block{}, errors.New("id: required")
	}
	var args json.RawMessage
	if err := take(fn, "name", &b.Name); err != nil {
		return llm.Block{}, fmt.Errorf("function.%v", err)
	}
	if err := take(fn, "arguments", &args); err != nil {
		return llm.Block{}, fmt.Errorf("function.%v", err)
	}
	if b.Name == "" {
		return llm.Block{}, errors.New("function.name: required")
	}
	b.Input = toolInput(argString(args))
	for k, v := range fn {
		c[k] = v
	}
	b.Extra = nonEmpty(c)
	return b, nil
}

func readTool(raw json.RawMessage) (llm.Tool, error) {
	m, err := object(raw)
	if err != nil {
		return llm.Tool{}, errors.New("must be an object")
	}
	var t llm.Tool
	if err := take(m, "type", &t.Type); err != nil {
		return llm.Tool{}, err
	}
	switch t.Type {
	case "":
		return llm.Tool{}, errors.New("type: required")
	case "function":
	default:
		// Another kind (custom, ...): kept as is, named when it says how.
		if inner, err := object(m[t.Type]); err == nil {
			_ = json.Unmarshal(inner["name"], &t.Name)
		}
		t.Extra = nonEmpty(m)
		return t, nil
	}
	t.Type = ""
	var fn map[string]json.RawMessage
	if err := take(m, "function", &fn); err != nil {
		return llm.Tool{}, err
	}
	for key, dst := range map[string]any{"name": &t.Name, "description": &t.Description, "parameters": &t.InputSchema} {
		if err := take(fn, key, dst); err != nil {
			return llm.Tool{}, fmt.Errorf("function.%v", err)
		}
	}
	if t.Name == "" {
		return llm.Tool{}, errors.New("function.name: required")
	}
	for k, v := range fn { // strict, ...
		m[k] = v
	}
	t.Extra = nonEmpty(m)
	return t, nil
}

func readToolChoice(raw json.RawMessage) (*llm.ToolChoice, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto", "none":
			return &llm.ToolChoice{Type: s}, nil
		case "required":
			return &llm.ToolChoice{Type: "any"}, nil
		}
		return nil, fmt.Errorf("unsupported value %q", s)
	}
	obj, err := object(raw)
	if err != nil {
		return nil, fmt.Errorf("unsupported value %s", raw)
	}
	var typ string
	var fn map[string]json.RawMessage
	_ = take(obj, "type", &typ)
	_ = take(obj, "function", &fn)
	var name string
	_ = take(fn, "name", &name)
	if typ != "function" || name == "" || len(obj) > 0 || len(fn) > 0 {
		return nil, fmt.Errorf("unsupported value %s", raw)
	}
	return &llm.ToolChoice{Type: "tool", Name: name}, nil
}
