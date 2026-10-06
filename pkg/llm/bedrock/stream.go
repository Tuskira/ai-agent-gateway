package bedrock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// emitter queues neutral stream events in the documented order: one
// message_start, blocks opened one at a time at indices 0, 1, 2, ..., and
// at the end one message_delta and message_stop. Bedrock reports the stop
// reason and the usage in separate events (and some families only when the
// stream ends), so the end is emitted when the stream ends, from the last
// stop reason and usage seen.
type emitter struct {
	queue    []llm.Event
	started  bool
	open     int // neutral index of the open block, -1 when none
	openKind llm.BlockType
	next     int
	stop     string // "" until the model said why it stopped
	usage    *llm.Usage
	done     bool
}

func (e *emitter) pop() (llm.Event, bool) {
	if len(e.queue) == 0 {
		return llm.Event{}, false
	}
	ev := e.queue[0]
	e.queue = e.queue[1:]
	return ev, true
}

func (e *emitter) push(ev llm.Event) { e.queue = append(e.queue, ev) }

func (e *emitter) start() {
	if !e.started {
		e.started = true
		e.push(llm.Event{Type: llm.EventMessageStart, Message: &llm.Response{Role: "assistant", Content: []llm.Block{}}})
	}
}

// openBlock closes the open block, if any, and opens b.
func (e *emitter) openBlock(b llm.Block) {
	e.start()
	e.closeBlock()
	e.open, e.openKind = e.next, b.Type
	e.next++
	e.push(llm.Event{Type: llm.EventContentBlockStart, Index: e.open, Block: &b})
}

func (e *emitter) closeBlock() {
	if e.open >= 0 {
		e.push(llm.Event{Type: llm.EventContentBlockStop, Index: e.open})
		e.open = -1
	}
}

// text appends text to the open text block, opening one if needed.
func (e *emitter) text(s string) {
	if s == "" {
		return
	}
	if e.open < 0 || e.openKind != llm.BlockText {
		e.openBlock(llm.Block{Type: llm.BlockText})
	}
	e.push(llm.Event{Type: llm.EventContentBlockDelta, Index: e.open, Delta: &llm.Delta{Type: llm.DeltaText, Text: s}})
}

// fail ends the stream with a vendor error event.
func (e *emitter) fail(err *llm.Error) {
	e.push(llm.Event{Type: llm.EventError, Error: err})
	e.done = true
}

// end reads the end of the underlying stream: on a clean end after the
// model stopped it queues message_delta and message_stop and returns
// io.EOF; on a clean end before, io.ErrUnexpectedEOF; any other error as is.
func (e *emitter) end(err error) error {
	if e.done || !errors.Is(err, io.EOF) {
		return err
	}
	if e.stop == "" {
		return io.ErrUnexpectedEOF
	}
	e.start()
	e.closeBlock()
	e.push(llm.Event{Type: llm.EventMessageDelta, StopReason: e.stop, Usage: e.usage})
	e.push(llm.Event{Type: llm.EventMessageStop})
	e.done = true
	return io.EOF
}

// converseState folds ConverseStream events (and Nova's invoke stream,
// which carries the same events) into neutral ones.
type converseState struct {
	emitter
	wire int // Bedrock's contentBlockIndex of the open block
}

func newConverseState() *converseState { return &converseState{emitter: emitter{open: -1}} }

