package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/sse"
)

// Name is the registry name of both the Dialect and the Provider.
const Name = "anthropic"

func init() {
	llm.RegisterDialect(Dialect{})
	llm.RegisterProvider(Provider{})
}

// Dialect is a client speaking the Anthropic Messages API (/v1/messages).
type Dialect struct{}

func (Dialect) Name() string { return Name }

// ParseRequest reads an Anthropic Messages request body.
func (Dialect) ParseRequest(r *http.Request) (*llm.Request, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, &llm.RequestError{Err: fmt.Errorf("cannot read request body: %v", err)}
	}
	req, err := parseRequest(body)
	if err != nil {
		return nil, &llm.RequestError{Err: err}
	}
	return req, nil
}

// RenderResponse renders an Anthropic Messages response.
func (Dialect) RenderResponse(resp *llm.Response) ([]byte, error) {
	return json.Marshal(renderMessage(resp))
}

// RenderError renders the Anthropic error envelope.
func (Dialect) RenderError(e *llm.Error) []byte { return renderError(e) }

// NewStreamEncoder writes Anthropic Messages SSE, one Write per event.
func (Dialect) NewStreamEncoder(w io.Writer) llm.StreamEncoder { return &encoder{w: w} }

type encoder struct {
	w   io.Writer
	err error
}

func (e *encoder) Write(ev llm.Event) error {
	if e.err != nil {
		return e.err
	}
	var data map[string]any
	switch ev.Type {
	case llm.EventMessageStart:
		msg := llm.Response{Role: "assistant"}
		if ev.Message != nil {
			msg = *ev.Message
		}
		msg.Content, msg.StopReason, msg.StopSequence = nil, "", ""
		data = map[string]any{"message": renderMessage(&msg)}
	case llm.EventContentBlockStart:
		blk := llm.Block{}
		if ev.Block != nil {
			blk = *ev.Block
		}
		data = map[string]any{"index": ev.Index, "content_block": renderBlock(blk)}
	case llm.EventContentBlockDelta:
		d := map[string]string{}
		if ev.Delta != nil {
			d["type"] = string(ev.Delta.Type)
			switch ev.Delta.Type {
			case llm.DeltaText:
				d["text"] = ev.Delta.Text
			case llm.DeltaThinking:
				d["thinking"] = ev.Delta.Thinking
			case llm.DeltaSignature:
				d["signature"] = ev.Delta.Signature
			case llm.DeltaInputJSON:
				d["partial_json"] = ev.Delta.PartialJSON
			}
		}
		data = map[string]any{"index": ev.Index, "delta": d}
	case llm.EventContentBlockStop:
		data = map[string]any{"index": ev.Index}
	case llm.EventMessageDelta:
		var stopSeq any
		if ev.StopSequence != "" {
			stopSeq = ev.StopSequence
		}
		u := llm.Usage{}
		if ev.Usage != nil {
			u = *ev.Usage
		}
		data = map[string]any{"delta": map[string]any{"stop_reason": ev.StopReason, "stop_sequence": stopSeq}, "usage": renderUsage(u)}
	case llm.EventError:
		e := llm.Error{Type: llm.ErrorTypeAPI}
		if ev.Error != nil {
			e = *ev.Error
		}
		data = map[string]any{"error": map[string]string{"type": firstNonEmpty(e.Type, llm.ErrorTypeAPI), "message": e.Message}}
	case llm.EventMessageStop, llm.EventPing:
		data = map[string]any{}
	default:
		return fmt.Errorf("anthropic: unknown stream event %q", ev.Type)
	}
	data["type"] = ev.Type
	b, err := json.Marshal(data)
	if err != nil {
		e.err = err
		return err
	}
	frame := make([]byte, 0, len(ev.Type)+len(b)+16)
	frame = append(append(append(append(frame, "event: "...), ev.Type...), "\ndata: "...), b...)
	_, e.err = e.w.Write(append(frame, "\n\n"...))
	return e.err
}

func (e *encoder) Close() error { return e.err }

