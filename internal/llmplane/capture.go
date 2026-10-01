package llmplane

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// lastJSONObjectForKey returns the bytes of the JSON object that is the VALUE of
// the last occurrence of any of keys (e.g. "usage", "usageMetadata"), matched with
// balanced braces (string-aware). It is the fallback when a non-stream response is
// larger than the usage scan window: a whole-body json.Unmarshal fails on the
// truncated fragment, but the trailing usage object is intact in the tail. Returns
// nil if no key is found or the object itself was cut off.
func lastJSONObjectForKey(body []byte, keys ...string) []byte {
	start := -1
	for _, k := range keys {
		if i := bytes.LastIndex(body, []byte(`"`+k+`"`)); i > start {
			start = i
		}
	}
	if start < 0 {
		return nil
	}
	i := start
	for i < len(body) && body[i] != '{' {
		i++
	}
	if i >= len(body) {
		return nil
	}
	depth, inStr, esc := 0, false, false
	for j := i; j < len(body); j++ {
		c := body[j]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return body[i : j+1]
			}
		}
	}
	return nil
}

// boundedCap is the tee target: it captures at most limit bytes of a stream and
// sets Truncated once anything is dropped, but always reports a full write so the
// io.MultiWriter relaying to the client is never short-circuited — an unbounded
// copy of a large stream could otherwise exhaust memory.
type boundedCap struct {
	buf       []byte
	limit     int
	Truncated bool
}

func newBoundedCap(limit int) *boundedCap { return &boundedCap{limit: limit} }

func (b *boundedCap) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		// Unbounded: capture the full body (lossless capture of huge session
		// bodies). ponytail: memory grows with body size until the call is
		// recorded; llm_proxy.capture.body_store (sink.BodyStore) moves the
		// bytes out of the capture row, not out of this buffer.
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	room := b.limit - len(b.buf)
	if room <= 0 {
		b.Truncated = true
		return len(p), nil
	}
	if room < len(p) {
		b.buf = append(b.buf, p[:room]...)
		b.Truncated = true
	} else {
		b.buf = append(b.buf, p...)
	}
	return len(p), nil // accept all bytes so the client copy is complete
}

// Bytes returns the captured prefix.
func (b *boundedCap) Bytes() []byte { return b.buf }

// tailCap keeps the LAST `limit` bytes written (boundedCap keeps the first).
// Used for usage parsing: a stream's final usage frame (Anthropic message_delta,
// the trailing usage object every provider places at the end of a non-stream
// JSON body) is always in the tail, so token accounting stays correct even when
// the response is far larger than the body-storage cap. limit<=0 keeps all.
type tailCap struct {
	buf   []byte
	limit int
}

func newTailCap(limit int) *tailCap { return &tailCap{limit: limit} }

func (t *tailCap) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if t.limit > 0 && len(t.buf) > 2*t.limit {
		// Trim to the last limit bytes, copying to a fresh slice so the large
		// backing array is freed; amortized O(1) per byte.
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.limit:]...)
	}
	return len(p), nil
}

func (t *tailCap) Bytes() []byte {
	if t.limit > 0 && len(t.buf) > t.limit {
		return t.buf[len(t.buf)-t.limit:]
	}
	return t.buf
}

// flushWriter flushes to the client after every write so SSE frames arrive as
// they are produced instead of buffering until the response ends. It keeps the
// write error so the router can tell a client disconnect from an upstream one.
type flushWriter struct {
	w   io.Writer
	f   http.Flusher
	err error
}

func newFlushWriter(w http.ResponseWriter) *flushWriter {
	f, _ := w.(http.Flusher)
	return &flushWriter{w: w, f: f}
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err != nil {
		fw.err = err // io.Copy stops at the first write error
		return n, err
	}
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, nil
}

// usageChunkStripper relays an OpenAI SSE stream to the client minus the final
// usage-only chunk ("choices":[] + a usage object) that InjectStreamUsage caused
// upstream to send. Events are forwarded whole, as soon as their blank-line
// terminator arrives; Flush forwards any unterminated remainder at the end.
type usageChunkStripper struct {
	w       io.Writer
	buf     []byte
	passthr bool // no event terminator within usageScanBytes: relay verbatim from here on
}

func (s *usageChunkStripper) Write(p []byte) (int, error) {
	if s.passthr {
		return s.w.Write(p)
	}
	s.buf = append(s.buf, p...)
	if sseEventEnd(s.buf) < 0 && len(s.buf) > usageScanBytes {
		// Not SSE-framed after all (an upstream ignoring stream); stop buffering.
		s.passthr = true
		return len(p), s.Flush()
	}
	for {
		n := sseEventEnd(s.buf)
		if n < 0 {
			return len(p), nil
		}
		if ev := s.buf[:n]; !isUsageOnlyChunk(ev) {
			if _, err := s.w.Write(ev); err != nil {
				return 0, err
			}
		}
		s.buf = s.buf[n:]
	}
}

// sseEventEnd returns the length of the first complete SSE event in b
// (terminated by a blank line, LF or CRLF), or -1 if none is complete yet.
func sseEventEnd(b []byte) int {
	lf, crlf := bytes.Index(b, []byte("\n\n")), bytes.Index(b, []byte("\r\n\r\n"))
	switch {
	case lf >= 0 && (crlf < 0 || lf < crlf):
		return lf + 2
	case crlf >= 0:
		return crlf + 4
	}
	return -1
}

// Flush forwards whatever is buffered (a stream that did not end on an event
// boundary, or a non-SSE error body).
func (s *usageChunkStripper) Flush() error {
	if len(s.buf) == 0 {
		return nil
	}
	_, err := s.w.Write(s.buf)
	s.buf = nil
	return err
}

func isUsageOnlyChunk(ev []byte) bool {
	data, ok := bytes.CutPrefix(bytes.TrimSpace(ev), []byte("data:"))
	if !ok {
		return false
	}
	var c struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   json.RawMessage   `json:"usage"`
	}
	if json.Unmarshal(bytes.TrimSpace(data), &c) != nil {
		return false
	}
	return c.Choices != nil && len(c.Choices) == 0 && len(c.Usage) > 0 && string(c.Usage) != "null"
}
