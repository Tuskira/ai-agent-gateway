package bedrock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/eventstream"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/strictjson"
)

// ConverseReader is the llm.Reader for Bedrock's Converse API: the body of
// POST /model/{modelId}/converse (or /converse-stream), the response, and
// the ConverseStream event stream.
//
// A request's system[].text blocks are the system prompt and its messages
// the conversation (roles user and assistant only). Content blocks are
// unions, one member each: text, image, document, toolUse, toolResult and
// reasoningContent map onto the neutral blocks; any other member
// (cachePoint, guardContent, video, ...) is kept whole in Raw under its
// member name. toolConfig.tools[].toolSpec are the tools and
// toolConfig.toolChoice the tool choice; inferenceConfig's maxTokens,
// temperature, topP and stopSequences are the sampling fields. Every other
// top-level field (additionalModelRequestFields, guardrailConfig, ...) rides
// in Request.Extra, as do the members of inferenceConfig and toolConfig the
// neutral form has no slot for, under their parent's name.
type ConverseReader struct{}

func (ConverseReader) Name() string { return ConverseName }

// DecodeRequest reads a Converse request body strictly.
func (ConverseReader) DecodeRequest(body []byte) (*llm.Request, error) {
	if err := strictjson.Check(body); err != nil {
		return nil, &llm.RequestError{Err: fmt.Errorf("invalid request body: %v", err)}
	}
	top, err := object(body)
	if err != nil {
		return nil, &llm.RequestError{Err: fmt.Errorf("invalid request body: %v", err)}
	}
	req, err := readConverse(top, false)
	if err != nil {
		return nil, &llm.RequestError{Err: err}
	}
	return req, nil
}

