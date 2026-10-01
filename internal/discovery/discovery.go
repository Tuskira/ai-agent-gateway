// Package discovery detects the skills and MCP servers an agent ACTUALLY
// USED, from the LLM response the gateway relayed (or an interceptor
// captured): a model asking to call the "Skill" tool, or a tool named
// mcp__<server>__<tool>, is proof the client used them, whether or not they
// are registered with this gateway. Only names are available.
//
// Only RESPONSE bytes are scanned, never request history: history replays
// every past tool call on every turn and would count one use many times.
// These dialects are understood, as the gateway emits them to the client
// (a translated response is already in the client's dialect). The format is
// sniffed from the bytes, so one entry point serves every route and the
// interceptor ingest path:
//
//   - Anthropic Messages (also Bedrock InvokeModel on an Anthropic model):
//     JSON content[] tool_use blocks, and SSE content_block_start (tool name)
//   - input_json_delta (input, accumulated per block index) +
//     content_block_stop.
//   - Bedrock InvokeModelWithResponseStream and ConverseStream: AWS
//     event-stream binary frames. A "chunk" frame carries {"bytes": base64 of
//     an Anthropic stream event}; Converse frames carry contentBlockStart /
//     contentBlockDelta (toolUse.input fragments) / contentBlockStop.
//     Converse JSON: output.message.content[].toolUse.
//   - OpenAI chat completions: JSON choices[].message.tool_calls, and stream
//     choices[].delta.tool_calls (name + argument fragments per tool index).
//   - OpenAI Responses: JSON output[] function_call (name, arguments string)
//     and mcp_call (server_label, name); stream response.output_item.added /
//     response.function_call_arguments.delta|done / response.output_item.done.
//   - Gemini: candidates[].content.parts[].functionCall {name, args}, as
//     generateContent JSON, an alt=sse stream, or the JSON-array stream.
//
// Scanner is an io.Writer meant to sit in the response tee next to usage
// extraction: it never returns an error, never panics out of Write, and keeps
// bounded memory, so it cannot affect the relay. Malformed input is ignored.
package discovery

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"strings"
)

// Bounds. They cap what one call can add to a row, and what the scanner
// holds in memory while a stream is in flight.
const (
	MaxSkills        = 50
	MaxMCPRefs       = 200
	MaxNameLen       = 128
	MaxToolLen       = 256
	maxInputPerBlock = 64 << 10 // tool input bytes kept per block
	maxOpenBlocks    = 256
	maxLineBytes     = 1 << 20 // one SSE line
	maxFrameBytes    = 1 << 20 // one event-stream frame; a bigger one is skipped
	maxJSONBodyBytes = 8 << 20 // whole non-stream body
	skillToolName    = "Skill"
	mcpPrefix        = "mcp__"
	mcpSeparator     = "__" // also the stored "server__tool" separator
)

// Result is what one call used. MCPTools holds "server__tool" values with the
// server lowercased; both slices are deduplicated, in first-seen order.
type Result struct {
	Skills   []string
	MCPTools []string
}

// Empty reports whether nothing was found.
func (r Result) Empty() bool { return len(r.Skills) == 0 && len(r.MCPTools) == 0 }

// Scan scans one complete captured response body (any supported format).
func Scan(body []byte) Result {
	s := NewScanner()
	_, _ = s.Write(body)
	return s.Result()
}

// Scanner incrementally scans a response as it is written. The zero value is
// not usable; call NewScanner.
type Scanner struct {
	mode      scanMode
	line      []byte // partial SSE line
	skipLine  bool   // current SSE line exceeded maxLineBytes
	json      []byte // whole non-stream body
	jsonOver  bool
	esBuf     []byte         // partial event-stream frame(s)
	esSkip    int            // bytes of an oversized frame still to drop
	esDead    bool           // event-stream framing lost; ignore the rest
	respIDs   map[string]int // Responses item_id -> output_index
	blocks    map[blockKey]*block
	skillSet  map[string]struct{}
	mcpSet    map[string]struct{}
	res       Result
	finalized bool
}

type scanMode int

