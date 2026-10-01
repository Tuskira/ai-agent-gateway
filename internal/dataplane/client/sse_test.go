package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

func TestStreamedNotificationsReachOnNotificationBeforeTheResponse(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isReply(r) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		frame := func(s string) {
			_, _ = fmt.Fprint(w, s)
			flusher.Flush()
		}
		frame(": keep-alive\n\n")
		frame("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progressToken\":\"t\",\"progress\":1}}\n\n")
		// A server-to-client request is not mistaken for the response.
		frame("data: {\"jsonrpc\":\"2.0\",\"id\":99,\"method\":\"sampling/createMessage\",\"params\":{}}\n\n")
		// A data field split over two lines is one message (CRLF endings).
		frame("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\r\ndata: \"params\":{\"progressToken\":\"t\",\"progress\":2}}\r\n\r\n")
		<-release
		frame("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n")
		// A server that holds the stream open after its response must
		// not hold the call open with it.
		<-r.Context().Done()
	}))
	defer srv.Close()

	var (
		mu    sync.Mutex
		notes []mcp.Request
	)
	call := pingCall(connector(srv.URL))
	call.OnNotification = func(n mcp.Request) {
		mu.Lock()
		notes = append(notes, n)
		if len(notes) == 2 {
			close(release)
		}
		mu.Unlock()
	}

	c := newTestClient(t, nil)
	result, err := c.Do(authedContext(), call)
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	if result.Response == nil || result.Response.Error != nil || string(result.Response.Result) != "{}" {
		t.Fatalf("response = %+v, want the final result", result.Response)
	}
	if result.BytesIn == 0 {
		t.Error("BytesIn = 0 for a streamed reply")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(notes) != 2 {
		t.Fatalf("got %d notifications, want 2 (the server request is not one): %+v", len(notes), notes)
	}
	for i, n := range notes {
		var p mcp.ProgressParams
		if n.Method != mcp.NotificationProgress || json.Unmarshal(n.Params, &p) != nil || p.Progress != float64(i+1) {
			t.Errorf("notification %d = %+v", i, n)
		}
	}
}

func TestAStreamWithoutAResponseFallsBackToItsLastFrame(t *testing.T) {
	resp, _, err := readSSE(strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"a\":1}}"), nil, nil)
	if err != nil || resp == nil || string(resp.Result) != `{"a":1}` {
		t.Fatalf("readSSE = %+v, %v; want the unterminated final frame", resp, err)
	}
	if _, _, err := readSSE(strings.NewReader(": only a comment\n\n"), nil, nil); err == nil {
		t.Fatal("readSSE of a stream with no data = nil error")
	}
	if _, _, err := readSSE(strings.NewReader("event: ping\ndata: {\"x\":1}\n\n"), nil, nil); err == nil {
		t.Fatal("a non-message event was taken as the response")
	}
}

func TestACancelledStreamAbortsWithoutRetryOrTimeout(t *testing.T) {
	var attempts atomic.Int32
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progressToken\":1,\"progress\":1}}\n\n")
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(authedContext())
	go func() {
		<-started
		cancel()
	}()

	c := newTestClient(t, nil)
	begin := time.Now()
	_, err := c.Do(ctx, pingCall(connector(srv.URL)))
	if err == nil {
		t.Fatal("Do = nil error after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Do error = %v, want context.Canceled in its chain", err)
	}
	if IsTimeout(err) {
		t.Errorf("a cancellation was classified as a timeout: %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1: a cancelled call is never retried", got)
	}
	if time.Since(begin) > time.Second {
		t.Error("the cancelled read did not stop promptly")
	}
}

func TestCallToolForwardsMetaAndTheChosenRequestID(t *testing.T) {
	var got mcp.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mcp.NewSuccessResponse(got.ID, mcp.ToolsCallResult{Content: []mcp.Content{}}))
	}))
	defer srv.Close()

	call := Call{Connector: connector(srv.URL), RequestID: "tools/call:x:abc", Meta: json.RawMessage(`{"progressToken":42}`)}
	c := newTestClient(t, nil)
	if _, _, err := c.CallTool(authedContext(), call, "x", nil); err != nil {
		t.Fatalf("CallTool = %v", err)
	}
	if got.ID != "tools/call:x:abc" {
		t.Errorf("upstream id = %v, want the chosen one", got.ID)
	}
	var params mcp.ToolsCallParams
	if err := json.Unmarshal(got.Params, &params); err != nil {
		t.Fatal(err)
	}
	if string(params.Meta) != `{"progressToken":42}` {
		t.Errorf("upstream _meta = %s, want it verbatim", params.Meta)
	}
}
