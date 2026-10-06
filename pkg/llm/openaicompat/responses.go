package openaicompat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/sse"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/strictjson"
)

// ResponsesReaderName is the registry name of ResponsesReader.
const ResponsesReaderName = "openai_responses"

func init() { llm.RegisterReader(ResponsesReader{}) }

// ResponsesReader is the llm.Reader for the OpenAI Responses API wire
// (/v1/responses): a client's request, and the response or stream it got
// back.
//
// A request's instructions are the system prompt, and so are leading
// system and developer messages of its input; a later one is a "system"
// turn. A string input is one user text turn. Input items map as follows:
//
//   - message (or an item with a role and no type): a turn of its role,
//     which must be user, assistant, system or developer; any other role is
//     refused. Parts: input_text and output_text are text blocks,
//     input_image an image block, input_file a document block; any other
//     part (refusal, input_audio, ...) is kept whole in Raw.
//   - function_call: a tool_use block (call_id is the id).
//   - function_call_output: a tool_result block (call_id is tool_use_id).
//   - reasoning: a thinking block holding the summary text.
//   - any other item type (web_search_call, mcp_call, item_reference,
//     computer_call_output, ...): kept whole in Raw, under its wire type.
//     Item types are kept rather than refused because the API adds them
//     often and none of them changes who said what.
//
// Assistant-side items (assistant messages, function_call, reasoning and
// unknown items) join the previous turn when it is an assistant turn, so a
// reasoning item, a message and the calls after it are one turn. Results
// (function_call_output, and unknown items whose type ends in "_output")
// join the previous turn when it holds only results; otherwise they open a
// user turn.
//
// Item fields with no neutral slot (id, status, ...) ride in the Extra of
// the item's first block. Function tools are Tools; a built-in tool
// (web_search, file_search, mcp, ...) is a Tool whose Type is its wire type,
// its other fields in Extra. A tool_choice naming a built-in tool is kept in
// Request.Extra. reasoning.effort is Effort. A request naming
// previous_response_id or conversation (both kept in Extra) has its earlier
// turns on the vendor: Extra[llm.HistoryKey] is llm.HistoryServerSide.
// Unknown top-level fields go to Request.Extra.
type ResponsesReader struct{}

func (ResponsesReader) Name() string { return ResponsesReaderName }

// DecodeRequest reads a Responses API request body strictly.
func (ResponsesReader) DecodeRequest(body []byte) (*llm.Request, error) {
	if err := strictjson.Check(body); err != nil {
		return nil, &llm.RequestError{Err: fmt.Errorf("invalid request body: %v", err)}
	}
	req, err := readResponsesRequest(body)
	if err != nil {
		return nil, &llm.RequestError{Err: err}
	}
	return req, nil
}