// Provider calls a vendor that speaks the Anthropic Messages API: it renders
// the neutral request back to Anthropic's wire (Extra and Raw included, so it
// carries everything) and parses Anthropic's answers.
type Provider struct{}

func (Provider) Name() string { return Name }

func (Provider) Capabilities() llm.Capabilities {
	return llm.Capabilities{
		Passthrough: true, ImagesBase64: true, ImagesURL: true, TextDocuments: true, Documents: true,
		Thinking: true, ToolChoiceNone: true, ParallelToolControl: true, ToolReferences: true,
		StopSequences: true, ServerTools: true,
	}
}

// BuildRequest POSTs {BaseURL}/v1/messages with x-api-key and
// anthropic-version, and nothing else from the client.
func (Provider) BuildRequest(ctx context.Context, req *llm.Request, t llm.Target) (*http.Request, error) {
	body, err := renderRequest(req, t.Model)
	if err != nil {
		return nil, err
	}
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(t.BaseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Accept", "application/json")
	if req.Stream {
		up.Header.Set("Accept", "text/event-stream")
	}
	up.Header.Set("Anthropic-Version", Version)
	if t.Auth.APIKey != "" {
		up.Header.Set("X-Api-Key", t.Auth.APIKey)
	}
	return up, nil
}

// ParseResponse reads an Anthropic Messages response.
func (Provider) ParseResponse(resp *http.Response) (*llm.Response, error) {
	var w wireMessage
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
		return nil, fmt.Errorf("upstream response: %v", err)
	}
	return w.neutral()
}

// ParseError reads the Anthropic error envelope.
func (Provider) ParseError(status int, body []byte) *llm.Error { return parseError(status, body) }

// NewStreamDecoder reads Anthropic Messages SSE.
func (Provider) NewStreamDecoder(body io.Reader) llm.StreamDecoder {
	return &decoder{r: sse.NewReader(body, llm.MaxFrameBytes, llm.ErrFrameTooLarge)}
}

type decoder struct {
	r    *sse.Reader
	done bool
}

func (d *decoder) Next() (llm.Event, error) {
	for {
		if d.done {
			return llm.Event{}, io.EOF
		}
		_, data, err := d.r.Next()
		if errors.Is(err, io.EOF) {
			return llm.Event{}, io.ErrUnexpectedEOF // no message_stop
		}
		if err != nil {
			return llm.Event{}, err
		}
		var ev struct {
			Type         llm.EventType   `json:"type"`
			Message      *wireMessage    `json:"message"`
			Index        int             `json:"index"`
			ContentBlock json.RawMessage `json:"content_block"`
			Delta        struct {
				llm.Delta
				StopReason   string `json:"stop_reason"`
				StopSequence string `json:"stop_sequence"`
			} `json:"delta"`
			Usage *wireUsage `json:"usage"`
			Error *llm.Error `json:"error"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue // not an event this decoder knows
		}
		out := llm.Event{Type: ev.Type, Index: ev.Index}
		switch ev.Type {
		case llm.EventMessageStart:
			if ev.Message == nil {
				continue
			}
			msg, err := ev.Message.neutral()
			if err != nil {
				return llm.Event{}, err
			}
			out.Message = msg
		case llm.EventContentBlockStart:
			b, err := parseBlock(ev.ContentBlock)
			if err != nil {
				return llm.Event{}, err
			}
			out.Block = &b
		case llm.EventContentBlockDelta:
			delta := ev.Delta.Delta
			out.Delta = &delta
		case llm.EventContentBlockStop, llm.EventPing:
		case llm.EventMessageDelta:
			out.StopReason, out.StopSequence = ev.Delta.StopReason, ev.Delta.StopSequence
			if ev.Usage != nil {
				u := ev.Usage.neutral()
				out.Usage = &u
			}
		case llm.EventMessageStop:
			d.done = true
		case llm.EventError:
			d.done = true
			out.Error = ev.Error
			if out.Error == nil {
				out.Error = &llm.Error{Type: llm.ErrorTypeAPI, Message: "upstream error"}
			}
		default:
			continue
		}
		return out, nil
	}
}