const (
	modeUnknown scanMode = iota
	modeJSON             // a JSON object, or a JSON array (Gemini's array stream)
	modeSSE              // text/event-stream lines
	modeES               // AWS event-stream binary frames
)

// Block namespaces (blockKey.choice): tool blocks of different dialects
// never collide. Non-negative values are OpenAI chat choice indexes.
const (
	nsAnthropic = -1
	nsResponses = -2
	nsConverse  = -3
)

type blockKey struct{ choice, index int }

type block struct {
	name string
	in   []byte
	over bool
}

// NewScanner returns an empty Scanner.
func NewScanner() *Scanner {
	return &Scanner{skillSet: map[string]struct{}{}, mcpSet: map[string]struct{}{}}
}

// Write implements io.Writer. It always reports a full write and never fails.
func (s *Scanner) Write(p []byte) (n int, err error) {
	n = len(p)
	defer func() { _ = recover() }() // never let a scan bug touch the relay
	if s.finalized || n == 0 {
		return n, nil
	}
	if s.mode == modeUnknown {
		t := bytes.TrimLeft(p, " \t\r\n\xef\xbb\xbf")
		if len(t) == 0 {
			return n, nil
		}
		switch {
		case t[0] == '{' || t[0] == '[':
			s.mode = modeJSON
		case p[0] == 0: // event-stream: 4-byte big-endian length, always < 16 MiB
			s.mode = modeES
		default:
			s.mode = modeSSE
		}
	}
	switch s.mode {
	case modeJSON:
		if s.jsonOver {
			return n, nil
		}
		if len(s.json)+len(p) > maxJSONBodyBytes {
			s.jsonOver, s.json = true, nil
			return n, nil
		}
		s.json = append(s.json, p...)
	case modeES:
		s.writeES(p)
	default:
		s.writeSSE(p)
	}
	return n, nil
}

// Result finalizes the scan (closing any tool block still open) and returns
// what was found. Safe to call more than once.
func (s *Scanner) Result() Result {
	if !s.finalized {
		func() {
			defer func() { _ = recover() }()
			switch s.mode {
			case modeJSON:
				if !s.jsonOver {
					s.scanJSON(s.json)
				}
			case modeSSE:
				if len(s.line) > 0 && !s.skipLine {
					s.sseLine(s.line)
				}
				s.flushBlocks()
			case modeES:
				s.flushBlocks()
			}
		}()
		s.finalized, s.json, s.line, s.blocks, s.esBuf, s.respIDs = true, nil, nil, nil, nil, nil
	}
	return s.res
}

/* ---- SSE ------------------------------------------------------------- */

func (s *Scanner) writeSSE(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if !s.skipLine {
				if len(s.line)+len(p) > maxLineBytes {
					s.skipLine, s.line = true, nil
				} else {
					s.line = append(s.line, p...)
				}
			}
			return
		}
		if !s.skipLine && len(s.line)+i <= maxLineBytes {
			s.line = append(s.line, p[:i]...)
			s.sseLine(s.line)
		}
		s.line, s.skipLine = s.line[:0], false
		p = p[i+1:]
	}
}

func (s *Scanner) sseLine(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[len("data:"):])
	if len(data) == 0 || data[0] != '{' {
		return
	}
	s.event(data)
}

// Cheap prefilter: an event only matters when it carries one of these.
var eventMarkers = [][]byte{
	[]byte("tool_use"), []byte("input_json_delta"), []byte("content_block_stop"), []byte("tool_calls"),
	[]byte("function_call"), []byte("mcp_call"), []byte("functionCall"), []byte("response.output_item"),
}

