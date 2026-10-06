package bedrock

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/eventstream"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/strictjson"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/openaicompat"
)

// HistoryKey is the Request.Extra key the invoke Reader sets, to
// HistoryPrompt, on a request read from a prompt-style body: the whole body
// is one prompt string, read as one user text turn, so any earlier turns
// are inside that text (rendered by the client's chat template) rather than
// in Request.Messages.
const (
	HistoryKey    = "history"
	HistoryPrompt = "prompt"
)

// InvokeReader is the llm.Reader for InvokeModel and
// InvokeModelWithResponseStream bodies of non-Anthropic model families
// (POST /model/{modelId}/invoke and /invoke-with-response-stream). The body
// format depends on the model family; the Reader tells them apart by their
// fields:
//
//   - Amazon Titan Text: {inputText, textGenerationConfig}; response
//     results[]; stream chunks {outputText, completionReason}.
//   - Prompt-style bodies (Meta Llama, Mistral's text models, and any other
//     family that sends {prompt, ...}): one user text turn, with
//     Extra[HistoryKey] = HistoryPrompt. Responses: Llama's {generation,
//     stop_reason} and Mistral's {outputs[]{text, stop_reason}}.
//   - Chat-style bodies {messages} in the OpenAI Chat Completions shape
//     (Mistral Large's chat, OpenAI-compatible models): read as the
//     "openai_chat" Reader reads them. Responses and stream chunks:
//     choices[] with message or delta, finish_reason or stop_reason.
//   - Amazon Nova {messages, system, inferenceConfig, toolConfig}: the
//     Converse shape, read as the "bedrock-converse" Reader reads it;
//     responses and stream chunks are Converse's too.
//
// Anything else, an Anthropic Messages body included (the "anthropic"
// Reader reads those), is ErrUnsupportedFormat.
type InvokeReader struct{}

func (InvokeReader) Name() string { return InvokeName }

// DecodeRequest reads an invoke request body strictly.
func (InvokeReader) DecodeRequest(body []byte) (*llm.Request, error) {
	if err := strictjson.Check(body); err != nil {
		return nil, &llm.RequestError{Err: fmt.Errorf("invalid request body: %v", err)}
	}
	top, err := object(body)
	if err != nil {
		return nil, &llm.RequestError{Err: fmt.Errorf("invalid request body: %v", err)}
	}
	var req *llm.Request
	switch {
	case has(top, "anthropic_version"):
		err = unsupported("an Anthropic Messages body (read it with the anthropic reader)")
	case has(top, "inputText"):
		req, err = readTitan(top)
	case has(top, "prompt"):
		req, err = readPrompt(top)
	case has(top, "messages") && novaShaped(top):
		req, err = readConverse(top, true)
	case has(top, "messages"):
		return openaicompat.ChatReader{}.DecodeRequest(body)
	default:
		err = unsupported("no inputText, prompt or messages field")
	}
	if err != nil {
		return nil, &llm.RequestError{Err: err}
	}
	return req, nil
}

func has(m map[string]json.RawMessage, key string) bool { _, ok := m[key]; return ok }

