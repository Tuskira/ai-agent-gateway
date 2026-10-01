package dptest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// Server-to-client requests.
//
// A real MCP server asks its client for things -- sampling/createMessage,
// elicitation/create, roots/list -- either inside the streamed reply to a
// tools/call (AskTool) or on its long-lived GET stream (Backend.Request).
// Either way the client answers with a POST carrying a JSON-RPC response,
// which the backend records (Replies) and hands to whoever is waiting on
// that id (WaitReply, or the AskTool call that asked).

// AskTool is the name of a tool every Backend serves (unless Handlers
// overrides it) whether or not it is advertised: mid-call it sends
// server-to-client requests on its streamed reply, waits for the client's
// answers, and returns them as its result -- one text content holding
// the JSON array of the responses received, in the order asked
// ([{"id":...,"result":...}|{"id":...,"error":...}]). AskToolDef
// advertises it.
//
// Arguments:
//
//	method    the request's method (default "elicitation/create")
//	params    its params, sent verbatim (default {})
//	count     how many to send at once (default 1)
//	delay_ms  pause before sending them
//	after_ms  pause after the answers, before the result
//
// The call ends without a result when it is cancelled
// (notifications/cancelled for its id) or the request goes away.
const AskTool = "ask"

// AskToolDef is AskTool's tools/list entry.
func AskToolDef() mcp.Tool {
	return mcp.Tool{
		Name:        AskTool,
		Description: "Asks the client (sampling, elicitation, roots) mid-call and returns its answers",
		InputSchema: mcp.InputSchema{Type: "object", Properties: map[string]any{
			"method":   map[string]any{"type": "string"},
			"params":   map[string]any{"type": "object"},
			"count":    map[string]any{"type": "number"},
			"delay_ms": map[string]any{"type": "number"},
			"after_ms": map[string]any{"type": "number"},
		}},
	}
}

// InitializeCapabilities returns params.capabilities of every initialize
// the backend received, verbatim, in order.
func (b *Backend) InitializeCapabilities() []json.RawMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]json.RawMessage(nil), b.initCaps...)
}

// AskRequests returns every request AskTool sent, in order.
func (b *Backend) AskRequests() []mcp.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]mcp.Request(nil), b.asked...)
}

// WaitReply waits up to d for the client's answer to the server-to-client
// request id (one sent with Request or by AskTool).
func (b *Backend) WaitReply(id string, d time.Duration) (mcp.Response, bool) {
	deadline := time.Now().Add(d)
	for {
		for _, r := range b.Replies() {
			if mcp.FormatID(r.ID) == id {
				return r, true
			}
		}
		if time.Now().After(deadline) {
			return mcp.Response{}, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recordReply keeps a client's answer and wakes the AskTool call
// waiting for it.
func (b *Backend) recordReply(resp mcp.Response) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.replies = append(b.replies, resp)
	id := mcp.FormatID(resp.ID)
	if ch, ok := b.waiting[id]; ok {
		ch <- resp
		delete(b.waiting, id)
	}
}

// serveAsk runs AskTool.
func (b *Backend) serveAsk(w http.ResponseWriter, r *http.Request, id any, params mcp.ToolsCallParams) {
	method, _ := params.Arguments["method"].(string)
	if method == "" {
		method = "elicitation/create"
	}
	reqParams := json.RawMessage(`{}`)
	if p, ok := params.Arguments["params"]; ok {
		reqParams, _ = json.Marshal(p)
	}
	count := 1
	if v, ok := params.Arguments["count"].(float64); ok && v > 0 {
		count = int(v)
	}
	pause := func(arg string) time.Duration {
		v, _ := params.Arguments[arg].(float64)
		return time.Duration(v) * time.Millisecond
	}

	key := mcp.FormatID(id)
	stop := make(chan struct{})
	b.mu.Lock()
	b.running[key] = stop
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.running[key] == stop {
			delete(b.running, key)
		}
		b.mu.Unlock()
	}()

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	send := func(msg any) {
		raw, _ := json.Marshal(msg)
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw)
		if flusher != nil {
			flusher.Flush()
		}
	}
	wait := func(d time.Duration) bool {
		select {
		case <-stop:
			return false
		case <-r.Context().Done():
			return false
		case <-time.After(d):
			return true
		}
	}

	if d := pause("delay_ms"); d > 0 && !wait(d) {
		return
	}

	ids := make([]string, count)
	answers := make([]chan mcp.Response, count)
	for i := range ids {
		b.mu.Lock()
		b.nextRequestID++
		ids[i] = fmt.Sprintf("ask-%d", b.nextRequestID)
		answers[i] = make(chan mcp.Response, 1)
		b.waiting[ids[i]] = answers[i]
		req := mcp.Request{JSONRPC: mcp.Version, ID: ids[i], Method: method, Params: reqParams}
		b.asked = append(b.asked, req)
		b.mu.Unlock()
		send(req)
	}
	defer func() {
		b.mu.Lock()
		for _, id := range ids {
			delete(b.waiting, id)
		}
		b.mu.Unlock()
	}()

	got := make([]mcp.Response, count)
	for i := range answers {
		select {
		case got[i] = <-answers[i]:
		case <-stop:
			return // cancelled: the spec says send no response
		case <-r.Context().Done():
			return
		}
	}

	if d := pause("after_ms"); d > 0 && !wait(d) {
		return
	}
	raw, _ := json.Marshal(got)
	send(mcp.NewSuccessResponse(id, mcp.ToolsCallResult{Content: []mcp.Content{mcp.TextContent(string(raw))}}))
}