// event handles one JSON stream event: an SSE data payload, or the decoded
// bytes of a Bedrock chunk. Anthropic, OpenAI chat, OpenAI Responses and
// Gemini events are told apart by their own shape.
func (s *Scanner) event(data []byte) {
	found := false
	for _, m := range eventMarkers {
		if bytes.Contains(data, m) {
			found = true
			break
		}
	}
	if !found {
		return
	}
	var ev struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta   json.RawMessage `json:"delta"` // Anthropic: object; Responses: string
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				ToolCalls []oaToolCall `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Candidates  []geminiCandidate `json:"candidates"`
		OutputIndex *int              `json:"output_index"`
		ItemID      string            `json:"item_id"`
		Arguments   string            `json:"arguments"`
		Item        *respItem         `json:"item"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "content_block_start":
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			s.open(blockKey{nsAnthropic, ev.Index}, ev.ContentBlock.Name)
		}
		return
	case "content_block_delta":
		var d struct {
			Type        string `json:"type"`
			PartialJSON string `json:"partial_json"`
		}
		if json.Unmarshal(ev.Delta, &d) == nil && d.Type == "input_json_delta" {
			if b := s.blocks[blockKey{nsAnthropic, ev.Index}]; b != nil {
				b.add(d.PartialJSON)
			}
		}
		return
	case "content_block_stop":
		s.closeBlock(blockKey{nsAnthropic, ev.Index})
		return
	case "response.output_item.added":
		s.respAdded(ev.OutputIndex, ev.Item)
		return
	case "response.function_call_arguments.delta":
		var frag string
		if json.Unmarshal(ev.Delta, &frag) == nil {
			if b := s.respBlock(ev.ItemID, ev.OutputIndex); b != nil {
				b.add(frag)
			}
		}
		return
	case "response.function_call_arguments.done":
		if b := s.respBlock(ev.ItemID, ev.OutputIndex); b != nil && ev.Arguments != "" {
			b.in, b.over = nil, false
			b.add(ev.Arguments)
		}
		return
	case "response.output_item.done":
		s.respDone(ev.OutputIndex, ev.Item)
		return
	}
	for _, c := range ev.Choices {
		for _, tc := range c.Delta.ToolCalls {
			k := blockKey{c.Index, tc.Index}
			b := s.blocks[k]
			if b == nil {
				if b = s.open(k, tc.Function.Name); b == nil {
					continue
				}
			} else if b.name == "" {
				b.name = tc.Function.Name
			}
			b.add(tc.Function.Arguments)
		}
	}
	for _, c := range ev.Candidates {
		s.gemini(c)
	}
}