// novaShaped tells a Nova messages body from a Chat Completions one: Nova
// has Converse's top-level fields, or content blocks without a "type".
func novaShaped(top map[string]json.RawMessage) bool {
	if has(top, "schemaVersion") || has(top, "inferenceConfig") || has(top, "toolConfig") {
		return true
	}
	var sys []json.RawMessage
	if json.Unmarshal(top["system"], &sys) == nil && top["system"] != nil && !isNull(top["system"]) {
		return true
	}
	var msgs []struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(top["messages"], &msgs) != nil || len(msgs) == 0 {
		return false
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(msgs[0].Content, &parts) != nil || len(parts) == 0 {
		return false
	}
	return !has(parts[0], "type")
}

// promptTurn is the one user text turn of a prompt-style body.
func promptTurn(req *llm.Request, top map[string]json.RawMessage, key string) error {
	var text string
	raw := top[key]
	if err := take(top, key, &text); err != nil || isNull(raw) {
		return fmt.Errorf("%s: must be a string", key)
	}
	if has(top, HistoryKey) {
		return fmt.Errorf("%s: unsupported field in a %s body", HistoryKey, key)
	}
	req.Messages = []llm.Message{{Role: "user", Content: []llm.Block{{Type: llm.BlockText, Text: text}}}}
	return nil
}

func markPrompt(req *llm.Request, top map[string]json.RawMessage) {
	top[HistoryKey] = json.RawMessage(`"` + HistoryPrompt + `"`)
	req.Extra = top
}

// readTitan reads {inputText, textGenerationConfig{maxTokenCount,
// temperature, topP, stopSequences}}.
func readTitan(top map[string]json.RawMessage) (*llm.Request, error) {
	req := &llm.Request{}
	if err := promptTurn(req, top, "inputText"); err != nil {
		return nil, err
	}
	cfg, err := takeObject(top, "textGenerationConfig")
	if err != nil {
		return nil, err
	}
	for key, dst := range map[string]any{
		"maxTokenCount": &req.MaxTokens, "temperature": &req.Temperature,
		"topP": &req.TopP, "stopSequences": &req.StopSequences,
	} {
		if err := take(cfg, key, dst); err != nil {
			return nil, fmt.Errorf("textGenerationConfig.%v", err)
		}
	}
	if len(cfg) > 0 {
		top["textGenerationConfig"], _ = json.Marshal(cfg)
	}
	markPrompt(req, top)
	return req, nil
}

// readPrompt reads {prompt, ...}: Llama's max_gen_len or the others'
// max_tokens, temperature, top_p, top_k, and Mistral's stop (or
// stop_sequences). Other fields (images, ...) ride in Extra.
func readPrompt(top map[string]json.RawMessage) (*llm.Request, error) {
	req := &llm.Request{}
	if err := promptTurn(req, top, "prompt"); err != nil {
		return nil, err
	}
	if has(top, "max_gen_len") && has(top, "max_tokens") {
		return nil, errors.New("max_gen_len and max_tokens: only one may be set")
	}
	if has(top, "stop") && has(top, "stop_sequences") {
		return nil, errors.New("stop and stop_sequences: only one may be set")
	}
	for _, f := range []struct {
		key string
		dst any
	}{
		{"max_gen_len", &req.MaxTokens}, {"max_tokens", &req.MaxTokens}, {"temperature", &req.Temperature},
		{"top_p", &req.TopP}, {"top_k", &req.TopK}, {"stop", &req.StopSequences}, {"stop_sequences", &req.StopSequences},
	} {
		if err := take(top, f.key, f.dst); err != nil {
			return nil, err
		}
	}
	markPrompt(req, top)
	return req, nil
}

// DecodeResponse reads a non-stream invoke response of a mapped family.
func (InvokeReader) DecodeResponse(body []byte) (*llm.Response, error) {
	top, err := checkedObject(body, "response body")
	if err != nil {
		return nil, err
	}
	out, err := readInvokeResponse(top)
	if err != nil {
		if errors.Is(err, ErrUnsupportedFormat) {
			return nil, err
		}
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	return out, nil
}

func readInvokeResponse(top map[string]json.RawMessage) (*llm.Response, error) {
	out := &llm.Response{Role: "assistant", Content: []llm.Block{}}
	text := func(s string) {
		if s != "" {
			out.Content = append(out.Content, llm.Block{Type: llm.BlockText, Text: s})
		}
	}
	switch {
	case has(top, "type") || has(top, "anthropic_version"):
		return nil, unsupported("an Anthropic Messages body (read it with the anthropic reader)")
	case has(top, "results"): // Titan
		var results []map[string]json.RawMessage
		if err := take(top, "results", &results); err != nil || len(results) == 0 {
			return nil, errors.New("results: must be a non-empty array")
		}
		var s, reason string
		if err := take(results[0], "outputText", &s); err != nil {
			return nil, fmt.Errorf("results.0.%v", err)
		}
		if err := take(results[0], "completionReason", &reason); err != nil {
			return nil, fmt.Errorf("results.0.%v", err)
		}
		text(s)
		out.StopReason = titanStop(reason)
		_ = take(top, "inputTextTokenCount", &out.Usage.InputTokens)
		_ = take(results[0], "tokenCount", &out.Usage.OutputTokens)
	case has(top, "generation"): // Llama
		var s, reason string
		if err := take(top, "generation", &s); err != nil {
			return nil, err
		}
		if err := take(top, "stop_reason", &reason); err != nil {
			return nil, err
		}
		text(s)
		out.StopReason = chatStop(reason)
		_ = take(top, "prompt_token_count", &out.Usage.InputTokens)
		_ = take(top, "generation_token_count", &out.Usage.OutputTokens)
	case has(top, "outputs"): // Mistral text
		var outputs []map[string]json.RawMessage
		if err := take(top, "outputs", &outputs); err != nil || len(outputs) == 0 {
			return nil, errors.New("outputs: must be a non-empty array")
		}
		var s, reason string
		if err := take(outputs[0], "text", &s); err != nil {
			return nil, fmt.Errorf("outputs.0.%v", err)
		}
		if err := take(outputs[0], "stop_reason", &reason); err != nil {
			return nil, fmt.Errorf("outputs.0.%v", err)
		}
		text(s)
		out.StopReason = chatStop(reason)
	case has(top, "choices"): // chat
		return readChatResponse(top, out)
	case has(top, "output"): // Nova
		return readConverseResponse(top)
	default:
		return nil, unsupported("no results, generation, outputs, choices or output field")
	}
	return out, nil
}

// readChatResponse reads {id, model, choices[0]{message, finish_reason |
// stop_reason}, usage{prompt_tokens, completion_tokens}}.
func readChatResponse(top map[string]json.RawMessage, out *llm.Response) (*llm.Response, error) {
	_ = take(top, "id", &out.ID)
	_ = take(top, "model", &out.Model)
	var choices []map[string]json.RawMessage
	if err := take(top, "choices", &choices); err != nil || len(choices) == 0 {
		return nil, errors.New("choices: must be a non-empty array")
	}
	c := choices[0]
	msg, err := takeObject(c, "message")
	if err != nil || msg == nil {
		return nil, errors.New("choices.0.message: must be an object")
	}
	var content string
	var calls []map[string]json.RawMessage
	if err := take(msg, "content", &content); err != nil {
		return nil, unsupported("choices.0.message.content is not a string")
	}
	if err := take(msg, "tool_calls", &calls); err != nil {
		return nil, fmt.Errorf("choices.0.message.%v", err)
	}
	if content != "" {
		out.Content = append(out.Content, llm.Block{Type: llm.BlockText, Text: content})
	}
	for i, call := range calls {
		b := llm.Block{Type: llm.BlockToolUse}
		fn, err := takeObject(call, "function")
		var args string
		if err == nil && fn != nil {
			_ = take(call, "id", &b.ID)
			_ = take(fn, "name", &b.Name)
			err = take(fn, "arguments", &args)
		}
		if err != nil || b.ID == "" || b.Name == "" {
			return nil, fmt.Errorf("choices.0.message.tool_calls.%d: id and function.name required", i)
		}
		b.Input = json.RawMessage(`{}`)
		if args != "" {
			if !json.Valid([]byte(args)) {
				return nil, fmt.Errorf("choices.0.message.tool_calls.%d.function.arguments: not JSON", i)
			}
			b.Input = json.RawMessage(args)
		}
		out.Content = append(out.Content, b)
	}
	var reason string
	if err := take(c, "finish_reason", &reason); err != nil {
		return nil, fmt.Errorf("choices.0.%v", err)
	}
	if reason == "" {
		if err := take(c, "stop_reason", &reason); err != nil {
			return nil, fmt.Errorf("choices.0.%v", err)
		}
	}
	out.StopReason = chatStop(reason)
	if usage, err := takeObject(top, "usage"); err == nil && usage != nil {
		_ = take(usage, "prompt_tokens", &out.Usage.InputTokens)
		_ = take(usage, "completion_tokens", &out.Usage.OutputTokens)
	}
	return out, nil
}

func titanStop(s string) string {
	switch s {
	case "FINISH", "FINISHED":
		return "end_turn"
	case "LENGTH":
		return "max_tokens"
	case "STOP_CRITERIA_MET":
		return "stop_sequence"
	case "CONTENT_FILTERED":
		return "refusal"
	}
	return s
}

func chatStop(s string) string {
	switch s {
	case "stop":
		return "end_turn"
	case "length", "model_length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return s
}

// NewResponseDecoder reads an InvokeModelWithResponseStream body: event-
// stream "chunk" frames whose {"bytes": base64} payload is one chunk of the
// family's own stream. The family is told from the first chunk; every
// chunk is checked strictly, the frame payload and the decoded chunk both.
func (InvokeReader) NewResponseDecoder(r io.Reader) llm.StreamDecoder {
	return &invokeDecoder{src: frames{eventstream.NewReader(r)}, conv: newConverseState()}
}

type invokeDecoder struct {
	src    frames
	conv   *converseState // the emitter; Nova's events use all of it
	family string
	err    error
}

func (d *invokeDecoder) Next() (llm.Event, error) {
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
		if err := d.chunk(m.Headers[":event-type"], m.Payload); err != nil {
			d.err = err
		}
	}
}

// chunk reads one event frame.
func (d *invokeDecoder) chunk(typ string, payload []byte) error {
	if typ != "chunk" {
		return fmt.Errorf("unknown stream event %q", typ)
	}
	if err := strictjson.Check(payload); err != nil {
		return fmt.Errorf("invalid chunk event: %v", err)
	}
	frame, err := object(payload)
	if err != nil {
		return fmt.Errorf("invalid chunk event: %v", err)
	}
	var b64 string
	if err := take(frame, "bytes", &b64); err != nil || b64 == "" {
		return errors.New("invalid chunk event: bytes: required")
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("invalid chunk event: bytes: %v", err)
	}
	if err := strictjson.Check(data); err != nil {
		return fmt.Errorf("invalid chunk: %v", err)
	}
	c, err := object(data)
	if err != nil {
		return fmt.Errorf("invalid chunk: %v", err)
	}
	if d.conv.done {
		return errors.New("chunk after the end of the stream")
	}
	// Bedrock's model-agnostic counts, on the last chunk of every family.
	if metrics, err := takeObject(c, "amazon-bedrock-invocationMetrics"); err == nil && metrics != nil {
		u := llm.Usage{}
		_ = take(metrics, "inputTokenCount", &u.InputTokens)
		_ = take(metrics, "outputTokenCount", &u.OutputTokens)
		_ = take(metrics, "cacheReadInputTokenCount", &u.CacheReadTokens)
		_ = take(metrics, "cacheWriteInputTokenCount", &u.CacheWriteTokens)
		d.conv.usage = &u
	}
	family := chunkFamily(c)
	if d.family == "" {
		d.family = family
	}
	switch {
	case d.family == "" || family == "" && len(c) > 0:
		return unsupported("a stream chunk of no mapped family")
	case family != d.family && len(c) > 0:
		return fmt.Errorf("invalid chunk: a %s chunk in a %s stream", family, d.family)
	}
	switch d.family {
	case "anthropic":
		return unsupported("an Anthropic stream (read it with the anthropic reader)")
	case "nova":
		for typ, v := range c { // exactly one member: chunkFamily checked
			if err := d.conv.event(typ, v); err != nil {
				return fmt.Errorf("invalid chunk: %v", err)
			}
		}
		return nil
	}
	if err := d.textChunk(c); err != nil {
		if errors.Is(err, ErrUnsupportedFormat) {
			return err
		}
		return fmt.Errorf("invalid chunk: %v", err)
	}
	return nil
}

// chunkFamily tells a decoded stream chunk's family by its fields ("" for
// none).
func chunkFamily(c map[string]json.RawMessage) string {
	switch {
	case has(c, "type"):
		return "anthropic"
	case has(c, "outputText"):
		return "titan"
	case has(c, "generation"):
		return "llama"
	case has(c, "outputs"):
		return "mistral"
	case has(c, "choices"):
		return "chat"
	}
	if len(c) == 1 {
		for k := range c {
			switch k {
			case "messageStart", "contentBlockStart", "contentBlockDelta", "contentBlockStop", "messageStop", "metadata":
				return "nova"
			}
		}
	}
	return ""
}

// textChunk reads one chunk of a text-only family: its text, and its stop
// reason and token counts when it carries them.
func (d *invokeDecoder) textChunk(c map[string]json.RawMessage) error {
	var text, reason string
	var in, out int64
	switch d.family {
	case "titan":
		for key, dst := range map[string]any{"outputText": &text, "completionReason": &reason,
			"inputTextTokenCount": &in, "totalOutputTextTokenCount": &out} {
			if err := take(c, key, dst); err != nil {
				return err
			}
		}
		reason = titanStop(reason)
	case "llama":
		for key, dst := range map[string]any{"generation": &text, "stop_reason": &reason,
			"prompt_token_count": &in, "generation_token_count": &out} {
			if err := take(c, key, dst); err != nil {
				return err
			}
		}
		reason = chatStop(reason)
	case "mistral", "chat":
		var items []map[string]json.RawMessage
		key := map[string]string{"mistral": "outputs", "chat": "choices"}[d.family]
		if err := take(c, key, &items); err != nil {
			return err
		}
		if len(items) > 0 {
			var err error
			if text, reason, err = choiceChunk(d.family, items[0]); err != nil {
				if errors.Is(err, ErrUnsupportedFormat) {
					return err
				}
				return fmt.Errorf("%s.0.%v", key, err)
			}
		}
		if usage, err := takeObject(c, "usage"); err == nil && usage != nil {
			_ = take(usage, "prompt_tokens", &in)
			_ = take(usage, "completion_tokens", &out)
		}
	}
	d.conv.start()
	d.conv.text(text)
	if reason != "" {
		d.conv.stop = reason
	}
	if d.conv.usage == nil && (in > 0 || out > 0) {
		d.conv.usage = &llm.Usage{}
	}
	if u := d.conv.usage; u != nil {
		u.InputTokens, u.OutputTokens = max(u.InputTokens, in), max(u.OutputTokens, out)
	}
	return nil
}

// choiceChunk reads one outputs[] (Mistral text) or choices[] (chat) item.
// A chat chunk carries its text in delta (or, for Mistral's chat, in
// message); a tool call in a stream is not mapped.
func choiceChunk(family string, item map[string]json.RawMessage) (text, reason string, err error) {
	if family == "mistral" {
		if err = take(item, "text", &text); err == nil {
			err = take(item, "stop_reason", &reason)
		}
		return text, chatStop(reason), err
	}
	for _, key := range []string{"delta", "message"} {
		part, err := takeObject(item, key)
		if err != nil {
			return "", "", err
		}
		if part == nil {
			continue
		}
		if has(part, "tool_calls") {
			return "", "", unsupported("a tool call in an invoke chat stream")
		}
		if err := take(part, "content", &text); err != nil {
			return "", "", fmt.Errorf("%s.%v", key, err)
		}
	}
	if err = take(item, "finish_reason", &reason); err == nil && reason == "" {
		err = take(item, "stop_reason", &reason)
	}
	return text, chatStop(reason), err
}
