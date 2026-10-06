package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// errNotObject is a list entry that is not a JSON object; at places it.
var errNotObject = errors.New("must be an object")

// at prefixes err with the path of the entry it is about.
func at(path string, err error) error {
	if err == errNotObject {
		return fmt.Errorf("%s: %v", path, err)
	}
	return fmt.Errorf("%s.%v", path, err)
}

func readRequest(body []byte) (*llm.Request, error) {
	top, err := object(body)
	if err != nil {
		return nil, fmt.Errorf("invalid request body: %v", err)
	}
	req := &llm.Request{}
	var contents, tools []json.RawMessage
	for _, f := range []struct {
		key string
		dst any
	}{
		{"model", &req.Model}, {"contents", &contents}, {"tools", &tools},
	} {
		if err := take(top, f.key, f.dst); err != nil {
			return nil, err
		}
	}
	system, err := takeObject(top, "systemInstruction")
	if err != nil {
		return nil, err
	}
	if system != nil {
		if req.System, err = readSystem(system); err != nil {
			return nil, fmt.Errorf("systemInstruction.%v", err)
		}
	}
	ids := &calls{}
	for i, raw := range contents {
		m, err := readContent(raw, ids)
		if err != nil {
			return nil, at(fmt.Sprintf("contents.%d", i), err)
		}
		req.Messages = append(req.Messages, m)
	}
	for i, raw := range tools {
		ts, err := readTool(raw)
		if err != nil {
			return nil, at(fmt.Sprintf("tools.%d", i), err)
		}
		req.Tools = append(req.Tools, ts...)
	}
	toolConfig, err := takeObject(top, "toolConfig")
	if err != nil {
		return nil, err
	}
	if toolConfig != nil {
		if req.ToolChoice, err = readToolConfig(toolConfig); err != nil {
			return nil, fmt.Errorf("toolConfig.%v", err)
		}
		if len(toolConfig) > 0 {
			top["toolConfig"], _ = json.Marshal(toolConfig)
		}
	}
	gen, err := takeObject(top, "generationConfig")
	if err != nil {
		return nil, err
	}
	if gen != nil {
		if err := readGenerationConfig(req, gen); err != nil {
			return nil, fmt.Errorf("generationConfig.%v", err)
		}
		if len(gen) > 0 {
			top["generationConfig"], _ = json.Marshal(gen)
		}
	}
	req.Extra = nonEmpty(top)
	return req, nil
}

// readSystem reads systemInstruction: its text parts (its role, if any, is
// dropped: the system prompt has none).
func readSystem(m map[string]json.RawMessage) ([]llm.Block, error) {
	var role string
	var parts []json.RawMessage
	if err := take(m, "role", &role); err != nil {
		return nil, err
	}
	if err := take(m, "parts", &parts); err != nil {
		return nil, err
	}
	var out []llm.Block
	for i, raw := range parts {
		b, err := (&calls{}).part(raw)
		if err != nil {
			return nil, fmt.Errorf("parts.%d: %v", i, err)
		}
		if b.Type != llm.BlockText {
			return nil, fmt.Errorf("parts.%d: only text parts may be set", i)
		}
		out = append(out, b)
	}
	if extra := nonEmpty(m); extra != nil && len(out) > 0 {
		merge(&out[0].Extra, extra)
	}
	return out, nil
}

// readContent reads one contents entry. The error is prefixed by the caller
// with its path.
func readContent(raw json.RawMessage, ids *calls) (llm.Message, error) {
	m, err := object(raw)
	if err != nil {
		return llm.Message{}, errNotObject
	}
	var role string
	var parts []json.RawMessage
	if err := take(m, "role", &role); err != nil {
		return llm.Message{}, err
	}
	if err := take(m, "parts", &parts); err != nil {
		return llm.Message{}, err
	}
	neutral := "user"
	switch role {
	case "", "user", "function":
	case "model":
		neutral = "assistant"
		ids.turn()
	default:
		return llm.Message{}, fmt.Errorf("role: unknown role %q", role)
	}
	if len(parts) == 0 {
		return llm.Message{}, errors.New("parts: required")
	}
	blocks := make([]llm.Block, 0, len(parts))
	for j, p := range parts {
		b, err := ids.part(p)
		if err != nil {
			return llm.Message{}, fmt.Errorf("parts.%d: %v", j, err)
		}
		if role == "function" && b.Type != llm.BlockToolResult {
			return llm.Message{}, fmt.Errorf("parts.%d: a function turn holds only functionResponse parts", j)
		}
		blocks = append(blocks, b)
	}
	merge(&blocks[0].Extra, m)
	return llm.Message{Role: neutral, Content: blocks}, nil
}

