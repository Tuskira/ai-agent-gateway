package dptest

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// The server-to-client stream.
//
// A GET on the backend opens a Streamable HTTP server-to-client stream
// (or answers 405 under BackendOptions.NoStream). Every message a test
// pushes -- Notify, ResourceUpdated, Request -- is numbered with an SSE
// event id and written to every stream open at the time, and a GET that
// carries Last-Event-ID is first replayed everything after that id, the
// way a server with resumable streams behaves. DropStreams ends every
// open stream from the server side.

// sseStream is one open GET.
type sseStream struct {
	frames chan []byte
	drop   chan struct{}
}

type streamEvent struct {
	id    string
	frame []byte
}

// StreamGETs returns how many GETs the backend received, refused ones
// included.
func (b *Backend) StreamGETs() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.streamGETs
}

// StreamHeaders returns a copy of the headers of the most recent GET.
func (b *Backend) StreamHeaders() http.Header {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.streamHeaders.Clone()
}

// OpenStreams returns how many streams are open right now.
func (b *Backend) OpenStreams() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.streams)
}

// LastEventIDs returns the Last-Event-ID each accepted GET carried ("" for
// none), in order.
func (b *Backend) LastEventIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lastEventIDs...)
}

// Subscribed reports whether uri is currently subscribed.
func (b *Backend) Subscribed(uri string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.subscribed[uri]
}

// Replies returns every JSON-RPC response the client POSTed back to a
// request sent with Request, in order.
func (b *Backend) Replies() []mcp.Response {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]mcp.Response(nil), b.replies...)
}

// Notify pushes a notification to every open stream and returns its
// event id.
func (b *Backend) Notify(method string, params any) string {
	msg := mcp.Request{JSONRPC: mcp.Version, Method: method}
	if params != nil {
		raw, _ := json.Marshal(params)
		msg.Params = raw
	}
	return b.push(msg)
}

// ResourceUpdated pushes notifications/resources/updated for uri, as a
// real server does, only when uri is subscribed. It reports whether it
// pushed anything.
func (b *Backend) ResourceUpdated(uri string) bool {
	if !b.Subscribed(uri) {
		return false
	}
	b.Notify(mcp.NotificationResourcesUpdated, mcp.ResourceUpdatedParams{URI: uri})
	return true
}

// Request pushes a server-to-client request to every open stream and
// returns its JSON-RPC id. The client's answer arrives as a POST and is
// recorded in Replies (see WaitReply).
func (b *Backend) Request(method string, params any) string {
	b.mu.Lock()
	b.nextRequestID++
	id := fmt.Sprintf("srv-%d", b.nextRequestID)
	b.mu.Unlock()

	msg := mcp.Request{JSONRPC: mcp.Version, ID: id, Method: method}
	if params != nil {
		raw, _ := json.Marshal(params)
		msg.Params = raw
	}
	b.push(msg)
	return id
}

// DropStreams ends every open stream from the server side, as a
// restarting server or a proxy's idle cut does.
func (b *Backend) DropStreams() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for st := range b.streams {
		close(st.drop)
		delete(b.streams, st)
	}
}

func (b *Backend) push(msg any) string {
	raw, _ := json.Marshal(msg)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextEvent++
	id := fmt.Sprintf("ev-%d", b.nextEvent)
	frame := []byte(fmt.Sprintf("id: %s\nevent: message\ndata: %s\n\n", id, raw))
	b.history = append(b.history, streamEvent{id: id, frame: frame})
	for st := range b.streams {
		select {
		case st.frames <- frame:
		default:
		}
	}
	return id
}

func (b *Backend) serveStream(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.streamGETs++
	b.streamHeaders = r.Header.Clone()
	noStream, sessionID := b.noStream, b.sessionID
	b.mu.Unlock()

	if noStream {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if b.requireSession && r.Header.Get(mcp.HeaderSessionID) != sessionID {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", http.StatusInternalServerError)
		return
	}

	st := &sseStream{frames: make(chan []byte, 64), drop: make(chan struct{})}
	lastID := r.Header.Get("Last-Event-ID")

	b.mu.Lock()
	b.lastEventIDs = append(b.lastEventIDs, lastID)
	var replay [][]byte
	if lastID != "" {
		for i, ev := range b.history {
			if ev.id == lastID {
				for _, later := range b.history[i+1:] {
					replay = append(replay, later.frame)
				}
				break
			}
		}
	}
	b.streams[st] = struct{}{}
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.streams, st)
		b.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for _, frame := range replay {
		_, _ = w.Write(frame)
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-st.drop:
			return
		case frame := <-st.frames:
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
