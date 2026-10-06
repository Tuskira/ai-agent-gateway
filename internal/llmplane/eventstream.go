package llmplane

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/eventstream"
)

// eventStreamToSSE re-frames a Bedrock InvokeModelWithResponseStream body
// (application/vnd.amazon.eventstream: binary frames whose "chunk" payload
// carries a base64 Anthropic stream event) as the text/event-stream an
// Anthropic client expects:
//
//	event: <type>
//	data: <the decoded event JSON>
//
// This is the ONE thing a client speaking the Anthropic dialect cannot
// consume from Bedrock as-is: the events themselves are Anthropic's own
// (message_start, content_block_delta, ...), only the framing differs. No
// field is added, dropped or re-ordered -- the decoded JSON is relayed
// byte-for-byte, so the usage parser and the client both see exactly what
// the model produced. A Bedrock exception frame becomes an Anthropic
// "error" event.
//
// The reader is pull-based: each Read decodes at most one frame, so the
// relay stays incremental (a frame is forwarded as soon as it arrives) and
// memory stays bounded by the largest single frame.
type eventStreamToSSE struct {
	frames  *eventstream.Reader
	pending []byte // encoded SSE bytes not yet handed to the caller
	err     error
}

func newEventStreamToSSE(src io.Reader) *eventStreamToSSE {
	return &eventStreamToSSE{frames: eventstream.NewReader(src)}
}

func (e *eventStreamToSSE) Read(p []byte) (int, error) {
	for len(e.pending) == 0 {
		if e.err != nil {
			return 0, e.err
		}
		frame, err := e.readFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				e.err = io.EOF
				return 0, io.EOF
			}
			e.err = err
			return 0, err
		}
		e.pending = frame
	}
	n := copy(p, e.pending)
	e.pending = e.pending[n:]
	return n, nil
}

// readFrame reads one eventstream message (see internal/eventstream) and
// returns its SSE rendering.
func (e *eventStreamToSSE) readFrame() ([]byte, error) {
	msg, err := e.frames.Next()
	if err != nil {
		return nil, err
	}
	return renderSSE(msg.Headers, msg.Payload), nil
}

// renderSSE turns one decoded frame into an SSE event. A "chunk" event's
// payload is {"bytes": "<base64 of the Anthropic event JSON>", "p": "..."};
// the decoded JSON's own "type" names the SSE event. Exceptions and errors
// (a throttle, a validation failure mid-stream) are rendered as an
// Anthropic "error" event so the client sees a well-formed failure instead
// of a silently ended stream.
func renderSSE(headers map[string]string, payload []byte) []byte {
	switch headers[":message-type"] {
	case "event":
		if headers[":event-type"] == "chunk" {
			var chunk struct {
				Bytes string `json:"bytes"`
			}
			if json.Unmarshal(payload, &chunk) == nil && chunk.Bytes != "" {
				if dec, err := base64.StdEncoding.DecodeString(chunk.Bytes); err == nil {
					return sseEvent(eventTypeOf(dec), dec)
				}
			}
		}
		// An event without a decodable chunk: relay its payload under its
		// event type so nothing is silently dropped.
		return sseEvent(headers[":event-type"], payload)
	default: // "exception", "error"
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &msg)
		if msg.Message == "" {
			msg.Message = string(bytes.TrimSpace(payload))
		}
		errType := headers[":exception-type"]
		if errType == "" {
			errType = headers[":error-code"]
		}
		if errType == "" {
			errType = "api_error"
		}
		body, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]string{"type": errType, "message": msg.Message},
		})
		return sseEvent("error", body)
	}
}

func eventTypeOf(event []byte) string {
	var t struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(event, &t)
	return t.Type
}

func sseEvent(name string, data []byte) []byte {
	var b bytes.Buffer
	if name != "" {
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.Write(bytes.TrimRight(data, "\r\n"))
	b.WriteString("\n\n")
	return b.Bytes()
}

// encodeEventStreamFrame builds one eventstream message (the inverse of
// readFrame) with string headers only. It exists for tests that need a
// real Bedrock-shaped stream from an httptest upstream; production never
// encodes.
func encodeEventStreamFrame(headers map[string]string, payload []byte) []byte {
	return eventstream.Encode(headers, payload)
}
