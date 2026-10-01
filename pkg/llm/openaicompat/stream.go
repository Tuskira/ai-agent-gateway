package openaicompat

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/sse"
)

// NewStreamDecoder turns Chat Completions SSE into neutral events.
//
// Text and thinking stream through as they arrive, one block open at a time.
// Tool calls are buffered per call until the vendor finishes, then emitted
// in first-seen order, each as one tool_use block with its whole input: a
// vendor may interleave the argument fragments of parallel calls (by index),
// and a client cannot reopen a block it has seen stop. Each buffered call is
// capped at llm.MaxFrameBytes and all of them at llm.MaxBufferedBytes.
//
// message_start is emitted once the first chunk has arrived, so a vendor
// that reports prompt usage up front has it in message_start; otherwise the
// input tokens arrive with message_delta.
func (Provider) NewStreamDecoder(body io.Reader) llm.StreamDecoder {
	return &decoder{r: sse.NewReader(body, llm.MaxFrameBytes, llm.ErrFrameTooLarge), byKey: map[int]*pendingTool{}}
}

type decoder struct {
	r       *sse.Reader
	queue   []llm.Event
	err     error // returned once the queue drains (io.EOF when finished)
	started bool

	next  int // next block index
	open  bool
	kind  llm.BlockType // of the open block
	index int           // of the open block

	tools    []*pendingTool // first-seen order
	byKey    map[int]*pendingTool
	buffered int

	finish, stopSeq string
	usage           *chatUsage
}

// pendingTool is one upstream tool call, keyed by its Chat index.
type pendingTool struct {
	id, name string
	args     strings.Builder
}

