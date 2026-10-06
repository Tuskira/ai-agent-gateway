package gemini

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/sse"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/strictjson"
)

// frames yields the JSON chunks of a stream: io.EOF when it ended between
// chunks, io.ErrUnexpectedEOF when it ended inside one it can tell is cut.
type frames interface {
	next() ([]byte, error)
}

// sseFrames reads alt=sse: one chunk per "data:" event.
type sseFrames struct{ r *sse.Reader }

func (f sseFrames) next() ([]byte, error) {
	for {
		_, data, err := f.r.Next()
		if err != nil {
			return nil, err
		}
		if data != "" {
			return []byte(data), nil
		}
	}
}

// arrayFrames reads the JSON array streamGenerateContent sends without
// alt=sse, one element per chunk, each capped at llm.MaxFrameBytes.
type arrayFrames struct {
	dec    *json.Decoder
	cap    *capped
	opened bool
}

func (f *arrayFrames) next() ([]byte, error) {
	if !f.opened {
		if _, err := f.dec.Token(); err != nil { // the '[' newDecoder saw
			return nil, err
		}
		f.opened = true
	}
	if !f.dec.More() {
		if tok, err := f.dec.Token(); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		} else if err == nil && tok != json.Delim(']') {
			return nil, fmt.Errorf("invalid stream: unexpected %v", tok)
		}
		return nil, io.EOF
	}
	var raw json.RawMessage
	if err := f.dec.Decode(&raw); err != nil {
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			return nil, io.ErrUnexpectedEOF
		case errors.Is(err, llm.ErrFrameTooLarge):
			return nil, err
		}
		return nil, fmt.Errorf("invalid stream frame: %w", err)
	}
	f.cap.n, _ = io.Copy(io.Discard, f.dec.Buffered()) // read ahead: the next chunk's
	return raw, nil
}

// capped fails a read once more than max bytes were read since n was reset.
type capped struct {
	r   io.Reader
	n   int64
	max int64
}

func (c *capped) Read(p []byte) (int, error) {
	if c.n > c.max {
		return 0, llm.ErrFrameTooLarge
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// decoder turns Gemini stream chunks into neutral events. Text and thinking
// stream through as they arrive, one block open at a time; Gemini sends
// each functionCall whole, so it is emitted at once as one tool_use block.
// Any other part (inline media, executable code) is one block emitted
// whole. message_start carries the first chunk's id, model and usage; the
// last usageMetadata seen is message_delta's usage.
type decoder struct {
	src     io.Reader // until the format is known
	frames  frames
	queue   []llm.Event
	err     error // returned once the queue drains (io.EOF when finished)
	started bool

	next  int // next block index
	open  bool
	kind  llm.BlockType // of the open block
	index int           // of the open block

	ids                 calls
	sawTool             bool
	finish, blockReason string
	usage               llm.Usage
}

func newDecoder(r io.Reader) *decoder { return &decoder{src: r} }

// detect picks SSE or the JSON array from the first non-space byte.
func (d *decoder) detect() {
	br := bufio.NewReader(d.src)
	d.src = nil
	for {
		b, err := br.Peek(1)
		if err != nil || (b[0] != ' ' && b[0] != '\t' && b[0] != '\r' && b[0] != '\n') {
			break
		}
		_, _ = br.ReadByte()
	}
	if b, err := br.Peek(1); err == nil && b[0] == '[' {
		c := &capped{r: br, max: llm.MaxFrameBytes}
		d.frames = &arrayFrames{dec: json.NewDecoder(c), cap: c}
		return
	}
	d.frames = sseFrames{sse.NewReader(br, llm.MaxFrameBytes, llm.ErrFrameTooLarge)}
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
	if d.frames == nil {
		d.detect()
	}
	data, err := d.frames.next()
	switch {
	case errors.Is(err, io.EOF):
		if d.finish == "" && d.blockReason == "" {
			// Cut before the model finished: ending it as a complete message
			// would present a truncated answer as final.
			d.err = io.ErrUnexpectedEOF
			return
		}
		d.end()
	case err != nil:
		d.err = err
	case !json.Valid(data):
		// A frame cut by the end of the stream is a cut stream; anything else
		// that is not JSON is a broken one.
		if _, err := d.frames.next(); errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			d.err = io.ErrUnexpectedEOF
			return
		}
		d.err = errors.New("invalid stream frame: not JSON")
	default:
		d.chunk(data)
	}
}

// chunk handles one stream chunk.
func (d *decoder) chunk(data []byte) {
	if err := strictjson.Check(data); err != nil {
		d.err = fmt.Errorf("invalid stream frame: %v", err)
		return
	}
	c, err := readChunk(data, &d.ids)
	if err != nil {
		d.err = fmt.Errorf("invalid stream frame: %v", err)
		return
	}
	if !d.started {
		d.started = true
		msg := &llm.Response{ID: c.id, Model: c.model, Role: "assistant", Content: []llm.Block{}}
		if c.usage != nil {
			msg.Usage = *c.usage
		}
		d.push(llm.Event{Type: llm.EventMessageStart, Message: msg})
	}
	if c.err != nil {
		d.push(llm.Event{Type: llm.EventError, Error: c.err})
		d.err = io.EOF
		return
	}
	if c.usage != nil {
		d.usage = *c.usage // cumulative: last one wins
	}
	for _, b := range c.parts {
		d.part(b)
	}
	if c.finish != "" {
		d.finish = c.finish
	}
	if c.blockReason != "" {
		d.blockReason = c.blockReason
	}
}

func (d *decoder) part(b llm.Block) {
	switch b.Type {
	case llm.BlockText:
		if b.Extra != nil { // a thoughtSignature, ...: a block of its own
			d.closeBlock()
			d.openBlock(llm.BlockText, &llm.Block{Type: llm.BlockText, Extra: b.Extra})
		}
		d.delta(llm.BlockText, b.Text)
	case llm.BlockThinking:
		d.delta(llm.BlockThinking, b.Thinking)
		if b.Signature != "" {
			if !d.open || d.kind != llm.BlockThinking {
				d.closeBlock()
				d.openBlock(llm.BlockThinking, &llm.Block{Type: llm.BlockThinking})
			}
			d.push(llm.Event{Type: llm.EventContentBlockDelta, Index: d.index,
				Delta: &llm.Delta{Type: llm.DeltaSignature, Signature: b.Signature}})
		}
	case llm.BlockToolUse:
		d.sawTool = true
		d.closeBlock()
		d.openBlock(llm.BlockToolUse, &llm.Block{Type: llm.BlockToolUse, ID: b.ID, Name: b.Name, Extra: b.Extra})
		d.push(llm.Event{Type: llm.EventContentBlockDelta, Index: d.index,
			Delta: &llm.Delta{Type: llm.DeltaInputJSON, PartialJSON: string(b.Input)}})
		d.closeBlock()
	default:
		d.closeBlock()
		d.openBlock(b.Type, &b)
		d.closeBlock()
	}
}

func (d *decoder) delta(kind llm.BlockType, text string) {
	if text == "" {
		return
	}
	if !d.open || d.kind != kind {
		d.closeBlock()
		d.openBlock(kind, &llm.Block{Type: kind})
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

// end closes the open block and finishes the message.
func (d *decoder) end() {
	d.closeBlock()
	c := chunk{finish: d.finish, blockReason: d.blockReason}
	u := d.usage
	d.push(llm.Event{Type: llm.EventMessageDelta, StopReason: c.stopReason(d.sawTool), Usage: &u})
	d.push(llm.Event{Type: llm.EventMessageStop})
	d.err = io.EOF
}