type oaToolCall struct {
	Index    int `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// respItem is an OpenAI Responses output item (function_call or mcp_call;
// other types are carried but ignored).
type respItem struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	ServerLabel string `json:"server_label"`
	Arguments   string `json:"arguments"`
}

type geminiCandidate struct {
	Content struct {
		Parts []struct {
			FunctionCall *struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			} `json:"functionCall"`
		} `json:"parts"`
	} `json:"content"`
}

// gemini records a candidate's functionCall parts; Gemini sends each call
// whole (name + args object), in both JSON and stream chunks.
func (s *Scanner) gemini(c geminiCandidate) {
	for _, p := range c.Content.Parts {
		if fc := p.FunctionCall; fc != nil {
			s.record(fc.Name, fc.Args, len(fc.Args) > maxInputPerBlock)
		}
	}
}

func (s *Scanner) open(k blockKey, name string) *block {
	if s.blocks == nil {
		s.blocks = map[blockKey]*block{}
	}
	if len(s.blocks) >= maxOpenBlocks {
		return nil
	}
	b := &block{name: name}
	s.blocks[k] = b
	return b
}

func (s *Scanner) closeBlock(k blockKey) {
	if b := s.blocks[k]; b != nil {
		s.record(b.name, b.in, b.over)
		delete(s.blocks, k)
	}
}

func (b *block) add(frag string) {
	if b.over || frag == "" {
		return
	}
	if len(b.in)+len(frag) > maxInputPerBlock {
		b.over, b.in = true, nil
		return
	}
	b.in = append(b.in, frag...)
}

func (s *Scanner) flushBlocks() {
	for _, b := range s.blocks {
		s.record(b.name, b.in, b.over)
	}
	s.blocks = nil
}

/* ---- OpenAI Responses stream ------------------------------------------ */

func (s *Scanner) respAdded(idx *int, it *respItem) {
	if it == nil || idx == nil {
		return
	}
	switch it.Type {
	case "function_call":
		b := s.open(blockKey{nsResponses, *idx}, it.Name)
		if b == nil {
			return
		}
		b.add(it.Arguments)
		if it.ID != "" {
			if s.respIDs == nil {
				s.respIDs = map[string]int{}
			}
			if len(s.respIDs) < maxOpenBlocks {
				s.respIDs[it.ID] = *idx
			}
		}
	case "mcp_call":
		s.recordMCPServer(it.ServerLabel, it.Name)
	}
}

// respBlock finds the open function_call for a delta/done event: by item_id
// first, else output_index.
func (s *Scanner) respBlock(itemID string, idx *int) *block {
	if i, ok := s.respIDs[itemID]; ok && itemID != "" {
		return s.blocks[blockKey{nsResponses, i}]
	}
	if idx != nil {
		return s.blocks[blockKey{nsResponses, *idx}]
	}
	return nil
}

func (s *Scanner) respDone(idx *int, it *respItem) {
	if it == nil {
		return
	}
	switch it.Type {
	case "function_call":
		var b *block
		if idx != nil {
			k := blockKey{nsResponses, *idx}
			b = s.blocks[k]
			delete(s.blocks, k)
		}
		name, args, over := it.Name, []byte(it.Arguments), false
		if b != nil {
			if name == "" {
				name = b.name
			}
			if len(args) == 0 {
				args, over = b.in, b.over
			}
		}
		if len(args) > maxInputPerBlock {
			args, over = nil, true
		}
		s.record(name, args, over)
		delete(s.respIDs, it.ID)
	case "mcp_call":
		s.recordMCPServer(it.ServerLabel, it.Name)
	}
}

/* ---- Bedrock event-stream --------------------------------------------- */

// Event-stream frame layout (all big-endian):
//
//	total length (4) | headers length (4) | prelude CRC (4) |
//	headers | payload | message CRC (4)
const (
	esPrelude = 12
	esTrailer = 4
)

func (s *Scanner) writeES(p []byte) {
	if s.esDead {
		return
	}
	s.esBuf = append(s.esBuf, p...)
	for {
		if s.esSkip > 0 {
			n := min(s.esSkip, len(s.esBuf))
			s.esBuf, s.esSkip = s.esBuf[n:], s.esSkip-n
			if s.esSkip > 0 {
				s.esBuf = s.esBuf[:0]
				return
			}
		}
		if len(s.esBuf) < esPrelude {
			return
		}
		total := int(binary.BigEndian.Uint32(s.esBuf[0:4]))
		hlen := int(binary.BigEndian.Uint32(s.esBuf[4:8]))
		if crc32.ChecksumIEEE(s.esBuf[:8]) != binary.BigEndian.Uint32(s.esBuf[8:12]) ||
			total < esPrelude+esTrailer || hlen > total-esPrelude-esTrailer {
			// Framing is lost: without a valid length there is no way to
			// find the next frame.
			s.esDead, s.esBuf = true, nil
			return
		}
		if total > maxFrameBytes {
			s.esSkip = total
			continue
		}
		if len(s.esBuf) < total {
			return
		}
		frame := s.esBuf[:total]
		if crc32.ChecksumIEEE(frame[:total-esTrailer]) == binary.BigEndian.Uint32(frame[total-esTrailer:]) {
			s.esFrame(frame[esPrelude:esPrelude+hlen], frame[esPrelude+hlen:total-esTrailer])
		}
		s.esBuf = s.esBuf[total:]
		if len(s.esBuf) == 0 {
			s.esBuf = nil
		}
	}
}

// esFrame handles one verified frame. Only string headers are read (the only
// kind Bedrock uses for :message-type / :event-type).
func (s *Scanner) esFrame(headers, payload []byte) {
	var msgType, evType string
	for len(headers) > 0 {
		nl := int(headers[0])
		if len(headers) < 2+nl {
			return
		}
		name, typ := string(headers[1:1+nl]), headers[1+nl]
		headers = headers[2+nl:]
		var size int
		switch typ {
		case 0, 1:
		case 2:
			size = 1
		case 3:
			size = 2
		case 4:
			size = 4
		case 5, 8:
			size = 8
		case 9:
			size = 16
		case 6, 7:
			if len(headers) < 2 {
				return
			}
			n := int(binary.BigEndian.Uint16(headers))
			if len(headers) < 2+n {
				return
			}
			if typ == 7 {
				switch name {
				case ":message-type":
					msgType = string(headers[2 : 2+n])
				case ":event-type":
					evType = string(headers[2 : 2+n])
				}
			}
			size = 2 + n
		default:
			return
		}
		if len(headers) < size {
			return
		}
		headers = headers[size:]
	}
	if msgType != "event" {
		return
	}
	switch evType {
	case "chunk": // InvokeModelWithResponseStream
		var c struct {
			Bytes string `json:"bytes"`
		}
		if json.Unmarshal(payload, &c) != nil || c.Bytes == "" {
			return
		}
		if dec, err := base64.StdEncoding.DecodeString(c.Bytes); err == nil {
			s.event(dec)
		}
	case "contentBlockStart", "contentBlockDelta", "contentBlockStop": // ConverseStream
		s.converseEvent(evType, payload)
	}
}

// converseEvent handles one ConverseStream event payload:
// contentBlockStart {contentBlockIndex, start:{toolUse:{toolUseId,name}}},
// contentBlockDelta {contentBlockIndex, delta:{toolUse:{input:"<fragment>"}}},
// contentBlockStop {contentBlockIndex}.
func (s *Scanner) converseEvent(evType string, payload []byte) {
	var ev struct {
		Index int `json:"contentBlockIndex"`
		Start *struct {
			ToolUse *struct {
				Name string `json:"name"`
			} `json:"toolUse"`
		} `json:"start"`
		Delta *struct {
			ToolUse *struct {
				Input string `json:"input"`
			} `json:"toolUse"`
		} `json:"delta"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	k := blockKey{nsConverse, ev.Index}
	switch evType {
	case "contentBlockStart":
		if ev.Start != nil && ev.Start.ToolUse != nil {
			s.open(k, ev.Start.ToolUse.Name)
		}
	case "contentBlockDelta":
		if ev.Delta != nil && ev.Delta.ToolUse != nil {
			if b := s.blocks[k]; b != nil {
				b.add(ev.Delta.ToolUse.Input)
			}
		}
	case "contentBlockStop":
		s.closeBlock(k)
	}
}