// calls numbers the functionCall parts of one body and pairs functionResponse
// parts with them.
type calls struct {
	n       int       // functionCall parts read so far
	pending []callRef // calls of the latest model turn not answered yet
}

type callRef struct{ id, name string }

// callID returns the id of the n-th functionCall of a body, named name, when
// the body gives it none.
func callID(n int, name string) string { return fmt.Sprintf("gemini-call-%d-%s", n, name) }

// call records a functionCall and returns its id.
func (c *calls) call(id, name string) string {
	if id == "" {
		id = callID(c.n, name)
	}
	c.n++
	c.pending = append(c.pending, callRef{id: id, name: name})
	return id
}

// turn starts a model turn: calls of earlier turns can no longer be answered
// by name.
func (c *calls) turn() { c.pending = nil }

// answer returns the id of the call a functionResponse answers: the pending
// call with its id, or, without one, the first pending call of its name.
func (c *calls) answer(id, name string) (string, error) {
	for i, p := range c.pending {
		if (id != "" && p.id == id) || (id == "" && p.name == name) {
			c.pending = append(c.pending[:i:i], c.pending[i+1:]...)
			return p.id, nil
		}
	}
	if id != "" {
		return id, nil
	}
	return "", fmt.Errorf("functionResponse: no unanswered functionCall named %q in the latest model turn", name)
}

// dataFields are the members of a part's data oneof.
var dataFields = []string{"text", "inlineData", "fileData", "functionCall", "functionResponse", "executableCode", "codeExecutionResult"}

// part reads one part into a block.
func (c *calls) part(raw json.RawMessage) (llm.Block, error) {
	p, err := object(raw)
	if err != nil {
		return llm.Block{}, errors.New("must be an object")
	}
	kind := ""
	for _, f := range dataFields {
		if _, ok := p[f]; !ok {
			continue
		}
		if kind != "" {
			return llm.Block{}, fmt.Errorf("%s and %s: only one may be set", kind, f)
		}
		kind = f
	}
	if kind == "" {
		return llm.Block{}, fmt.Errorf("one of %s is required", strings.Join(dataFields, ", "))
	}
	var thought bool
	var sig string
	if err := take(p, "thought", &thought); err != nil {
		return llm.Block{}, err
	}
	if err := take(p, "thoughtSignature", &sig); err != nil {
		return llm.Block{}, err
	}
	var b llm.Block
	switch kind {
	case "text":
		var text string
		if err := take(p, "text", &text); err != nil {
			return llm.Block{}, err
		}
		b = llm.Block{Type: llm.BlockText, Text: text}
		if thought {
			b = llm.Block{Type: llm.BlockThinking, Thinking: text, Signature: sig}
			thought, sig = false, ""
		}
	case "inlineData", "fileData":
		if b, err = c.media(p, kind); err != nil {
			return llm.Block{}, err
		}
	case "functionCall":
		if b, err = c.functionCall(p); err != nil {
			return llm.Block{}, err
		}
	case "functionResponse":
		if b, err = c.functionResponse(p); err != nil {
			return llm.Block{}, err
		}
	default:
		// executableCode, codeExecutionResult: kept whole; the text a reader
		// can use is read.
		inner, _ := object(p[kind])
		b = llm.Block{Type: llm.BlockType(kind), Raw: raw}
		_ = json.Unmarshal(inner["code"], &b.Text)
		if kind == "codeExecutionResult" {
			_ = json.Unmarshal(inner["output"], &b.Text)
		}
		return b, nil
	}
	if thought {
		p["thought"] = json.RawMessage(`true`)
	}
	if sig != "" {
		p["thoughtSignature"], _ = json.Marshal(sig)
	}
	merge(&b.Extra, p)
	return b, nil
}

