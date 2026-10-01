package transport_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// openStream opens GET /mcp/stream (for a session when sessionID is set)
// and returns every data frame as it arrives. The handler subscribes
// before sending its headers, so once this returns the stream is live.
func (f *fixture) openStream(t *testing.T, sessionID string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/mcp/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if sessionID != "" {
		req.Header.Set(mcp.HeaderSessionID, sessionID)
	}
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("GET /mcp/stream: Content-Type %q, body %s", ct, body)
	}

	frames := make(chan string, 64)
	go func() {
		defer close(frames)
		defer resp.Body.Close()
		lines := bufio.NewScanner(resp.Body)
		for lines.Scan() {
			if data, ok := strings.CutPrefix(lines.Text(), "data: "); ok {
				frames <- data
			}
		}
	}()
	return frames
}

// post sends one JSON-RPC message without failing the test from a
// goroutine.
func (f *fixture) post(sessionID string, req mcp.Request) (int, mcp.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return 0, mcp.Response{}, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, f.server.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return 0, mcp.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set(mcp.HeaderSessionID, sessionID)
	resp, err := f.server.Client().Do(httpReq)
	if err != nil {
		return 0, mcp.Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, mcp.Response{}, err
	}
	var decoded mcp.Response
	if len(bytes.TrimSpace(raw)) > 0 {
		err = json.Unmarshal(raw, &decoded)
	}
	return resp.StatusCode, decoded, err
}

func slowCallRequest(id any, args, meta string) mcp.Request {
	params := `{"name":"alpha__` + dptest.SlowTool + `","arguments":` + args
	if meta != "" {
		params += `,"_meta":` + meta
	}
	return mcp.Request{JSONRPC: mcp.Version, ID: id, Method: mcp.MethodToolsCall, Params: json.RawMessage(params + "}")}
}

func cancelRequest(requestID string) mcp.Request {
	return mcp.Request{
		JSONRPC: mcp.Version, Method: mcp.NotificationCancelled,
		Params: json.RawMessage(`{"requestId":` + requestID + `,"reason":"user abort"}`),
	}
}

func TestProgressReachesOnlyTheCallingSessionsStream(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	sessionID := f.initialize(t)
	otherID := f.initialize(t)

	frames := f.openStream(t, sessionID)
	otherFrames := f.openStream(t, otherID)

	status, resp, err := f.post(sessionID, slowCallRequest(3, `{"steps":3,"foreign":true}`, `{"progressToken":42}`))
	if err != nil || status != http.StatusOK || resp.Error != nil {
		t.Fatalf("tools/call = %d %+v %v", status, resp.Error, err)
	}
	var result mcp.ToolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil || len(result.Content) != 1 ||
		result.Content[0].Text != "slow: done after 3 steps" {
		t.Fatalf("tools/call result = %s", resp.Result)
	}

	for want := 1; want <= 3; want++ {
		select {
		case frame := <-frames:
			var note mcp.Request
			if err := json.Unmarshal([]byte(frame), &note); err != nil {
				t.Fatalf("frame %q: %v", frame, err)
			}
			var p mcp.ProgressParams
			if err := json.Unmarshal(note.Params, &p); err != nil {
				t.Fatal(err)
			}
			if note.Method != mcp.NotificationProgress || string(p.ProgressToken) != "42" || p.Progress != float64(want) {
				t.Fatalf("frame %d = %s, want progress %d for token 42", want, frame, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("progress %d never reached the session's stream", want)
		}
	}

	select {
	case frame := <-otherFrames:
		t.Fatalf("another session's stream received %s", frame)
	case frame := <-frames:
		t.Fatalf("an extra frame (the foreign token?) reached the stream: %s", frame)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAStreamForAnUnknownSessionIsRefused(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	req, err := http.NewRequest(http.MethodGet, f.server.URL+"/mcp/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set(mcp.HeaderSessionID, "not-a-real-session")
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var decoded mcp.Response
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Error == nil || decoded.Error.Code != mcp.ErrorCodeSessionNotFound {
		t.Fatalf("error = %+v, want -32000", decoded.Error)
	}
}

func TestNotificationsCancelledStopsAToolsCallOverHTTP(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	sessionID := f.initialize(t)

	type outcome struct {
		status int
		resp   mcp.Response
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		status, resp, err := f.post(sessionID, slowCallRequest(7, `{"steps":1,"hold":true}`, ""))
		done <- outcome{status, resp, err}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for f.backend.Running() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the call never reached the backend")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// An id nothing is running under: acknowledged like any
	// notification, and nothing is cancelled.
	if status, _, err := f.post(sessionID, cancelRequest("12345")); err != nil || status != http.StatusNoContent {
		t.Fatalf("unknown-id cancel = %d, %v; want 204", status, err)
	}
	if status, _, err := f.post(sessionID, cancelRequest("7")); err != nil || status != http.StatusNoContent {
		t.Fatalf("cancel = %d, %v; want 204", status, err)
	}

	select {
	case out := <-done:
		if out.err != nil || out.status != http.StatusOK {
			t.Fatalf("tools/call = %d, %v; want HTTP 200", out.status, out.err)
		}
		if out.resp.Error == nil || out.resp.Error.Code != mcp.ErrorCodeRequestCancelled {
			t.Fatalf("tools/call error = %+v, want -32800", out.resp.Error)
		}
		if id, _ := out.resp.ID.(float64); id != 7 {
			t.Errorf("response id = %v, want 7", out.resp.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled tools/call never returned")
	}

	deadline = time.Now().Add(5 * time.Second)
	for len(f.backend.Cancellations()) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("backend saw %d cancellations, want 1", len(f.backend.Cancellations()))
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := f.backend.Cancellations()[0].RequestID; got != f.backend.SlowCallIDs()[0] {
		t.Errorf("forwarded cancellation named %q, want the upstream id %q", got, f.backend.SlowCallIDs()[0])
	}

	conn, err := f.store.Connectors().Get(context.Background(), tenantID, f.conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if conn.Status != "healthy" {
		t.Errorf("connector status = %q after a cancellation, want healthy", conn.Status)
	}
}
