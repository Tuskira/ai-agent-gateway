package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/sse"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/strictjson"
)

// CompleteReaderName is the registry name of CompleteReader.
const CompleteReaderName = "anthropic_complete"

func init() { llm.RegisterReader(CompleteReader{}) }

// The legacy prompt's turn markers.
const (
	humanMarker     = "\n\nHuman:"
	assistantMarker = "\n\nAssistant:"
)

// CompleteReader is the llm.Reader for the legacy Anthropic Text
// Completions wire (/v1/complete): one prompt text in, one completion text
// out.
//
// The prompt is split into turns best effort, by its markers: every
// "\n\nHuman:" starts a user turn and every "\n\nAssistant:" an assistant
// turn, running to the next marker (a prompt may also begin with "Human:"
// or "Assistant:" without the newlines). Each turn's text is trimmed of
// surrounding whitespace and an empty turn (the final "\n\nAssistant:" cue,
// say) is dropped. Text before the first marker is the system prompt. A
// prompt with no marker is one user text block, verbatim; a body with no
// prompt (absent or null) is refused. Markers inside a
// turn's own text cannot be told from real ones, so this is a reading, not
// the model's view: the request is one flat prompt and
// Extra[llm.HistoryKey] is llm.HistoryPrompt.
//
// max_tokens_to_sample is MaxTokens; stop_sequences, temperature, top_p,
// top_k, stream and metadata map to their neutral slots; any other field
// goes to Request.Extra. A response's completion is one text block;
// stop_reason "max_tokens" is max_tokens, and "stop_sequence" is
// stop_sequence when the matched stop is a client sequence, else end_turn
// (the model ended its turn with "\n\nHuman:"). The wire has no usage.
type CompleteReader struct{}

func (CompleteReader) Name() string { return CompleteReaderName }

// DecodeRequest reads a Text Completions request body strictly.
func (CompleteReader) DecodeRequest(body []byte) (*llm.Request, error) {
	if err := strictjson.Check(body); err != nil {
		return nil, &llm.RequestError{Err: fmt.Errorf("invalid request body: %v", err)}
	}
	req, err := readCompleteRequest(body)
	if err != nil {
		return nil, &llm.RequestError{Err: err}
	}
	return req, nil
}

func readCompleteRequest(body []byte) (*llm.Request, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		if err == nil {
			err = errors.New("not a JSON object")
		}
		return nil, fmt.Errorf("invalid request body: %v", err)
	}
	if _, ok := top[llm.HistoryKey]; ok {
		return nil, fmt.Errorf("%s: reserved", llm.HistoryKey)
	}
	req := &llm.Request{}
	var prompt *string
	for _, f := range []struct {
		key string
		dst any
	}{
		{"model", &req.Model}, {"prompt", &prompt}, {"max_tokens_to_sample", &req.MaxTokens},
		{"stop_sequences", &req.StopSequences}, {"temperature", &req.Temperature}, {"top_p", &req.TopP},
		{"top_k", &req.TopK}, {"stream", &req.Stream}, {"metadata", &req.Metadata},
	} {
		raw, ok := top[f.key]
		if !ok {
			continue
		}
		delete(top, f.key)
		if isNull(raw) {
			continue
		}
		if err := json.Unmarshal(raw, f.dst); err != nil {
			return nil, fmt.Errorf("%s: %v", f.key, err)
		}
	}
	if prompt == nil {
		return nil, errors.New("prompt: required")
	}
	req.System, req.Messages = splitPrompt(*prompt)
	req.Extra = nonEmpty(top)
	if req.Extra == nil {
		req.Extra = map[string]json.RawMessage{}
	}
	req.Extra[llm.HistoryKey], _ = json.Marshal(llm.HistoryPrompt)
	return req, nil
}

// splitPrompt splits a legacy prompt into a system prompt and turns (see
// CompleteReader).
func splitPrompt(prompt string) ([]llm.Block, []llm.Message) {
	p := prompt
	if strings.HasPrefix(p, "Human:") || strings.HasPrefix(p, "Assistant:") {
		p = "\n\n" + p
	}
	type mark struct {
		at, end int
		role    string
	}
	var marks []mark
	for i := 0; i < len(p); {
		h, a := strings.Index(p[i:], humanMarker), strings.Index(p[i:], assistantMarker)
		var m mark
		switch {
		case h < 0 && a < 0:
			i = len(p)
			continue
		case a < 0 || (h >= 0 && h < a):
			m = mark{i + h, i + h + len(humanMarker), "user"}
		default:
			m = mark{i + a, i + a + len(assistantMarker), "assistant"}
		}
		marks = append(marks, m)
		i = m.end
	}
	if len(marks) == 0 {
		return nil, []llm.Message{{Role: "user", Content: []llm.Block{{Type: llm.BlockText, Text: prompt}}}}
	}
	var system []llm.Block
	if head := strings.TrimSpace(p[:marks[0].at]); head != "" {
		system = []llm.Block{{Type: llm.BlockText, Text: head}}
	}
	var msgs []llm.Message
	for i, m := range marks {
		stop := len(p)
		if i+1 < len(marks) {
			stop = marks[i+1].at
		}
		if text := strings.TrimSpace(p[m.end:stop]); text != "" {
			msgs = append(msgs, llm.Message{Role: m.role, Content: []llm.Block{{Type: llm.BlockText, Text: text}}})
		}
	}
	return system, msgs
}

