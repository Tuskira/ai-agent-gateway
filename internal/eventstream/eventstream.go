// Package eventstream reads and writes AWS event-stream messages
// (application/vnd.amazon.eventstream), the binary framing of Bedrock's
// streaming APIs (InvokeModelWithResponseStream and ConverseStream). The LLM
// plane re-frames these streams for clients, and the pkg/llm/bedrock Readers
// decode them; both read frames here.
package eventstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
)

// Frame layout (all big-endian):
//
//	total length (4) | headers length (4) | prelude CRC (4) |
//	headers (headers length) | payload | message CRC (4)
const (
	PreludeLen = 12
	TrailerLen = 4
	// MaxFrameLen bounds one frame. Bedrock frames are small; anything
	// bigger is corruption.
	MaxFrameLen = 16 << 20
)

// Message is one decoded event-stream message.
type Message struct {
	// Headers holds the string-valued headers (":message-type",
	// ":event-type", ":exception-type", ...). Headers of other value types
	// are skipped.
	Headers map[string]string
	// Payload is the message body (JSON for every Bedrock event).
	Payload []byte
}

// truncated is a frame cut short. Its text is the one the plane has always
// reported; it unwraps to io.ErrUnexpectedEOF when the input simply ended,
// so a reader can tell a cut stream from a corrupt one.
type truncated struct {
	msg   string
	cause error
}

func (e *truncated) Error() string { return e.msg }
func (e *truncated) Unwrap() error { return e.cause }

func cut(msg string, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.ErrUnexpectedEOF
	}
	return &truncated{msg: msg, cause: err}
}

// Reader reads messages one at a time from an event stream. Memory stays
// bounded by the largest single frame.
type Reader struct {
	src io.Reader
}

// NewReader reads messages from r.
func NewReader(r io.Reader) *Reader { return &Reader{src: r} }

// Next reads one message. It returns io.EOF when the stream ends cleanly
// between frames; a frame cut by the end of the stream is an error that
// matches io.ErrUnexpectedEOF under errors.Is. A bad CRC, an impossible
// length or a malformed header block is an error; any other read error is
// returned as is (before a frame starts) or wrapped (inside one).
func (r *Reader) Next() (Message, error) {
	var prelude [PreludeLen]byte
	if _, err := io.ReadFull(r.src, prelude[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return Message{}, cut("eventstream: truncated frame prelude", err)
		}
		return Message{}, err // io.EOF between frames: clean end of stream
	}
	total := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:12]) {
		return Message{}, fmt.Errorf("eventstream: prelude CRC mismatch")
	}
	if total < PreludeLen+TrailerLen || total > MaxFrameLen || headersLen > total-PreludeLen-TrailerLen {
		return Message{}, fmt.Errorf("eventstream: invalid frame lengths (total %d, headers %d)", total, headersLen)
	}
	rest := make([]byte, total-PreludeLen)
	if _, err := io.ReadFull(r.src, rest); err != nil {
		return Message{}, cut("eventstream: truncated frame body", err)
	}
	body := rest[:len(rest)-TrailerLen]
	want := binary.BigEndian.Uint32(rest[len(rest)-TrailerLen:])
	sum := crc32.ChecksumIEEE(prelude[:])
	sum = crc32.Update(sum, crc32.IEEETable, body)
	if sum != want {
		return Message{}, fmt.Errorf("eventstream: message CRC mismatch")
	}
	headers, err := parseHeaders(body[:headersLen])
	if err != nil {
		return Message{}, err
	}
	return Message{Headers: headers, Payload: body[headersLen:]}, nil
}

// parseHeaders decodes the header block: repeated
// name length (1) | name | value type (1) | value, where the only value
// type Bedrock uses is 7 (string: length (2) | bytes). Other types are
// skipped by their fixed size so an unexpected header never desyncs.
func parseHeaders(b []byte) (map[string]string, error) {
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

// Encode builds one message (the inverse of Next) with string headers only,
// written in name order so the bytes are deterministic. It exists for tests
// and fixtures that need a real Bedrock-shaped stream; the gateway never
// encodes one.
func Encode(headers map[string]string, payload []byte) []byte {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var hb bytes.Buffer
	for _, name := range names {
		value := headers[name]
		hb.WriteByte(byte(len(name)))
		hb.WriteString(name)
		hb.WriteByte(7)
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(value)))
		hb.Write(l[:])
		hb.WriteString(value)
	}
	total := PreludeLen + hb.Len() + len(payload) + TrailerLen
	frame := make([]byte, 0, total)
	var prelude [PreludeLen]byte
	binary.BigEndian.PutUint32(prelude[0:4], uint32(total))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(hb.Len()))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[:8]))
	frame = append(frame, prelude[:]...)
	frame = append(frame, hb.Bytes()...)
	frame = append(frame, payload...)
	var trailer [TrailerLen]byte
	binary.BigEndian.PutUint32(trailer[:], crc32.ChecksumIEEE(frame))
	return append(frame, trailer[:]...)
}