/* ---- JSON ------------------------------------------------------------ */

var jsonMarkers = [][]byte{
	[]byte("tool_use"), []byte("tool_calls"), []byte("function_call"), []byte("mcp_call"),
	[]byte("functionCall"), []byte("toolUse"),
}

func (s *Scanner) scanJSON(body []byte) {
	found := false
	for _, m := range jsonMarkers {
		if bytes.Contains(body, m) {
			found = true
			break
		}
	}
	if !found {
		return
	}
	if t := bytes.TrimLeft(body, " \t\r\n\xef\xbb\xbf"); len(t) > 0 && t[0] == '[' {
		// Gemini's streamGenerateContent without alt=sse: a JSON array of
		// the same objects generateContent returns.
		var items []json.RawMessage
		if json.Unmarshal(body, &items) == nil {
			for _, it := range items {
				s.scanJSONObject(it)
			}
		}
		return
	}
	s.scanJSONObject(body)
}

func (s *Scanner) scanJSONObject(body []byte) {
	var r struct {
		Content json.RawMessage `json:"content"` // Anthropic: array; OpenAI: string
		Choices []struct {
			Message struct {
				ToolCalls []oaToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Candidates []geminiCandidate `json:"candidates"`
		Output     json.RawMessage   `json:"output"` // Responses: array; Converse: object
	}
	if json.Unmarshal(body, &r) != nil {
		return
	}
	var blocks []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(r.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "tool_use" {
				s.record(b.Name, b.Input, false)
			}
		}
	}
	for _, c := range r.Choices {
		for _, tc := range c.Message.ToolCalls {
			s.record(tc.Function.Name, []byte(tc.Function.Arguments), false)
		}
	}
	for _, c := range r.Candidates {
		s.gemini(c)
	}
	var items []respItem
	if json.Unmarshal(r.Output, &items) == nil { // Responses output[]
		for _, it := range items {
			switch it.Type {
			case "function_call":
				s.record(it.Name, []byte(it.Arguments), len(it.Arguments) > maxInputPerBlock)
			case "mcp_call":
				s.recordMCPServer(it.ServerLabel, it.Name)
			}
		}
		return
	}
	var conv struct { // Converse: output.message.content[].toolUse
		Message struct {
			Content []struct {
				ToolUse *struct {
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"toolUse"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(r.Output, &conv) == nil {
		for _, c := range conv.Message.Content {
			if c.ToolUse != nil {
				s.record(c.ToolUse.Name, c.ToolUse.Input, false)
			}
		}
	}
}

/* ---- extraction ------------------------------------------------------ */

func (s *Scanner) record(name string, input []byte, inputOver bool) {
	switch {
	case name == skillToolName:
		if inputOver || len(input) > maxInputPerBlock {
			return
		}
		if sk := skillName(input); sk != "" {
			if _, dup := s.skillSet[sk]; !dup && len(s.res.Skills) < MaxSkills {
				s.skillSet[sk] = struct{}{}
				s.res.Skills = append(s.res.Skills, sk)
			}
		}
	case strings.HasPrefix(name, mcpPrefix):
		if server, tool, ok := SplitMCPName(name); ok {
			s.addMCP(server, tool)
		}
	}
}

// recordMCPServer records an MCP call that names its server explicitly
// (OpenAI Responses mcp_call: server_label + name), with the same
// normalization and bounds as an mcp__<server>__<tool> tool name.
func (s *Scanner) recordMCPServer(server, tool string) {
	server, tool = strings.ToLower(strings.TrimSpace(server)), strings.TrimSpace(tool)
	if server == "" || tool == "" || len(server) > MaxNameLen || len(tool) > MaxToolLen {
		return
	}
	s.addMCP(server, tool)
}

func (s *Scanner) addMCP(server, tool string) {
	ref := server + mcpSeparator + tool
	if _, dup := s.mcpSet[ref]; !dup && len(s.res.MCPTools) < MaxMCPRefs {
		s.mcpSet[ref] = struct{}{}
		s.res.MCPTools = append(s.res.MCPTools, ref)
	}
}

// SplitMCPName splits an "mcp__<server>__<tool>" tool name (regexp
// ^mcp__(.+?)__(.+)$: the server ends at the FIRST "__" after the prefix, so
// "mcp__gw__langfuse__get_trace" is server "gw", tool "langfuse__get_trace").
// The server is lowercased. ok is false when the name does not fit or a part
// is over its length bound.
func SplitMCPName(name string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(name, mcpPrefix)
	if !found {
		return "", "", false
	}
	i := strings.Index(rest, mcpSeparator)
	if i < 1 || i+len(mcpSeparator) >= len(rest) {
		return "", "", false
	}
	server, tool = strings.ToLower(strings.TrimSpace(rest[:i])), strings.TrimSpace(rest[i+len(mcpSeparator):])
	if server == "" || tool == "" || len(server) > MaxNameLen || len(tool) > MaxToolLen {
		return "", "", false
	}
	return server, tool, true
}

// SplitRef splits a stored "server__tool" value at its first "__".
func SplitRef(ref string) (server, tool string, ok bool) {
	server, tool, ok = strings.Cut(ref, mcpSeparator)
	return server, tool, ok && server != "" && tool != ""
}

// skillName reads the skill a Skill tool call named from its JSON input:
// {"skill": name}, also accepting "command" and "name". Lowercased, trimmed,
// a leading "/" dropped, at most MaxNameLen bytes (longer is ignored).
func skillName(input []byte) string {
	var in map[string]json.RawMessage
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	for _, k := range []string{"skill", "command", "name"} {
		var v string
		if raw, ok := in[k]; ok && json.Unmarshal(raw, &v) == nil {
			v = strings.TrimSpace(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(v), "/")))
			if v != "" && len(v) <= MaxNameLen {
				return v
			}
		}
	}
	return ""
}