// completion is a non-stream body and a stream completion event.
type completion struct {
	Type       string     `json:"type"`
	ID         string     `json:"id"`
	Model      string     `json:"model"`
	Completion string     `json:"completion"`
	StopReason string     `json:"stop_reason"`
	Stop       string     `json:"stop"`
	Error      *llm.Error `json:"error"`
}

// stop maps the completion's stop_reason and stop onto a neutral stop
// reason and stop sequence ("" while not finished).
func (c *completion) stop() (string, string) {
	switch c.StopReason {
	case "":
		return "", ""
	case "max_tokens":
		return "max_tokens", ""
	case "stop_sequence":
		if c.Stop != "" && c.Stop != humanMarker {
			return "stop_sequence", c.Stop
		}
	}
	return "end_turn", ""
}

// DecodeResponse reads a non-stream Text Completions response body.
func (CompleteReader) DecodeResponse(body []byte) (*llm.Response, error) {
	if len(body) > llm.MaxBodyBytes {
		return nil, llm.ErrFrameTooLarge
	}
	if err := strictjson.Check(body); err != nil {
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	var c completion
	if err := json.Unmarshal(body, &c); err != nil || !strings.HasPrefix(strings.TrimSpace(string(body)), "{") {
		if err == nil {
			err = errors.New("not a JSON object")
		}
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	out := &llm.Response{ID: c.ID, Model: c.Model, Role: "assistant", Content: []llm.Block{{Type: llm.BlockText, Text: c.Completion}}}
	out.StopReason, out.StopSequence = c.stop()
	return out, nil
}

// NewResponseDecoder reads Text Completions SSE (completion events, each
// carrying the next piece of text, the last one a stop_reason; ping and
// error events) over frames that passed the strict check. The text is one
// text block; a stream that ends before a stop_reason is cut.
func (CompleteReader) NewResponseDecoder(r io.Reader) llm.StreamDecoder {
	return &completeDecoder{r: sse.NewReader(sse.Checked(r, llm.MaxFrameBytes, llm.ErrFrameTooLarge, strictjson.Check), llm.MaxFrameBytes, llm.ErrFrameTooLarge)}
}

type completeDecoder struct {
	r       *sse.Reader
	queue   []llm.Event
	err     error
	started bool
	open    bool
}

func (d *completeDecoder) Next() (llm.Event, error) {
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

func (d *completeDecoder) push(ev llm.Event) { d.queue = append(d.queue, ev) }

func (d *completeDecoder) read() {
	_, data, err := d.r.Next()
	switch {
	case errors.Is(err, io.EOF):
		d.err = io.ErrUnexpectedEOF // no stop_reason
		return
	case err != nil:
		d.err = err
		return
	}
	var c completion
	if json.Unmarshal([]byte(data), &c) != nil {
		return // not an event this decoder knows
	}
	switch c.Type {
	case "completion":
		if !d.started {
			d.started = true
			d.push(llm.Event{Type: llm.EventMessageStart, Message: &llm.Response{ID: c.ID, Model: c.Model, Role: "assistant", Content: []llm.Block{}}})
		}
		if c.Completion != "" {
			if !d.open {
				d.open = true
				d.push(llm.Event{Type: llm.EventContentBlockStart, Index: 0, Block: &llm.Block{Type: llm.BlockText}})
			}
			d.push(llm.Event{Type: llm.EventContentBlockDelta, Index: 0, Delta: &llm.Delta{Type: llm.DeltaText, Text: c.Completion}})
		}
		reason, seq := c.stop()
		if reason == "" {
			return
		}
		if d.open {
			d.push(llm.Event{Type: llm.EventContentBlockStop, Index: 0})
			d.open = false
		}
		d.push(llm.Event{Type: llm.EventMessageDelta, StopReason: reason, StopSequence: seq, Usage: &llm.Usage{}})
		d.push(llm.Event{Type: llm.EventMessageStop})
		d.err = io.EOF
	case "ping":
		if d.started {
			d.push(llm.Event{Type: llm.EventPing})
		}
	case "error":
		e := c.Error
		if e == nil {
			e = &llm.Error{Type: llm.ErrorTypeAPI, Message: "upstream error"}
		}
		d.push(llm.Event{Type: llm.EventError, Error: e})
		d.err = io.EOF
	}
}