// event reads one event payload of type typ.
func (s *converseState) event(typ string, payload []byte) error {
	if s.done {
		return fmt.Errorf("%s event after the end of the stream", typ)
	}
	m, err := object(payload)
	if err != nil {
		return fmt.Errorf("invalid %s event: %v", typ, err)
	}
	var idx int
	if err := take(m, "contentBlockIndex", &idx); err != nil {
		return fmt.Errorf("invalid %s event: %v", typ, err)
	}
	switch typ {
	case "messageStart":
		var role string
		if err := take(m, "role", &role); err != nil || role != "assistant" {
			return fmt.Errorf("invalid messageStart event: role %q, want assistant", role)
		}
		s.start()
	case "contentBlockStart":
		var start json.RawMessage
		if err := take(m, "start", &start); err != nil {
			return fmt.Errorf("invalid contentBlockStart event: %v", err)
		}
		kind, v, err := member(start)
		if err != nil {
			return fmt.Errorf("invalid contentBlockStart event: start: %v", err)
		}
		b := llm.Block{Type: llm.BlockType(kind), Raw: start}
		if kind == "toolUse" {
			o, err := object(v)
			if err != nil {
				return errors.New("invalid contentBlockStart event: start.toolUse: must be an object")
			}
			b = llm.Block{Type: llm.BlockToolUse}
			_ = take(o, "toolUseId", &b.ID)
			_ = take(o, "name", &b.Name)
			if b.ID == "" || b.Name == "" {
				return errors.New("invalid contentBlockStart event: start.toolUse: toolUseId and name required")
			}
		}
		s.openBlock(b)
		s.wire = idx
	case "contentBlockDelta":
		var delta json.RawMessage
		if err := take(m, "delta", &delta); err != nil {
			return fmt.Errorf("invalid contentBlockDelta event: %v", err)
		}
		kind, v, err := member(delta)
		if err != nil {
			return fmt.Errorf("invalid contentBlockDelta event: delta: %v", err)
		}
		if err := s.delta(idx, kind, v); err != nil {
			return fmt.Errorf("invalid contentBlockDelta event: delta.%v", err)
		}
	case "contentBlockStop":
		if s.open >= 0 && s.wire != idx {
			return fmt.Errorf("invalid contentBlockStop event: block %d, open block %d", idx, s.wire)
		}
		s.closeBlock()
	case "messageStop":
		var stop string
		if err := take(m, "stopReason", &stop); err != nil || stop == "" {
			return errors.New("invalid messageStop event: stopReason required")
		}
		s.start()
		s.closeBlock()
		s.stop = converseStop(stop)
	case "metadata":
		usage, err := takeObject(m, "usage")
		if err != nil {
			return fmt.Errorf("invalid metadata event: %v", err)
		}
		if usage != nil {
			u, err := converseUsage(usage)
			if err != nil {
				return fmt.Errorf("invalid metadata event: usage.%v", err)
			}
			s.usage = &u
		}
	default:
		return fmt.Errorf("unknown stream event %q", typ)
	}
	return nil
}

// delta reads one contentBlockDelta member. A text or reasoning delta for a
// block with no contentBlockStart opens it (Bedrock starts only tool_use
// blocks explicitly).
func (s *converseState) delta(idx int, kind string, v json.RawMessage) error {
	want := map[string]llm.BlockType{"text": llm.BlockText, "toolUse": llm.BlockToolUse, "reasoningContent": llm.BlockThinking}[kind]
	if want == "" {
		return fmt.Errorf("%s: unsupported delta", kind)
	}
	if kind == "reasoningContent" {
		if rk, rv, err := member(v); err == nil && rk == "redactedContent" {
			// Opaque: a redacted_thinking block of its own, no delta.
			b := llm.Block{Type: llm.BlockRedactedThinking}
			if err := json.Unmarshal(rv, &b.Data); err != nil {
				return errors.New("reasoningContent.redactedContent: must be a string")
			}
			s.openBlock(b)
			s.wire = idx
			return nil
		}
	}
	if s.open < 0 || s.wire != idx {
		if kind == "toolUse" {
			return errors.New("toolUse: before its contentBlockStart")
		}
		s.openBlock(llm.Block{Type: want})
		s.wire = idx
	}
	if s.openKind != want {
		return fmt.Errorf("%s: delta for a %s block", kind, s.openKind)
	}
	d := &llm.Delta{}
	switch kind {
	case "text":
		d.Type = llm.DeltaText
		if err := json.Unmarshal(v, &d.Text); err != nil {
			return errors.New("text: must be a string")
		}
	case "toolUse":
		o, err := object(v)
		if err != nil {
			return errors.New("toolUse: must be an object")
		}
		d.Type = llm.DeltaInputJSON
		if err := take(o, "input", &d.PartialJSON); err != nil {
			return fmt.Errorf("toolUse.%v", err)
		}
	case "reasoningContent":
		rk, rv, err := member(v)
		if err != nil {
			return fmt.Errorf("reasoningContent: %v", err)
		}
		var str string
		if err := json.Unmarshal(rv, &str); err != nil {
			return fmt.Errorf("reasoningContent.%s: must be a string", rk)
		}
		switch rk {
		case "text":
			d.Type, d.Thinking = llm.DeltaThinking, str
		case "signature":
			d.Type, d.Signature = llm.DeltaSignature, str
		default:
			return fmt.Errorf("reasoningContent: unsupported member %q", rk)
		}
	}
	s.push(llm.Event{Type: llm.EventContentBlockDelta, Index: s.open, Delta: d})
	return nil
}
