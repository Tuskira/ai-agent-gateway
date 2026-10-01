package llmplane

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
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
	src     io.Reader
	pending []byte // encoded SSE bytes not yet handed to the caller
	err     error
}

func newEventStreamToSSE(src io.Reader) *eventStreamToSSE { return &eventStreamToSSE{src: src} }

// eventStream frame layout (all big-endian):
//
//	total length (4) | headers length (4) | prelude CRC (4) |
//	headers (headers length) | payload | message CRC (4)
const (
	esPreludeLen  = 12
	esTrailerLen  = 4
	esMaxFrameLen = 16 << 20 // Bedrock frames are small; anything bigger is corruption
)

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

// readFrame reads one eventstream message and returns its SSE rendering.
func (e *eventStreamToSSE) readFrame() ([]byte, error) {
	var prelude [esPreludeLen]byte
	if _, err := io.ReadFull(e.src, prelude[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("eventstream: truncated frame prelude")
		}
		return nil, err // io.EOF between frames: clean end of stream
	}
	total := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:12]) {
		return nil, fmt.Errorf("eventstream: prelude CRC mismatch")
	}
	if total < esPreludeLen+esTrailerLen || total > esMaxFrameLen || headersLen > total-esPreludeLen-esTrailerLen {
		return nil, fmt.Errorf("eventstream: invalid frame lengths (total %d, headers %d)", total, headersLen)
	}
	rest := make([]byte, total-esPreludeLen)
	if _, err := io.ReadFull(e.src, rest); err != nil {
		return nil, fmt.Errorf("eventstream: truncated frame body")
	}
	body := rest[:len(rest)-esTrailerLen]
	want := binary.BigEndian.Uint32(rest[len(rest)-esTrailerLen:])
	sum := crc32.ChecksumIEEE(prelude[:])
	sum = crc32.Update(sum, crc32.IEEETable, body)
	if sum != want {
		return nil, fmt.Errorf("eventstream: message CRC mismatch")
	}

	headers, err := parseEventStreamHeaders(body[:headersLen])
	if err != nil {
		return nil, err
	}
	payload := body[headersLen:]
	return renderSSE(headers, payload), nil
}

// parseEventStreamHeaders decodes the header block: repeated
// name length (1) | name | value type (1) | value, where the only value
// type Bedrock uses is 7 (string: length (2) | bytes). Other types are
// skipped by their fixed size so an unexpected header never desyncs.
func parseEventStreamHeaders(b []byte) (map[string]string, error) {
	out := map[string]string{}
	for len(b) > 0 {
		nameLen := int(b[0])
		if len(b) < 1+nameLen+1 {
			return nil, fmt.Errorf("eventstream: truncated header")
		}
		name := string(b[1 : 1+nameLen])
		typ := b[1+nameLen]
		b = b[2+nameLen:]
		switch typ {
		case 0, 1: // bool true/false, no value bytes
		case 2: // byte
			b = b[min(1, len(b)):]
		case 3: // int16
			b = b[min(2, len(b)):]
		case 4: // int32
			b = b[min(4, len(b)):]
		case 5, 8: // int64, timestamp
			b = b[min(8, len(b)):]
		case 6, 7: // byte array, string: length (2) | bytes
			if len(b) < 2 {
				return nil, fmt.Errorf("eventstream: truncated header value")
			}
			n := int(binary.BigEndian.Uint16(b[:2]))
			if len(b) < 2+n {
				return nil, fmt.Errorf("eventstream: truncated header value")
			}
			if typ == 7 {
				out[name] = string(b[2 : 2+n])
			}
			b = b[2+n:]
		case 9: // uuid
			b = b[min(16, len(b)):]
		default:
			return nil, fmt.Errorf("eventstream: unknown header value type %d", typ)
		}
	}
	return out, nil
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
	var hb bytes.Buffer
	for name, value := range headers {
		hb.WriteByte(byte(len(name)))
		hb.WriteString(name)
		hb.WriteByte(7)
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(value)))
		hb.Write(l[:])
		hb.WriteString(value)
	}
	total := esPreludeLen + hb.Len() + len(payload) + esTrailerLen
	frame := make([]byte, 0, total)
	var prelude [esPreludeLen]byte
	binary.BigEndian.PutUint32(prelude[0:4], uint32(total))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(hb.Len()))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[:8]))
	frame = append(frame, prelude[:]...)
	frame = append(frame, hb.Bytes()...)
	frame = append(frame, payload...)
	var trailer [esTrailerLen]byte
	binary.BigEndian.PutUint32(trailer[:], crc32.ChecksumIEEE(frame))
	return append(frame, trailer[:]...)
}