// DecodeResponse reads a Converse response body: output.message,
// stopReason and usage. Converse responses carry no id or model.
func (ConverseReader) DecodeResponse(body []byte) (*llm.Response, error) {
	top, err := checkedObject(body, "response body")
	if err != nil {
		return nil, err
	}
	out, err := readConverseResponse(top)
	if err != nil {
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	return out, nil
}

// NewResponseDecoder reads a ConverseStream event stream: messageStart,
// contentBlockStart/Delta/Stop, messageStop and metadata events, each
// frame's payload checked strictly. An exception frame is an error event.
func (ConverseReader) NewResponseDecoder(r io.Reader) llm.StreamDecoder {
	return &converseDecoder{src: frames{eventstream.NewReader(r)}, conv: newConverseState()}
}

// readConverse reads a Converse-shaped request object (a Nova invoke body
// too, with nova set: its inferenceConfig also spells the sampling fields
// max_new_tokens, top_p and top_k).
func readConverse(top map[string]json.RawMessage, nova bool) (*llm.Request, error) {
	req := &llm.Request{}
	var messages, system []json.RawMessage
	if err := take(top, "messages", &messages); err != nil {
		return nil, err
	}
	if err := take(top, "system", &system); err != nil {
		return nil, err
	}
	for i, raw := range system {
		b, err := readBlock(raw)
		if err != nil {
			return nil, fmt.Errorf("system.%d: %v", i, err)
		}
		req.System = append(req.System, b)
	}
	for i, raw := range messages {
		m, err := readMessage(raw)
		if err != nil {
			return nil, fmt.Errorf("messages.%d.%v", i, err)
		}
		req.Messages = append(req.Messages, m)
	}
	if req.Messages == nil {
		req.Messages = []llm.Message{}
	}
	if err := readInference(req, top, nova); err != nil {
		return nil, err
	}
	if err := readToolConfig(req, top); err != nil {
		return nil, err
	}
	req.Extra = nonEmpty(top)
	return req, nil
}

func readMessage(raw json.RawMessage) (llm.Message, error) {
	m, err := object(raw)
	if err != nil {
		return llm.Message{}, errors.New("message: must be an object")
	}
	var role string
	var content []json.RawMessage
	if err := take(m, "role", &role); err != nil {
		return llm.Message{}, err
	}
	switch role {
	case "user", "assistant":
	default:
		return llm.Message{}, fmt.Errorf("role: unknown role %q", role)
	}
	if err := take(m, "content", &content); err != nil {
		return llm.Message{}, err
	}
	if len(m) > 0 {
		for k := range m {
			return llm.Message{}, fmt.Errorf("%s: unknown message field", k)
		}
	}
	out := llm.Message{Role: role, Content: []llm.Block{}}
	for j, c := range content {
		b, err := readBlock(c)
		if err != nil {
			return llm.Message{}, fmt.Errorf("content.%d: %v", j, err)
		}
		out.Content = append(out.Content, b)
	}
	return out, nil
}

// readBlock reads one ContentBlock (or SystemContentBlock) union.
func readBlock(raw json.RawMessage) (llm.Block, error) {
	kind, v, err := member(raw)
	if err != nil {
		return llm.Block{}, err
	}
	switch kind {
	case "text":
		b := llm.Block{Type: llm.BlockText}
		if err := json.Unmarshal(v, &b.Text); err != nil {
			return llm.Block{}, fmt.Errorf("text: must be a string")
		}
		return b, nil
	case "image", "document":
		return readMedia(kind, v)
	case "toolUse":
		return readToolUse(v)
	case "toolResult":
		return readToolResult(v)
	case "reasoningContent":
		return readReasoning(v)
	}
	// cachePoint, guardContent, video, citationsContent, ...: kept whole.
	return llm.Block{Type: llm.BlockType(kind), Raw: raw}, nil
}

// documentTypes maps Converse document formats onto media types.
var documentTypes = map[string]string{
	"pdf":  "application/pdf",
	"csv":  "text/csv",
	"doc":  "application/msword",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xls":  "application/vnd.ms-excel",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"html": "text/html",
	"txt":  "text/plain",
	"md":   "text/markdown",
}

// readMedia reads an image or document block: {format, [name,] source}.
// The source is a union of bytes (base64), s3Location, and for documents
// text or content.
func readMedia(kind string, raw json.RawMessage) (llm.Block, error) {
	m, err := object(raw)
	if err != nil {
		return llm.Block{}, fmt.Errorf("%s: must be an object", kind)
	}
	b := llm.Block{Type: llm.BlockImage}
	if kind == "document" {
		b.Type = llm.BlockDocument
	}
	var format string
	if err := take(m, "format", &format); err != nil {
		return llm.Block{}, fmt.Errorf("%s.%v", kind, err)
	}
	var srcRaw json.RawMessage
	if err := take(m, "source", &srcRaw); err != nil || srcRaw == nil {
		if err == nil {
			err = errors.New("source: required")
		}
		return llm.Block{}, fmt.Errorf("%s.%v", kind, err)
	}
	media := ""
	if format != "" {
		if kind == "image" {
			media = "image/" + format
		} else if media = documentTypes[format]; media == "" {
			m["format"], _ = json.Marshal(format) // unknown: kept as sent
		}
	}
	src, v, err := member(srcRaw)
	if err != nil {
		return llm.Block{}, fmt.Errorf("%s.source: %v", kind, err)
	}
	b.Source = &llm.Source{MediaType: media}
	switch {
	case src == "bytes":
		b.Source.Type = "base64"
		err = json.Unmarshal(v, &b.Source.Data)
	case src == "s3Location":
		var loc map[string]json.RawMessage
		if loc, err = object(v); err == nil {
			b.Source.Type = "url"
			if err = take(loc, "uri", &b.Source.URL); err == nil && len(loc) > 0 {
				m["s3Location"], _ = json.Marshal(loc) // bucketOwner
			}
		}
	case src == "text" && kind == "document":
		b.Source.Type = "text"
		err = json.Unmarshal(v, &b.Source.Data)
	case src == "content" && kind == "document":
		b.Source.Type, b.Source.Content = "content", v
	default:
		return llm.Block{}, fmt.Errorf("%s.source: unsupported source %q", kind, src)
	}
	if err != nil {
		return llm.Block{}, fmt.Errorf("%s.source.%s: %v", kind, src, err)
	}
	b.Extra = nonEmpty(m) // name, context, citations, ...
	return b, nil
}

func readToolUse(raw json.RawMessage) (llm.Block, error) {
	m, err := object(raw)
	if err != nil {
		return llm.Block{}, errors.New("toolUse: must be an object")
	}
	b := llm.Block{Type: llm.BlockToolUse}
	for key, dst := range map[string]any{"toolUseId": &b.ID, "name": &b.Name, "input": &b.Input} {
		if err := take(m, key, dst); err != nil {
			return llm.Block{}, fmt.Errorf("toolUse.%v", err)
		}
	}
	if b.ID == "" {
		return llm.Block{}, errors.New("toolUse.toolUseId: required")
	}
	if b.Name == "" {
		return llm.Block{}, errors.New("toolUse.name: required")
	}
	if b.Input == nil {
		b.Input = json.RawMessage(`{}`)
	}
	b.Extra = nonEmpty(m)
	return b, nil
}

// readToolResult reads {toolUseId, content[], status}. Content members
// text, image and document map onto blocks; json is kept whole in Raw
// (type "json") with its JSON as Text.
func readToolResult(raw json.RawMessage) (llm.Block, error) {
	m, err := object(raw)
	if err != nil {
		return llm.Block{}, errors.New("toolResult: must be an object")
	}
	b := llm.Block{Type: llm.BlockToolResult}
	var content []json.RawMessage
	var status string
	for key, dst := range map[string]any{"toolUseId": &b.ToolUseID, "content": &content, "status": &status} {
		if err := take(m, key, dst); err != nil {
			return llm.Block{}, fmt.Errorf("toolResult.%v", err)
		}
	}
	if b.ToolUseID == "" {
		return llm.Block{}, errors.New("toolResult.toolUseId: required")
	}
	switch status {
	case "", "success":
	case "error":
		b.IsError = true
	default:
		return llm.Block{}, fmt.Errorf("toolResult.status: unknown status %q", status)
	}
	for i, c := range content {
		kind, v, err := member(c)
		if err != nil {
			return llm.Block{}, fmt.Errorf("toolResult.content.%d: %v", i, err)
		}
		var cb llm.Block
		if kind == "json" {
			var buf bytes.Buffer
			if err := json.Compact(&buf, v); err != nil {
				return llm.Block{}, fmt.Errorf("toolResult.content.%d.json: %v", i, err)
			}
			cb = llm.Block{Type: "json", Text: buf.String(), Raw: c}
		} else if cb, err = readBlock(c); err != nil {
			return llm.Block{}, fmt.Errorf("toolResult.content.%d: %v", i, err)
		}
		b.Content = append(b.Content, cb)
	}
	b.Extra = nonEmpty(m)
	return b, nil
}

// readReasoning reads reasoningContent: {reasoningText: {text, signature}}
// or {redactedContent: base64}.
func readReasoning(raw json.RawMessage) (llm.Block, error) {
	kind, v, err := member(raw)
	if err != nil {
		return llm.Block{}, fmt.Errorf("reasoningContent: %v", err)
	}
	switch kind {
	case "reasoningText":
		m, err := object(v)
		if err != nil {
			return llm.Block{}, errors.New("reasoningContent.reasoningText: must be an object")
		}
		b := llm.Block{Type: llm.BlockThinking}
		if err := take(m, "text", &b.Thinking); err != nil {
			return llm.Block{}, fmt.Errorf("reasoningContent.reasoningText.%v", err)
		}
		if err := take(m, "signature", &b.Signature); err != nil {
			return llm.Block{}, fmt.Errorf("reasoningContent.reasoningText.%v", err)
		}
		b.Extra = nonEmpty(m)
		return b, nil
	case "redactedContent":
		b := llm.Block{Type: llm.BlockRedactedThinking}
		if err := json.Unmarshal(v, &b.Data); err != nil {
			return llm.Block{}, errors.New("reasoningContent.redactedContent: must be a string")
		}
		return b, nil
	}
	return llm.Block{}, fmt.Errorf("reasoningContent: unsupported member %q", kind)
}

// readInference reads inferenceConfig; members with no neutral slot stay
// under Extra["inferenceConfig"].
func readInference(req *llm.Request, top map[string]json.RawMessage, nova bool) error {
	cfg, err := takeObject(top, "inferenceConfig")
	if err != nil || cfg == nil {
		return err
	}
	type pair struct {
		keys []string
		dst  any
	}
	pairs := []pair{
		{[]string{"maxTokens"}, &req.MaxTokens},
		{[]string{"temperature"}, &req.Temperature},
		{[]string{"topP"}, &req.TopP},
		{[]string{"stopSequences"}, &req.StopSequences},
	}
	if nova {
		pairs[0].keys = append(pairs[0].keys, "max_new_tokens")
		pairs[2].keys = append(pairs[2].keys, "top_p")
		pairs = append(pairs, pair{[]string{"top_k", "topK"}, &req.TopK})
	}
	for _, p := range pairs {
		set := ""
		for _, k := range p.keys {
			if _, ok := cfg[k]; !ok {
				continue
			}
			if set != "" {
				return fmt.Errorf("inferenceConfig: %s and %s: only one may be set", set, k)
			}
			set = k
			if err := take(cfg, k, p.dst); err != nil {
				return fmt.Errorf("inferenceConfig.%v", err)
			}
		}
	}
	if len(cfg) > 0 {
		top["inferenceConfig"], _ = json.Marshal(cfg)
	}
	return nil
}

// readToolConfig reads toolConfig: tools[] (toolSpec; any other member is a
// typed tool named by its member) and toolChoice (auto, any or tool).
func readToolConfig(req *llm.Request, top map[string]json.RawMessage) error {
	cfg, err := takeObject(top, "toolConfig")
	if err != nil || cfg == nil {
		return err
	}
	var tools []json.RawMessage
	var choice json.RawMessage
	if err := take(cfg, "tools", &tools); err != nil {
		return fmt.Errorf("toolConfig.%v", err)
	}
	if err := take(cfg, "toolChoice", &choice); err != nil {
		return fmt.Errorf("toolConfig.%v", err)
	}
	for i, raw := range tools {
		t, err := readTool(raw)
		if err != nil {
			return fmt.Errorf("toolConfig.tools.%d: %v", i, err)
		}
		req.Tools = append(req.Tools, t)
	}
	if choice != nil {
		kind, v, err := member(choice)
		if err != nil {
			return fmt.Errorf("toolConfig.toolChoice: %v", err)
		}
		switch kind {
		case "auto", "any":
			req.ToolChoice = &llm.ToolChoice{Type: kind}
		case "tool":
			tc := &llm.ToolChoice{Type: "tool"}
			o, err := object(v)
			if err == nil {
				err = take(o, "name", &tc.Name)
			}
			if err != nil || tc.Name == "" || len(o) > 0 {
				return fmt.Errorf("toolConfig.toolChoice.tool: unsupported value %s", v)
			}
			req.ToolChoice = tc
		default:
			return fmt.Errorf("toolConfig.toolChoice: unsupported member %q", kind)
		}
	}
	if len(cfg) > 0 {
		top["toolConfig"], _ = json.Marshal(cfg)
	}
	return nil
}

func readTool(raw json.RawMessage) (llm.Tool, error) {
	kind, v, err := member(raw)
	if err != nil {
		return llm.Tool{}, err
	}
	if kind != "toolSpec" {
		// cachePoint, systemTool, ...: kept whole, named when it says how.
		t := llm.Tool{Type: kind, Extra: map[string]json.RawMessage{kind: v}}
		if o, err := object(v); err == nil {
			_ = json.Unmarshal(o["name"], &t.Name)
		}
		return t, nil
	}
	spec, err := object(v)
	if err != nil {
		return llm.Tool{}, errors.New("toolSpec: must be an object")
	}
	var t llm.Tool
	if err := take(spec, "name", &t.Name); err != nil {
		return llm.Tool{}, fmt.Errorf("toolSpec.%v", err)
	}
	if err := take(spec, "description", &t.Description); err != nil {
		return llm.Tool{}, fmt.Errorf("toolSpec.%v", err)
	}
	if t.Name == "" {
		return llm.Tool{}, errors.New("toolSpec.name: required")
	}
	var schema json.RawMessage
	if err := take(spec, "inputSchema", &schema); err != nil {
		return llm.Tool{}, fmt.Errorf("toolSpec.%v", err)
	}
	if schema != nil {
		k, s, err := member(schema)
		if err != nil || k != "json" {
			return llm.Tool{}, errors.New("toolSpec.inputSchema: must hold exactly one member, json")
		}
		t.InputSchema = s
	}
	t.Extra = nonEmpty(spec) // strict, ...
	return t, nil
}

// readConverseResponse reads {output: {message}, stopReason, usage} (a Nova
// invoke response too).
func readConverseResponse(top map[string]json.RawMessage) (*llm.Response, error) {
	output, err := takeObject(top, "output")
	if err != nil {
		return nil, err
	}
	if output == nil {
		return nil, errors.New("output: required")
	}
	var msgRaw json.RawMessage
	if err := take(output, "message", &msgRaw); err != nil || msgRaw == nil {
		if err == nil {
			err = errors.New("message: required")
		}
		return nil, fmt.Errorf("output.%v", err)
	}
	msg, err := readMessage(msgRaw)
	if err != nil {
		return nil, fmt.Errorf("output.message.%v", err)
	}
	if msg.Role != "assistant" {
		return nil, fmt.Errorf("output.message.role: %q, want assistant", msg.Role)
	}
	out := &llm.Response{Role: "assistant", Content: msg.Content}
	var stop string
	if err := take(top, "stopReason", &stop); err != nil {
		return nil, err
	}
	out.StopReason = converseStop(stop)
	usage, err := takeObject(top, "usage")
	if err != nil {
		return nil, err
	}
	if out.Usage, err = converseUsage(usage); err != nil {
		return nil, fmt.Errorf("usage.%v", err)
	}
	return out, nil
}

// converseStop maps a Converse stopReason onto the neutral set; the
// filtering reasons are refusals, anything else unknown is kept as sent.
func converseStop(s string) string {
	switch s {
	case "guardrail_intervened", "content_filtered":
		return "refusal"
	}
	return s
}

// converseUsage reads Converse usage (Nova's invoke spelling of the cache
// counts, *TokenCount, too). Bedrock reports inputTokens without the cached
// prefix, the neutral convention already.
func converseUsage(m map[string]json.RawMessage) (llm.Usage, error) {
	var u llm.Usage
	if m == nil {
		return u, nil
	}
	var readAlt, writeAlt int64
	var details []map[string]json.RawMessage
	for key, dst := range map[string]any{
		"inputTokens": &u.InputTokens, "outputTokens": &u.OutputTokens,
		"cacheReadInputTokens": &u.CacheReadTokens, "cacheWriteInputTokens": &u.CacheWriteTokens,
		"cacheReadInputTokenCount": &readAlt, "cacheWriteInputTokenCount": &writeAlt,
		"cacheDetails": &details,
	} {
		if err := take(m, key, dst); err != nil {
			return u, err
		}
	}
	u.CacheReadTokens = max(u.CacheReadTokens, readAlt)
	u.CacheWriteTokens = max(u.CacheWriteTokens, writeAlt)
	for _, d := range details {
		var ttl string
		var n int64
		if take(d, "ttl", &ttl) == nil && take(d, "inputTokens", &n) == nil && ttl == "1h" {
			u.CacheWrite1hTokens += n
		}
	}
	return u, nil
}

// converseDecoder reads a ConverseStream body.
type converseDecoder struct {
	src  frames
	conv *converseState
	err  error
}

func (d *converseDecoder) Next() (llm.Event, error) {
	for {
		if ev, ok := d.conv.pop(); ok {
			return ev, nil
		}
		if d.err != nil {
			return llm.Event{}, d.err
		}
		m, err := d.src.next()
		if err != nil {
			d.err = d.conv.end(err)
			continue
		}
		if m.Headers[":message-type"] != "event" {
			d.conv.fail(exception(m))
			d.err = io.EOF
			continue
		}
		if err := strictjson.Check(m.Payload); err != nil {
			d.err = fmt.Errorf("invalid %s event: %v", m.Headers[":event-type"], err)
			continue
		}
		if err := d.conv.event(m.Headers[":event-type"], m.Payload); err != nil {
			d.err = err
		}
	}
}
