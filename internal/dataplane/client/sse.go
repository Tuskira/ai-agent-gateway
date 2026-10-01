package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// isEventStream reports whether a Content-Type names an SSE stream.
func isEventStream(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")
}

// sseMessage is just enough of a JSON-RPC message to tell a response
// from a notification from a server-to-client request.
type sseMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

// sseEvent is one dispatched SSE event.
type sseEvent struct {
	// id is the stream's last event id as of this event: the SSE spec
	// has an "id:" field persist until another one replaces it, so an
	// event without one still carries its predecessor's.
	id string
	// typ is the "event:" field; empty means the default, "message".
	typ string
	// data is the event's data lines joined by "\n", trimmed.
	data []byte
}

// sseReader splits an SSE byte stream into events. It is shared by the
// reply reader (readSSE), which stops at the first response, and by
// Stream, which reads a server-to-client stream until it ends.
type sseReader struct {
	br     *bufio.Reader
	n      int64
	lastID string
	eof    bool
}

func newSSEReader(r io.Reader) *sseReader { return &sseReader{br: bufio.NewReader(r)} }

// next returns the next event that carries data. Comments (keep-alives)
// and data-less events are consumed silently. At the end of the stream
// an event with no trailing blank line still counts and is returned
// first; after it, next returns io.EOF. Any other read error is returned
// as-is.
func (r *sseReader) next() (sseEvent, error) {
	var (
		typ  string
		data [][]byte
	)
	// dispatch builds the pending event and reports whether it has any
	// data worth returning.
	dispatch := func() (sseEvent, bool) {
		defer func() { typ, data = "", nil }()
		if len(data) == 0 {
			return sseEvent{}, false
		}
		payload := bytes.TrimSpace(bytes.Join(data, []byte("\n")))
		if len(payload) == 0 {
			return sseEvent{}, false
		}
		return sseEvent{id: r.lastID, typ: typ, data: payload}, true
	}

	for {
		if r.eof {
			return sseEvent{}, io.EOF
		}
		line, err := r.br.ReadBytes('\n')
		r.n += int64(len(line))
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(bytes.TrimSpace(line)) == 0:
				if ev, ok := dispatch(); ok {
					return ev, nil
				}
			case line[0] == ':':
				// A comment (keep-alive).
			default:
				field, value, _ := bytes.Cut(line, []byte(":"))
				value = bytes.TrimPrefix(value, []byte(" "))
				switch string(bytes.TrimSpace(field)) {
				case "event":
					typ = string(bytes.TrimSpace(value))
				case "data":
					data = append(data, value)
				case "id":
					// The spec ignores an id containing NUL.
					if !bytes.ContainsRune(value, 0) {
						r.lastID = string(value)
					}
				}
			}
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) {
			return sseEvent{}, err
		}
		r.eof = true
		if ev, ok := dispatch(); ok {
			return ev, nil
		}
		return sseEvent{}, io.EOF
	}
}

// isMessage reports whether ev carries a JSON-RPC message: MCP
// sends every message as the default event type.
func (ev sseEvent) isMessage() bool { return ev.typ == "" || ev.typ == "message" }

// readSSE reads a backend's Streamable HTTP reply to one request: zero or
// more messages the server sends while it works, then the response.
//
// Every notification seen before the response goes to onNotification (if
// set), and every server-to-client request (sampling, elicitation, roots)
// to onRequest (if set; skipped otherwise) with its raw id. Neither may
// block: the reply must keep being read while a request is answered. The
// first response ends the read -- the spec has the server close the
// stream after it, and returning then means a server that holds the
// stream open does not hold the call open with it.
//
// A stream that ends (EOF) without ever sending a response is an error,
// never a fallback to whatever message it sent last: that last message
// can be a still-unanswered request or notification (for example, a
// connector that abandons a tools/call's reply the moment it is told to
// cancel, per the spec's "send no response"), and decoding it as if it
// were the result would hand the caller a fabricated, empty success
// instead of surfacing the real failure. A single unterminated data
// frame carrying the response itself (no trailing blank line) is not
// this case: sseReader.next returns it as a normal event before EOF, so
// it is handled by the loop below like any other response.
//
// It returns the bytes read alongside the response.
func readSSE(r io.Reader, onNotification func(mcp.Request), onRequest func(id json.RawMessage, req mcp.Request)) (*mcp.Response, int64, error) {
	events := newSSEReader(r)
	sawFrame := false

	for {
		ev, err := events.next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, events.n, fmt.Errorf("client: read response: %w", err)
			}
			break
		}
		if !ev.isMessage() {
			continue
		}
		sawFrame = true

		var probe sseMessage
		if err := json.Unmarshal(ev.data, &probe); err != nil {
			return nil, events.n, fmt.Errorf("client: parse SSE response: %w", err)
		}
		noID := len(probe.ID) == 0 || string(probe.ID) == "null"
		switch {
		case probe.Method != "" && noID:
			if onNotification != nil {
				var note mcp.Request
				if err := json.Unmarshal(ev.data, &note); err == nil {
					onNotification(note)
				}
			}
			continue
		case probe.Method != "":
			if onRequest != nil {
				var req mcp.Request
				if err := json.Unmarshal(ev.data, &req); err == nil {
					onRequest(probe.ID, req)
				}
			}
			continue
		}

		var resp mcp.Response
		if err := json.Unmarshal(ev.data, &resp); err != nil {
			return nil, events.n, fmt.Errorf("client: parse SSE response: %w", err)
		}
		return &resp, events.n, nil
	}

	if !sawFrame {
		return nil, events.n, errors.New("client: parse SSE response: no data frame in SSE response")
	}
	return nil, events.n, errors.New("client: parse SSE response: stream ended before sending a response")
}