func (d *decoder) Next() (llm.Event, error) {
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

func (d *decoder) push(ev llm.Event) { d.queue = append(d.queue, ev) }

func (d *decoder) read() {
	_, data, err := d.r.Next()
	switch {
	case errors.Is(err, io.EOF):
		if d.finish == "" {
			// Cut before the model finished: ending it as a complete message
			// would hand the client a truncated answer or tool input as final.
			d.err = io.ErrUnexpectedEOF
			return
		}
		d.end()
	case err != nil:
		d.err = err
	default:
		d.chunk(data)
	}
}

func (d *decoder) start(u *chatUsage) {
	d.started = true
	d.push(llm.Event{Type: llm.EventMessageStart, Message: &llm.Response{
		ID: newID("msg_"), Role: "assistant", Content: []llm.Block{}, Usage: u.neutral(),
	}})
	d.push(llm.Event{Type: llm.EventPing})
}

// chunk handles one SSE data payload.
func (d *decoder) chunk(data string) {
	if data == "[DONE]" {
		if !d.started {
			d.start(nil)
		}
		d.end()
		return
	}
	var c struct {
		Choices []struct {
			Delta        chatDelta       `json:"delta"`
			FinishReason string          `json:"finish_reason"`
			StopReason   json.RawMessage `json:"stop_reason"`
		} `json:"choices"`
		Usage *chatUsage      `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(data), &c) != nil {
		return
	}
	if !d.started {
		d.start(c.Usage)
	}
	if len(c.Error) > 0 && string(c.Error) != "null" {
		d.upstreamError(c.Error)
		return
	}
	if c.Usage != nil {
		d.usage = c.Usage // cumulative: last one wins
	}
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		if ch.FinishReason != "" {
			d.finish = ch.FinishReason
		}
		if s := matchedStop(ch.StopReason); s != "" {
			d.stopSeq = s
		}
		d.delta(llm.BlockThinking, ch.Delta.reasoningText())
		for _, p := range ch.Delta.pieces() {
			d.delta(p.kind, p.text)
		}
		for i, tc := range ch.Delta.ToolCalls {
			if d.toolDelta(i, tc); d.err != nil {
				return
			}
		}
	}
	// include_usage puts usage on a chunk after the finish_reason (OpenAI) or on
	// the same one (DeepSeek, GLM, Kimi); without usage, [DONE] or EOF ends it.
	if d.finish != "" && c.Usage != nil {
		d.end()
	}
}

func (d *decoder) delta(kind llm.BlockType, text string) {
	if text == "" {
		return
	}
	if !d.open || d.kind != kind {
		d.closeBlock()
		blk := llm.Block{Type: kind}
		d.openBlock(kind, &blk)
	}
	delta := &llm.Delta{Type: llm.DeltaText, Text: text}
	if kind == llm.BlockThinking {
		delta = &llm.Delta{Type: llm.DeltaThinking, Thinking: text}
	}
	d.push(llm.Event{Type: llm.EventContentBlockDelta, Index: d.index, Delta: delta})
}

func (d *decoder) openBlock(kind llm.BlockType, blk *llm.Block) {
	d.open, d.kind, d.index = true, kind, d.next
	d.next++
	d.push(llm.Event{Type: llm.EventContentBlockStart, Index: d.index, Block: blk})
}

func (d *decoder) closeBlock() {
	if d.open {
		d.push(llm.Event{Type: llm.EventContentBlockStop, Index: d.index})
		d.open = false
	}
}

// toolDelta buffers one tool-call fragment under its call.
func (d *decoder) toolDelta(pos int, tc chatToolCallIn) {
	key := pos
	if tc.Index != nil {
		key = *tc.Index
	}
	t := d.byKey[key]
	if t != nil && tc.ID != "" && t.id != "" && tc.ID != t.id {
		// A new id under a reused key: vendors that omit index (or restart it at
		// 0 per chunk) send one call per chunk. The key now names the new call.
		t = nil
	}
	if t == nil {
		t = &pendingTool{}
		d.byKey[key] = t
		d.tools = append(d.tools, t)
	}
	t.id = firstNonEmpty(t.id, tc.ID)
	t.name = firstNonEmpty(t.name, tc.Function.Name)
	args := argString(tc.Function.Arguments)
	if t.args.Len()+len(args) > llm.MaxFrameBytes || d.buffered+len(args) > llm.MaxBufferedBytes {
		d.err = llm.ErrFrameTooLarge
		return
	}
	t.args.WriteString(args)
	d.buffered += len(args)
}

// end closes the open block, emits the buffered tool calls and finishes the
// message.
func (d *decoder) end() {
	d.closeBlock()
	sawTool := false
	for _, t := range d.tools {
		if t.name == "" {
			continue // a call without a name cannot be executed
		}
		sawTool = true
		d.openBlock(llm.BlockToolUse, &llm.Block{Type: llm.BlockToolUse, ID: firstNonEmpty(t.id, newID("toolu_")), Name: t.name})
		d.push(llm.Event{Type: llm.EventContentBlockDelta, Index: d.index,
			Delta: &llm.Delta{Type: llm.DeltaInputJSON, PartialJSON: string(toolInput(t.args.String()))}})
		d.closeBlock()
	}
	d.tools, d.byKey, d.buffered = nil, nil, 0
	u := d.usage.neutral()
	ev := llm.Event{Type: llm.EventMessageDelta, StopReason: stopReason(d.finish, sawTool, d.stopSeq), Usage: &u}
	if ev.StopReason == "stop_sequence" {
		ev.StopSequence = d.stopSeq
	}
	d.push(ev)
	d.push(llm.Event{Type: llm.EventMessageStop})
	d.err = io.EOF
}

func (d *decoder) upstreamError(raw json.RawMessage) {
	var e struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	}
	if json.Unmarshal(raw, &e) != nil {
		e.Message = jsonString(raw)
	}
	d.push(llm.Event{Type: llm.EventError, Error: &llm.Error{
		Type:    streamErrorType(firstNonEmpty(jsonString(e.Code), e.Type), e.Code),
		Message: firstNonEmpty(e.Message, "upstream error"),
	}})
	d.err = io.EOF
}