// media reads an inlineData or fileData part: an image block for an image/*
// MIME type, else a document block.
func (c *calls) media(p map[string]json.RawMessage, kind string) (llm.Block, error) {
	d, err := takeObject(p, kind)
	if err != nil {
		return llm.Block{}, err
	}
	var mime, data string
	if err := take(d, "mimeType", &mime); err != nil {
		return llm.Block{}, fmt.Errorf("%s.%v", kind, err)
	}
	src := &llm.Source{MediaType: mime}
	if kind == "inlineData" {
		if err := take(d, "data", &data); err != nil {
			return llm.Block{}, fmt.Errorf("%s.%v", kind, err)
		}
		if mime == "" || data == "" {
			return llm.Block{}, errors.New("inlineData: mimeType and data are required")
		}
		src.Type, src.Data = "base64", data
	} else {
		if err := take(d, "fileUri", &data); err != nil {
			return llm.Block{}, fmt.Errorf("%s.%v", kind, err)
		}
		if data == "" {
			return llm.Block{}, errors.New("fileData.fileUri: required")
		}
		src.Type, src.FileID = "file", data
	}
	typ := llm.BlockDocument
	if strings.HasPrefix(mime, "image/") {
		typ = llm.BlockImage
	}
	return llm.Block{Type: typ, Source: src, Extra: nonEmpty(d)}, nil // displayName, ...
}

func (c *calls) functionCall(p map[string]json.RawMessage) (llm.Block, error) {
	fc, err := takeObject(p, "functionCall")
	if err != nil {
		return llm.Block{}, err
	}
	var id, name string
	var args json.RawMessage
	for key, dst := range map[string]any{"id": &id, "name": &name, "args": &args} {
		if err := take(fc, key, dst); err != nil {
			return llm.Block{}, fmt.Errorf("functionCall.%v", err)
		}
	}
	if name == "" {
		return llm.Block{}, errors.New("functionCall.name: required")
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	} else if !isJSONObject(args) {
		return llm.Block{}, errors.New("functionCall.args: must be an object")
	}
	return llm.Block{Type: llm.BlockToolUse, ID: c.call(id, name), Name: name, Input: args, Extra: nonEmpty(fc)}, nil
}

func (c *calls) functionResponse(p map[string]json.RawMessage) (llm.Block, error) {
	fr, err := takeObject(p, "functionResponse")
	if err != nil {
		return llm.Block{}, err
	}
	var id, name string
	var response json.RawMessage
	var parts []json.RawMessage
	for key, dst := range map[string]any{"id": &id, "name": &name, "response": &response, "parts": &parts} {
		if err := take(fr, key, dst); err != nil {
			return llm.Block{}, fmt.Errorf("functionResponse.%v", err)
		}
	}
	if name == "" {
		return llm.Block{}, errors.New("functionResponse.name: required")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(response, &obj) != nil || obj == nil {
		return llm.Block{}, errors.New("functionResponse.response: must be an object")
	}
	toolUseID, err := c.answer(id, name)
	if err != nil {
		return llm.Block{}, err
	}
	var text bytes.Buffer
	_ = json.Compact(&text, response)
	b := llm.Block{
		Type: llm.BlockToolResult, ToolUseID: toolUseID, IsError: !isNull(obj["error"]),
		Content: []llm.Block{{Type: llm.BlockText, Text: text.String()}},
	}
	for i, raw := range parts {
		pb, err := c.part(raw)
		if err != nil {
			return llm.Block{}, fmt.Errorf("functionResponse.parts.%d: %v", i, err)
		}
		if pb.Type != llm.BlockImage && pb.Type != llm.BlockDocument {
			return llm.Block{}, fmt.Errorf("functionResponse.parts.%d: only inlineData and fileData parts may be set", i)
		}
		b.Content = append(b.Content, pb)
	}
	b.Extra = nonEmpty(fr) // willContinue, scheduling, ...
	return b, nil
}

// readTool reads one tools entry: its function declarations, then each
// other key as a vendor-defined tool, in key order.
func readTool(raw json.RawMessage) ([]llm.Tool, error) {
	m, err := object(raw)
	if err != nil {
		return nil, errNotObject
	}
	var decls []json.RawMessage
	if err := take(m, "functionDeclarations", &decls); err != nil {
		return nil, err
	}
	var out []llm.Tool
	for j, d := range decls {
		t, err := readDeclaration(d)
		if err != nil {
			return nil, at(fmt.Sprintf("functionDeclarations.%d", j), err)
		}
		out = append(out, t)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, llm.Tool{Type: k, Name: k, Extra: map[string]json.RawMessage{k: m[k]}})
	}
	return out, nil
}