// DecodeResponse reads a non-stream Responses API response body: its output
// items, read as input items are, are the answer's blocks.
func (ResponsesReader) DecodeResponse(body []byte) (*llm.Response, error) {
	if len(body) > llm.MaxBodyBytes {
		return nil, llm.ErrFrameTooLarge
	}
	if err := strictjson.Check(body); err != nil {
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	out, err := readResponsesResponse(body)
	if err != nil {
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	return out, nil
}

// NewResponseDecoder reads Responses API SSE, over frames that passed the
// strict check.
//
// Text (response.output_text.delta, response.refusal.delta) and reasoning
// (response.reasoning_summary_text.delta, response.reasoning_text.delta)
// stream through as they arrive, one block per content part or reasoning
// item. A function call is buffered until its response.output_item.done
// (or the end of the response) and emitted as one tool_use block with its
// whole input, as the Chat decoder does; each is capped at
// llm.MaxFrameBytes and all of them at llm.MaxBufferedBytes. Another item
// type finished by response.output_item.done is one block kept whole in
// Raw. response.completed and response.incomplete end the message;
// response.failed and error are an error event.
func (ResponsesReader) NewResponseDecoder(r io.Reader) llm.StreamDecoder {
	return &responsesDecoder{
		r:      sse.NewReader(sse.Checked(r, llm.MaxFrameBytes, llm.ErrFrameTooLarge, strictjson.Check), llm.MaxFrameBytes, llm.ErrFrameTooLarge),
		byItem: map[string]*pendingCall{},
	}
}

// reserveHistory refuses a body that sets llm.HistoryKey itself.
func reserveHistory(top map[string]json.RawMessage) error {
	if _, ok := top[llm.HistoryKey]; ok {
		return fmt.Errorf("%s: reserved", llm.HistoryKey)
	}
	return nil
}

// markHistory returns extra with llm.HistoryKey set to value.
func markHistory(extra map[string]json.RawMessage, value string) map[string]json.RawMessage {
	if extra == nil {
		extra = map[string]json.RawMessage{}
	}
	extra[llm.HistoryKey], _ = json.Marshal(value)
	return extra
}

func readResponsesRequest(body []byte) (*llm.Request, error) {
	top, err := object(body)
	if err != nil {
		return nil, fmt.Errorf("invalid request body: %v", err)
	}
	if err := reserveHistory(top); err != nil {
		return nil, err
	}
	req := &llm.Request{}
	var instructions, input, toolChoice json.RawMessage
	var tools []json.RawMessage
	var parallel *bool
	var reasoning map[string]json.RawMessage
	for _, f := range []struct {
		key string
		dst any
	}{
		{"model", &req.Model}, {"instructions", &instructions}, {"input", &input}, {"tools", &tools},
		{"tool_choice", &toolChoice}, {"max_output_tokens", &req.MaxTokens}, {"temperature", &req.Temperature},
		{"top_p", &req.TopP}, {"stream", &req.Stream}, {"parallel_tool_calls", &parallel}, {"reasoning", &reasoning},
	} {
		if err := take(top, f.key, f.dst); err != nil {
			return nil, err
		}
	}
	if len(instructions) > 0 {
		var s string
		if json.Unmarshal(instructions, &s) == nil {
			req.System = []llm.Block{{Type: llm.BlockText, Text: s}}
		} else {
			// An array of input items (a stored prompt's expansion): all of
			// it is the system prompt.
			var items []json.RawMessage
			if json.Unmarshal(instructions, &items) != nil {
				return nil, errors.New("instructions: must be a string or an array of input items")
			}
			inner := &llm.Request{}
			if err := readItems(inner, items, "instructions"); err != nil {
				return nil, err
			}
			req.System = append(req.System, inner.System...)
			for _, m := range inner.Messages {
				req.System = append(req.System, m.Content...)
			}
		}
	}
	if len(input) > 0 {
		var s string
		if json.Unmarshal(input, &s) == nil {
			req.Messages = []llm.Message{{Role: "user", Content: []llm.Block{{Type: llm.BlockText, Text: s}}}}
		} else {
			var items []json.RawMessage
			if json.Unmarshal(input, &items) != nil {
				return nil, errors.New("input: must be a string or an array of input items")
			}
			if err := readItems(req, items, "input"); err != nil {
				return nil, err
			}
		}
	}
	for i, raw := range tools {
		t, err := readResponsesTool(raw)
		if err != nil {
			return nil, fmt.Errorf("tools.%d: %v", i, err)
		}
		req.Tools = append(req.Tools, t)
	}
	if len(toolChoice) > 0 {
		tc, err := readResponsesToolChoice(toolChoice)
		if err != nil {
			return nil, fmt.Errorf("tool_choice: %v", err)
		}
		if tc == nil {
			top["tool_choice"] = toolChoice // a built-in tool or allowed_tools
		}
		req.ToolChoice = tc
	}
	if parallel != nil && !*parallel {
		if req.ToolChoice == nil {
			req.ToolChoice = &llm.ToolChoice{Type: "auto"}
		}
		req.ToolChoice.DisableParallelToolUse = true
	}
	if err := take(reasoning, "effort", &req.Effort); err != nil {
		return nil, fmt.Errorf("reasoning.%v", err)
	}
	if len(reasoning) > 0 { // summary, ...
		top["reasoning"], _ = json.Marshal(reasoning)
	}
	req.Extra = nonEmpty(top)
	if !isNull(top["previous_response_id"]) || !isNull(top["conversation"]) {
		req.Extra = markHistory(req.Extra, llm.HistoryServerSide)
	}
	return req, nil
}

// readItems appends Responses input items to req (see ResponsesReader). path
// prefixes errors ("input").
func readItems(req *llm.Request, items []json.RawMessage, path string) error {
	leading := len(req.Messages) == 0 // still in the leading system/developer messages
	for i, raw := range items {
		at := fmt.Sprintf("%s.%d", path, i)
		it, err := object(raw)
		if err != nil {
			return fmt.Errorf("%s: must be an object", at)
		}
		var typ string
		if err := take(it, "type", &typ); err != nil {
			return fmt.Errorf("%s.%v", at, err)
		}
		if _, hasRole := it["role"]; typ == "" && hasRole {
			typ = "message" // an easy input message
		}
		switch typ {
		case "":
			return fmt.Errorf("%s.type: required", at)
		case "message":
			var role string
			if err := take(it, "role", &role); err != nil {
				return fmt.Errorf("%s.%v", at, err)
			}
			switch role {
			case "user", "assistant", "system", "developer":
			case "":
				return fmt.Errorf("%s.role: required", at)
			default: // roles are refused; item types are not (see ResponsesReader)
				return fmt.Errorf("%s.role: unknown role %q", at, role)
			}
			blocks, err := readItemMessage(it)
			if err != nil {
				return fmt.Errorf("%s.%v", at, err)
			}
			switch role {
			case "system", "developer":
				if leading {
					req.System = append(req.System, blocks...)
					continue
				}
				addTurn(req, "system", blocks, false)
			case "assistant":
				addTurn(req, "assistant", blocks, true)
			default:
				addTurn(req, "user", blocks, false)
			}
		case "function_call":
			b, err := readFunctionCall(it)
			if err != nil {
				return fmt.Errorf("%s.%v", at, err)
			}
			addTurn(req, "assistant", []llm.Block{b}, true)
		case "function_call_output":
			b, err := readFunctionCallOutput(it)
			if err != nil {
				return fmt.Errorf("%s.%v", at, err)
			}
			addResult(req, b)
		case "reasoning":
			b, err := readReasoning(it)
			if err != nil {
				return fmt.Errorf("%s.%v", at, err)
			}
			addTurn(req, "assistant", []llm.Block{b}, true)
		default:
			b := llm.Block{Type: llm.BlockType(typ), Raw: raw}
			if isResult(b) {
				addResult(req, b)
			} else {
				addTurn(req, "assistant", []llm.Block{b}, true)
			}
		}
		leading = false
	}
	return nil
}

// addTurn appends blocks as a turn of role, or to the previous turn when
// join is set and that turn has the same role.
func addTurn(req *llm.Request, role string, blocks []llm.Block, join bool) {
	if n := len(req.Messages); join && n > 0 && req.Messages[n-1].Role == role {
		req.Messages[n-1].Content = append(req.Messages[n-1].Content, blocks...)
		return
	}
	req.Messages = append(req.Messages, llm.Message{Role: role, Content: blocks})
}

// addResult appends a result block to the previous turn when that turn holds
// only results, else as a new user turn.
func addResult(req *llm.Request, b llm.Block) {
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "user" {
		only := true
		for _, c := range req.Messages[n-1].Content {
			only = only && isResult(c)
		}
		if only {
			req.Messages[n-1].Content = append(req.Messages[n-1].Content, b)
			return
		}
	}
	req.Messages = append(req.Messages, llm.Message{Role: "user", Content: []llm.Block{b}})
}

func isResult(b llm.Block) bool {
	return b.Type == llm.BlockToolResult || (b.Raw != nil && strings.HasSuffix(string(b.Type), "_output"))
}

// withExtra moves m's remaining fields into the Extra of blocks[0] (adding an
// empty text block when there is none).
func withExtra(blocks []llm.Block, m map[string]json.RawMessage) []llm.Block {
	if len(m) == 0 {
		return blocks
	}
	if len(blocks) == 0 {
		blocks = []llm.Block{{Type: llm.BlockText}}
	}
	if blocks[0].Extra == nil {
		blocks[0].Extra = map[string]json.RawMessage{}
	}
	for k, v := range m {
		blocks[0].Extra[k] = v
	}
	return blocks
}

// readItemMessage reads a message item's content (type and role already
// removed).
func readItemMessage(it map[string]json.RawMessage) ([]llm.Block, error) {
	content, ok := it["content"]
	delete(it, "content")
	if !ok || isNull(content) {
		return nil, errors.New("content: required")
	}
	blocks, err := readItemContent(content)
	if err != nil {
		return nil, err
	}
	return withExtra(blocks, it), nil
}

// readItemContent reads a string (one text block) or an array of parts.
func readItemContent(raw json.RawMessage) ([]llm.Block, error) {
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
		b, err := readItemPart(p)
		if err != nil {
			return nil, fmt.Errorf("content.%d: %v", i, err)
		}
		out = append(out, b)
	}
	return out, nil
}

func readItemPart(raw json.RawMessage) (llm.Block, error) {
	p, err := object(raw)
	if err != nil {
		return llm.Block{}, errors.New("must be an object")
	}
	var typ string
	if err := take(p, "type", &typ); err != nil {
		return llm.Block{}, err
	}
	switch typ {
	case "input_text", "output_text":
		b := llm.Block{Type: llm.BlockText}
		if err := take(p, "text", &b.Text); err != nil {
			return llm.Block{}, err
		}
		b.Extra = nonEmpty(p) // annotations, logprobs
		return b, nil
	case "input_image":
		var url, id string
		if err := take(p, "image_url", &url); err != nil {
			return llm.Block{}, err
		}
		if err := take(p, "file_id", &id); err != nil {
			return llm.Block{}, err
		}
		b := llm.Block{Type: llm.BlockImage}
		switch {
		case url != "" && id != "":
			return llm.Block{}, errors.New("image_url and file_id: only one may be set")
		case url != "":
			b.Source = dataURLSource(url)
		case id != "":
			b.Source = &llm.Source{Type: "file", FileID: id}
		default:
			return llm.Block{}, errors.New("image_url or file_id required")
		}
		b.Extra = nonEmpty(p) // detail
		return b, nil
	case "input_file":
		var id, data, url string
		for key, dst := range map[string]*string{"file_id": &id, "file_data": &data, "file_url": &url} {
			if err := take(p, key, dst); err != nil {
				return llm.Block{}, err
			}
		}
		set := 0
		for _, v := range []string{id, data, url} {
			if v != "" {
				set++
			}
		}
		if set != 1 {
			return llm.Block{}, errors.New("exactly one of file_id, file_data and file_url required")
		}
		b := llm.Block{Type: llm.BlockDocument}
		switch {
		case id != "":
			b.Source = &llm.Source{Type: "file", FileID: id}
		case url != "":
			b.Source = &llm.Source{Type: "url", URL: url}
		case strings.HasPrefix(data, "data:"):
			b.Source = dataURLSource(data)
		default:
			b.Source = &llm.Source{Type: "base64", Data: data}
		}
		b.Extra = nonEmpty(p) // filename
		return b, nil
	case "":
		return llm.Block{}, errors.New("type: required")
	}
	// refusal, input_audio, ...: kept whole, with the text a reader can use.
	b := llm.Block{Type: llm.BlockType(typ), Raw: raw}
	_ = json.Unmarshal(p["text"], &b.Text)
	if typ == "refusal" {
		_ = json.Unmarshal(p["refusal"], &b.Text)
	}
	return b, nil
}

// readFunctionCall reads a function_call item (type removed).
func readFunctionCall(it map[string]json.RawMessage) (llm.Block, error) {
	b := llm.Block{Type: llm.BlockToolUse}
	var args json.RawMessage
	for key, dst := range map[string]any{"call_id": &b.ID, "name": &b.Name, "arguments": &args} {
		if err := take(it, key, dst); err != nil {
			return llm.Block{}, err
		}
	}
	if b.ID == "" {
		return llm.Block{}, errors.New("call_id: required")
	}
	if b.Name == "" {
		return llm.Block{}, errors.New("name: required")
	}
	b.Input = toolInput(argString(args))
	b.Extra = nonEmpty(it) // id, status
	return b, nil
}

// readFunctionCallOutput reads a function_call_output item (type removed).
func readFunctionCallOutput(it map[string]json.RawMessage) (llm.Block, error) {
	b := llm.Block{Type: llm.BlockToolResult}
	if err := take(it, "call_id", &b.ToolUseID); err != nil {
		return llm.Block{}, err
	}
	if b.ToolUseID == "" {
		return llm.Block{}, errors.New("call_id: required")
	}
	if output, ok := it["output"]; ok {
		delete(it, "output")
		if !isNull(output) {
			cs, err := readItemContent(output)
			if err != nil {
				return llm.Block{}, fmt.Errorf("output: %v", err)
			}
			b.Content = cs
		}
	}
	b.Extra = nonEmpty(it) // id, status
	return b, nil
}

// readReasoning reads a reasoning item (type removed): its summary texts,
// joined by blank lines, are the thinking; encrypted_content and the rest
// stay in Extra (the signature slot is Anthropic's).
func readReasoning(it map[string]json.RawMessage) (llm.Block, error) {
	var summary []map[string]json.RawMessage
	if err := take(it, "summary", &summary); err != nil {
		return llm.Block{}, err
	}
	texts := make([]string, 0, len(summary))
	for _, s := range summary {
		var t string
		_ = json.Unmarshal(s["text"], &t)
		texts = append(texts, t)
	}
	return llm.Block{Type: llm.BlockThinking, Thinking: strings.Join(texts, "\n\n"), Extra: nonEmpty(it)}, nil
}

func readResponsesTool(raw json.RawMessage) (llm.Tool, error) {
	m, err := object(raw)
	if err != nil {
		return llm.Tool{}, errors.New("must be an object")
	}
	var t llm.Tool
	if err := take(m, "type", &t.Type); err != nil {
		return llm.Tool{}, err
	}
	if t.Type == "" {
		return llm.Tool{}, errors.New("type: required")
	}
	if err := take(m, "name", &t.Name); err != nil {
		return llm.Tool{}, err
	}
	if t.Type != "function" {
		// A built-in tool (web_search, file_search, mcp, custom, ...): kept
		// under its wire type, its fields in Extra.
		t.Extra = nonEmpty(m)
		return t, nil
	}
	t.Type = ""
	if t.Name == "" {
		return llm.Tool{}, errors.New("name: required")
	}
	if err := take(m, "description", &t.Description); err != nil {
		return llm.Tool{}, err
	}
	if err := take(m, "parameters", &t.InputSchema); err != nil {
		return llm.Tool{}, err
	}
	t.Extra = nonEmpty(m) // strict
	return t, nil
}

// readResponsesToolChoice maps tool_choice; nil (and no error) for a choice
// the neutral form has no slot for (a built-in tool, allowed_tools), which
// the caller keeps in Extra.
func readResponsesToolChoice(raw json.RawMessage) (*llm.ToolChoice, error) {
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
	var typ, name string
	if json.Unmarshal(obj["type"], &typ) != nil || typ == "" {
		return nil, errors.New("type: required")
	}
	if typ != "function" {
		return nil, nil
	}
	_ = json.Unmarshal(obj["name"], &name)
	if name == "" || len(obj) != 2 {
		return nil, fmt.Errorf("unsupported value %s", raw)
	}
	return &llm.ToolChoice{Type: "tool", Name: name}, nil
}

// --- responses ---

// responsesUsage is Responses API usage: input_tokens include the cached
// prefix, the neutral InputTokens exclude it.
type responsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (u *responsesUsage) neutral() llm.Usage {
	if u == nil {
		return llm.Usage{}
	}
	cached := u.InputTokensDetails.CachedTokens
	return llm.Usage{InputTokens: max(0, u.InputTokens-cached), OutputTokens: u.OutputTokens, CacheReadTokens: cached}
}

// responsesStop maps a response's status: completed is tool_use when it
// called a function, else end_turn; incomplete is max_tokens for
// max_output_tokens, refusal for content_filter, else end_turn. Any other
// status (failed, cancelled, in_progress, queued) has not finished: "".
func responsesStop(status, reason string, sawTool bool) string {
	switch status {
	case "completed":
		if sawTool {
			return "tool_use"
		}
		return "end_turn"
	case "incomplete":
		switch reason {
		case "max_output_tokens":
			return "max_tokens"
		case "content_filter":
			return "refusal"
		}
		return "end_turn"
	}
	return ""
}

type responsesHeader struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *responsesUsage `json:"usage"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *responsesHeader) reason() string {
	if h.IncompleteDetails == nil {
		return ""
	}
	return h.IncompleteDetails.Reason
}

func readResponsesResponse(body []byte) (*llm.Response, error) {
	if _, err := object(body); err != nil {
		return nil, err
	}
	var in struct {
		responsesHeader
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	answer := &llm.Request{}
	if err := readItems(answer, in.Output, "output"); err != nil {
		return nil, err
	}
	out := &llm.Response{ID: in.ID, Model: in.Model, Role: "assistant", Content: []llm.Block{}, Usage: in.Usage.neutral()}
	out.Content = append(out.Content, answer.System...)
	sawTool := false
	for _, m := range answer.Messages {
		for _, b := range m.Content {
			sawTool = sawTool || b.Type == llm.BlockToolUse
			out.Content = append(out.Content, b)
		}
	}
	out.StopReason = responsesStop(in.Status, in.reason(), sawTool)
	return out, nil
}

// --- stream ---

type responsesDecoder struct {
	r       *sse.Reader
	queue   []llm.Event
	err     error // returned once the queue drains (io.EOF when finished)
	started bool

	next  int // next block index
	open  bool
	kind  llm.BlockType // of the open block
	key   string        // the part (or reasoning item) the open block reads
	index int           // of the open block

	calls    []*pendingCall // first-seen order
	byItem   map[string]*pendingCall
	buffered int
	sawTool  bool
}

// pendingCall is one function call not emitted yet.
type pendingCall struct {
	id, name string
	args     strings.Builder
	done     bool
}

type responsesEvent struct {
	Type         string           `json:"type"`
	Response     *responsesHeader `json:"response"`
	OutputIndex  *int             `json:"output_index"`
	ContentIndex *int             `json:"content_index"`
	ItemID       string           `json:"item_id"`
	Item         json.RawMessage  `json:"item"`
	Delta        json.RawMessage  `json:"delta"`
	Arguments    *string          `json:"arguments"`
	Code         json.RawMessage  `json:"code"`
	Message      string           `json:"message"`
}

func (d *responsesDecoder) Next() (llm.Event, error) {
	for len(d.queue) == 0 {
		if d.err != nil {
			return llm.Event{}, d.err
		}
		d.read()
	}
	ev := d.queue[0]
	d.queue = d.queue[1:]
	return ev, nil
}

func (d *responsesDecoder) push(ev llm.Event) { d.queue = append(d.queue, ev) }

func (d *responsesDecoder) read() {
	_, data, err := d.r.Next()
	switch {
	case errors.Is(err, io.EOF):
		d.err = io.ErrUnexpectedEOF // no response.completed: cut
	case err != nil:
		d.err = err
	default:
		d.event(data)
	}
}

func (d *responsesDecoder) start(h *responsesHeader) {
	if d.started {
		return
	}
	d.started = true
	msg := &llm.Response{Role: "assistant", Content: []llm.Block{}}
	if h != nil {
		msg.ID, msg.Model = h.ID, h.Model
	}
	d.push(llm.Event{Type: llm.EventMessageStart, Message: msg})
}

func (d *responsesDecoder) event(data string) {
	var ev responsesEvent
	if json.Unmarshal([]byte(data), &ev) != nil {
		return // not an event this decoder knows
	}
	if ev.Type == "error" {
		d.fail(jsonString(ev.Code), ev.Code, ev.Message)
		return
	}
	d.start(ev.Response)
	switch ev.Type {
	case "response.output_text.delta", "response.refusal.delta":
		d.delta(llm.BlockText, partKey(ev), jsonString(ev.Delta))
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		d.delta(llm.BlockThinking, ev.ItemID, jsonString(ev.Delta))
	case "response.content_part.done":
		if d.open && d.key == partKey(ev) {
			d.closeBlock()
		}
	case "response.output_item.added":
		if it, ok := readStreamItem(ev.Item); ok && it.Type == "function_call" {
			c := d.call(ev)
			if c == nil {
				return
			}
			c.id, c.name = firstNonEmpty(c.id, it.CallID), firstNonEmpty(c.name, it.Name)
			d.addArgs(c, it.Arguments, false)
		}
	case "response.function_call_arguments.delta":
		if c := d.call(ev); c != nil {
			d.addArgs(c, jsonString(ev.Delta), false)
		}
	case "response.function_call_arguments.done":
		if c := d.call(ev); c != nil && ev.Arguments != nil {
			d.addArgs(c, *ev.Arguments, true)
		}
	case "response.output_item.done":
		d.itemDone(ev)
	case "response.completed", "response.incomplete":
		h := ev.Response
		if h == nil {
			h = &responsesHeader{Status: strings.TrimPrefix(ev.Type, "response.")}
		}
		d.end(h)
	case "response.failed", "response.cancelled":
		msg, code := "response "+strings.TrimPrefix(ev.Type, "response."), ""
		if ev.Response != nil && ev.Response.Error != nil {
			msg, code = firstNonEmpty(ev.Response.Error.Message, msg), ev.Response.Error.Code
		}
		d.fail(code, nil, msg)
	}
}

// partKey names the content part a text delta extends.
func partKey(ev responsesEvent) string {
	key := ev.ItemID
	if ev.ContentIndex != nil {
		key += "/" + strconv.Itoa(*ev.ContentIndex)
	}
	return key
}

// streamItem is the part of a stream event's item the decoder reads.
type streamItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// readStreamItem decodes an event's item, reporting success.
func readStreamItem(raw json.RawMessage) (streamItem, bool) {
	var it streamItem
	return it, len(raw) > 0 && json.Unmarshal(raw, &it) == nil
}

// call finds (or registers) the pending call an event names, by item id,
// else by output index; nil when the event names neither.
func (d *responsesDecoder) call(ev responsesEvent) *pendingCall {
	key := ev.ItemID
	if it, ok := readStreamItem(ev.Item); key == "" && ok {
		key = it.ID
	}
	if key == "" && ev.OutputIndex != nil {
		key = "#" + strconv.Itoa(*ev.OutputIndex)
	}
	if key == "" {
		return nil
	}
	if c := d.byItem[key]; c != nil {
		return c
	}
	if ev.OutputIndex != nil { // registered by index before its id was known
		if c := d.byItem["#"+strconv.Itoa(*ev.OutputIndex)]; c != nil {
			d.byItem[key] = c
			return c
		}
	}
	c := &pendingCall{}
	d.byItem[key] = c
	if ev.OutputIndex != nil {
		d.byItem["#"+strconv.Itoa(*ev.OutputIndex)] = c
	}
	d.calls = append(d.calls, c)
	return c
}

// addArgs appends (or, with replace, sets) a call's arguments, within the
// buffer caps.
func (d *responsesDecoder) addArgs(c *pendingCall, args string, replace bool) {
	if replace {
		d.buffered -= c.args.Len()
		c.args.Reset()
	}
	if c.args.Len()+len(args) > llm.MaxFrameBytes || d.buffered+len(args) > llm.MaxBufferedBytes {
		d.err = llm.ErrFrameTooLarge
		return
	}
	c.args.WriteString(args)
	d.buffered += len(args)
}

func (d *responsesDecoder) itemDone(ev responsesEvent) {
	it, ok := readStreamItem(ev.Item)
	if !ok {
		return
	}
	switch it.Type {
	case "function_call":
		c := d.call(ev)
		if c == nil {
			return
		}
		c.id, c.name = firstNonEmpty(it.CallID, c.id), firstNonEmpty(it.Name, c.name)
		if it.Arguments != "" {
			if d.addArgs(c, it.Arguments, true); d.err != nil {
				return
			}
		}
		d.emitCall(c)
	case "message", "reasoning":
		d.closeBlock()
	case "":
	default: // web_search_call, mcp_call, ...: one block, kept whole
		d.closeBlock()
		d.openBlock(llm.BlockType(it.Type), "", &llm.Block{Type: llm.BlockType(it.Type), Raw: append(json.RawMessage(nil), ev.Item...)})
		d.closeBlock()
	}
}

func (d *responsesDecoder) delta(kind llm.BlockType, key, text string) {
	if text == "" {
		return
	}
	if !d.open || d.kind != kind || d.key != key {
		d.closeBlock()
		d.openBlock(kind, key, &llm.Block{Type: kind})
	}
	delta := &llm.Delta{Type: llm.DeltaText, Text: text}
	if kind == llm.BlockThinking {
		delta = &llm.Delta{Type: llm.DeltaThinking, Thinking: text}
	}
	d.push(llm.Event{Type: llm.EventContentBlockDelta, Index: d.index, Delta: delta})
}

func (d *responsesDecoder) openBlock(kind llm.BlockType, key string, blk *llm.Block) {
	d.open, d.kind, d.key, d.index = true, kind, key, d.next
	d.next++
	d.push(llm.Event{Type: llm.EventContentBlockStart, Index: d.index, Block: blk})
}

func (d *responsesDecoder) closeBlock() {
	if d.open {
		d.push(llm.Event{Type: llm.EventContentBlockStop, Index: d.index})
		d.open = false
	}
}

// emitCall emits a finished call as one tool_use block (once; a call with
// no name cannot be executed and is dropped).
func (d *responsesDecoder) emitCall(c *pendingCall) {
	if c.done {
		return
	}
	c.done = true
	d.buffered -= c.args.Len()
	if c.name == "" {
		return
	}
	d.sawTool = true
	d.closeBlock()
	d.openBlock(llm.BlockToolUse, "", &llm.Block{Type: llm.BlockToolUse, ID: firstNonEmpty(c.id, newID("call_")), Name: c.name})
	d.push(llm.Event{Type: llm.EventContentBlockDelta, Index: d.index,
		Delta: &llm.Delta{Type: llm.DeltaInputJSON, PartialJSON: string(toolInput(c.args.String()))}})
	d.closeBlock()
}

// end emits the calls not finished yet and finishes the message.
func (d *responsesDecoder) end(h *responsesHeader) {
	d.closeBlock()
	for _, c := range d.calls {
		d.emitCall(c)
	}
	u := h.Usage.neutral()
	d.push(llm.Event{Type: llm.EventMessageDelta, StopReason: firstNonEmpty(responsesStop(h.Status, h.reason(), d.sawTool), "end_turn"), Usage: &u})
	d.push(llm.Event{Type: llm.EventMessageStop})
	d.err = io.EOF
}

// fail ends the stream with an error event.
func (d *responsesDecoder) fail(code string, raw json.RawMessage, msg string) {
	d.push(llm.Event{Type: llm.EventError, Error: &llm.Error{Type: streamErrorType(code, raw), Message: firstNonEmpty(msg, "upstream error")}})
	d.err = io.EOF
}