func readDeclaration(raw json.RawMessage) (llm.Tool, error) {
	m, err := object(raw)
	if err != nil {
		return llm.Tool{}, errNotObject
	}
	var t llm.Tool
	var params, jsonSchema json.RawMessage
	for key, dst := range map[string]any{"name": &t.Name, "description": &t.Description, "parameters": &params, "parametersJsonSchema": &jsonSchema} {
		if err := take(m, key, dst); err != nil {
			return llm.Tool{}, err
		}
	}
	if t.Name == "" {
		return llm.Tool{}, errors.New("name: required")
	}
	if len(params) > 0 && len(jsonSchema) > 0 {
		return llm.Tool{}, errors.New("parameters and parametersJsonSchema: only one may be set")
	}
	t.InputSchema = params
	if len(jsonSchema) > 0 {
		t.InputSchema = jsonSchema
	}
	t.Extra = nonEmpty(m) // response, behavior, ...
	return t, nil
}

// readToolConfig maps toolConfig.functionCallingConfig onto a ToolChoice,
// removing from tc what it maps.
func readToolConfig(tc map[string]json.RawMessage) (*llm.ToolChoice, error) {
	fc, err := takeObject(tc, "functionCallingConfig")
	if err != nil || fc == nil {
		return nil, err
	}
	modeRaw, namesRaw := fc["mode"], fc["allowedFunctionNames"]
	var mode string
	var names []string
	if err := take(fc, "mode", &mode); err != nil {
		return nil, fmt.Errorf("functionCallingConfig.%v", err)
	}
	if err := take(fc, "allowedFunctionNames", &names); err != nil {
		return nil, fmt.Errorf("functionCallingConfig.%v", err)
	}
	var choice *llm.ToolChoice
	switch mode {
	case "", "MODE_UNSPECIFIED":
	case "AUTO":
		choice = &llm.ToolChoice{Type: "auto"}
	case "NONE":
		choice = &llm.ToolChoice{Type: "none"}
	case "ANY":
		choice = &llm.ToolChoice{Type: "any"}
		if len(names) == 1 {
			choice = &llm.ToolChoice{Type: "tool", Name: names[0]}
			names = nil
		}
	case "VALIDATED":
		choice = &llm.ToolChoice{Type: "auto"}
		fc["mode"] = modeRaw
	default:
		return nil, fmt.Errorf("functionCallingConfig.mode: unsupported value %q", mode)
	}
	if len(names) > 0 {
		fc["allowedFunctionNames"] = namesRaw
	}
	if len(fc) > 0 {
		tc["functionCallingConfig"], _ = json.Marshal(fc)
	}
	return choice, nil
}

// readGenerationConfig fills req from g, removing from g what it maps.
func readGenerationConfig(req *llm.Request, g map[string]json.RawMessage) error {
	for _, f := range []struct {
		key string
		dst any
	}{
		{"maxOutputTokens", &req.MaxTokens}, {"temperature", &req.Temperature}, {"topP", &req.TopP},
		{"topK", &req.TopK}, {"stopSequences", &req.StopSequences},
	} {
		if err := take(g, f.key, f.dst); err != nil {
			return err
		}
	}
	tc, err := takeObject(g, "thinkingConfig")
	if err != nil || tc == nil {
		return err
	}
	levelRaw := tc["thinkingLevel"]
	var budget *int64
	var level string
	if err := take(tc, "thinkingBudget", &budget); err != nil {
		return fmt.Errorf("thinkingConfig.%v", err)
	}
	if err := take(tc, "thinkingLevel", &level); err != nil {
		return fmt.Errorf("thinkingConfig.%v", err)
	}
	switch l := strings.ToLower(level); l {
	case "":
	case "low", "medium", "high":
		req.Effort = l
	default:
		tc["thinkingLevel"] = levelRaw
	}
	if budget != nil {
		switch b := *budget; {
		case b == -1:
			req.Thinking = &llm.Thinking{Type: "adaptive"}
		case b == 0:
			req.Thinking = &llm.Thinking{Type: "disabled"}
		case b > 0:
			req.Thinking = &llm.Thinking{Type: "enabled", BudgetTokens: b}
		default:
			return fmt.Errorf("thinkingConfig.thinkingBudget: unsupported value %d", b)
		}
		req.Thinking.Extra = nonEmpty(tc) // includeThoughts, ...
		return nil
	}
	if len(tc) > 0 {
		g["thinkingConfig"], _ = json.Marshal(tc)
	}
	return nil
}
