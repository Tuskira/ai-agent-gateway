package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// isReply reports whether r is the client POSTing a JSON-RPC response
// (its answer to a server-to-client request) rather than a request. It
// leaves r.Body readable.
func isReply(r *http.Request) bool {
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(raw)))
	return !strings.Contains(string(raw), `"method"`)
}

// askingServer answers every call with a streamed reply that first sends
// the given server-to-client requests, then waits for the client's
// answers to all of them (keeping the stream moving with a progress
// notification meanwhile), then sends the call's response. It records
// the answers and the session header each arrived with.
type askingServer struct {
	*httptest.Server
	requests []string // raw JSON-RPC requests, sent in order
	before   time.Duration

	mu       sync.Mutex
	answers  []string
	sessions []string
	got      chan struct{}
	// answered receives a copy of every answer's raw JSON as it arrives,
	// in addition to got: a test that needs to react to one answer (say,
	// "the first one has arrived") reads this instead of got, since got
	// is drained by serve's own wait loop below and taking from it here
	// too would make that loop wait forever for an answer that was
	// already consumed.
	answered chan string
}

func newAskingServer(t *testing.T, before time.Duration, requests ...string) *askingServer {
	t.Helper()
	s := &askingServer{
		requests: requests, before: before,
		got:      make(chan struct{}, len(requests)+8),
		answered: make(chan string, len(requests)+8),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *askingServer) serve(w http.ResponseWriter, r *http.Request) {
	if isReply(r) {
		raw, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.answers = append(s.answers, string(raw))
		s.sessions = append(s.sessions, r.Header.Get(mcp.HeaderSessionID))
		s.mu.Unlock()
		s.got <- struct{}{}
		s.answered <- string(raw)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set(mcp.HeaderSessionID, "backend-sess")
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	frame := func(data string) {
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
		flusher.Flush()
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	select {
	case <-time.After(s.before):
	case <-r.Context().Done():
		return
	}
	for _, req := range s.requests {
		frame(req)
	}
	frame(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"t","progress":1}}`)
	for range s.requests {
		select {
		case <-s.got:
		case <-r.Context().Done():
			return
		}
	}
	frame(`{"jsonrpc":"2.0","id":1,"result":{}}`)
}

func (s *askingServer) received() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.answers...), append([]string(nil), s.sessions...)
}

func TestARequestInsideAReplyIsAnsweredUnderItsOwnIDWhileTheReplyIsRead(t *testing.T) {
	srv := newAskingServer(t, 0, `{"jsonrpc":"2.0","id":99,"method":"elicitation/create","params":{"message":"Which?"}}`)

	var (
		progressed = make(chan struct{})
		once       sync.Once
		gotReq     mcp.Request
	)
	call := pingCall(connector(srv.URL))
	call.OnNotification = func(mcp.Request) { once.Do(func() { close(progressed) }) }
	call.OnRequest = func(ctx context.Context, req mcp.Request) (json.RawMessage, *mcp.Error) {
		gotReq = req
		// The reply keeps being read while the request is answered.
		select {
		case <-progressed:
		case <-time.After(5 * time.Second):
			t.Error("a notification behind the request was not delivered while it was being answered")
		}
		return json.RawMessage(`{"action":"accept","content":{"x":1}}`), nil
	}

	result, err := newTestClient(t, nil).Do(authedContext(), call)
	if err != nil || result.Response == nil || string(result.Response.Result) != "{}" {
		t.Fatalf("Do = %+v, %v; want the final response", result, err)
	}
	if gotReq.Method != "elicitation/create" || string(gotReq.Params) != `{"message":"Which?"}` {
		t.Fatalf("OnRequest got %+v", gotReq)
	}
	answers, sessions := srv.received()
	if len(answers) != 1 || answers[0] != `{"jsonrpc":"2.0","id":99,"result":{"action":"accept","content":{"x":1}}}` {
		t.Fatalf("answers = %q, want the result under id 99", answers)
	}
	if sessions[0] != "backend-sess" {
		t.Fatalf("answer sent on session %q, want the one the reply issued", sessions[0])
	}
}

func TestWithoutOnRequestARequestIsRefused(t *testing.T) {
	srv := newAskingServer(t, 0, `{"jsonrpc":"2.0","id":"r1","method":"sampling/createMessage","params":{}}`)
	if _, err := newTestClient(t, nil).Do(authedContext(), pingCall(connector(srv.URL))); err != nil {
		t.Fatalf("Do = %v", err)
	}
	answers, _ := srv.received()
	var resp mcp.Response
	if len(answers) != 1 || json.Unmarshal([]byte(answers[0]), &resp) != nil ||
		resp.ID != "r1" || resp.Error == nil || resp.Error.Code != mcp.ErrorCodeMethodNotFound {
		t.Fatalf("answers = %q, want -32601 under r1", answers)
	}
}

func TestAtMostFourRequestsPerCallAreAnsweredAtOnce(t *testing.T) {
	var reqs []string
	for i := 1; i <= maxRequestsPerCall+1; i++ {
		reqs = append(reqs, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"roots/list"}`, i))
	}
	srv := newAskingServer(t, 0, reqs...)

	// The dispatcher's cap (requests.go's maxRequestsPerCall) is on
	// requests concurrently AWAITING an answer, not a running total: a
	// slot frees the instant its handler returns. So the first four
	// handlers must stay blocked -- occupying all four slots -- until
	// the excess (5th) request has actually been dispatched and
	// refused; otherwise one of them can finish and free its slot before
	// the 5th is even read off the stream, and the 5th gets handled
	// instead. (Confirmed by forcing it: with handlers that return
	// immediately instead of blocking, ~1 in 1500 runs saw all 5
	// handled and 0 refused -- the same "0 refused" the flaky CI
	// failure reported.) Holding the four open until the 5th is
	// observed refused makes the cap's effect deterministic, with no
	// sleeps.
	release := make(chan struct{})
	var (
		mu     sync.Mutex
		inside int
	)
	call := pingCall(connector(srv.URL))
	call.OnRequest = func(ctx context.Context, req mcp.Request) (json.RawMessage, *mcp.Error) {
		mu.Lock()
		inside++
		mu.Unlock()
		<-release
		return json.RawMessage(`{"roots":[]}`), nil
	}

	go func() {
		// The first answer to reach the server can only be the excess
		// request's refusal: the four accepted requests are all
		// blocked above, so none of them can have answered yet.
		select {
		case <-srv.answered:
		case <-time.After(5 * time.Second):
			t.Error("no request was answered while the first four were held")
		}
		close(release)
	}()

	if _, err := newTestClient(t, nil).Do(authedContext(), call); err != nil {
		t.Fatalf("Do = %v", err)
	}

	answers, _ := srv.received()
	refused := 0
	for _, a := range answers {
		var resp mcp.Response
		if err := json.Unmarshal([]byte(a), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Error != nil {
			if resp.Error.Code != mcp.ErrorCodeInternalError {
				t.Fatalf("refusal = %+v, want -32603", resp.Error)
			}
			refused++
		}
	}
	if len(answers) != maxRequestsPerCall+1 || refused != 1 || inside != maxRequestsPerCall {
		t.Fatalf("answers %q (%d refused, %d handled), want %d handled and 1 refused",
			answers, refused, inside, maxRequestsPerCall)
	}
}

func TestTheTimeoutIsPausedWhileARequestIsAnswered(t *testing.T) {
	conn := func(url string) Call {
		call := pingCall(connector(url))
		call.Connector.TimeoutMS = 300
		call.OnRequest = func(ctx context.Context, req mcp.Request) (json.RawMessage, *mcp.Error) {
			select {
			case <-time.After(600 * time.Millisecond):
			case <-ctx.Done():
				return nil, mcp.NewInternalError("cancelled")
			}
			return json.RawMessage(`{}`), nil
		}
		return call
	}
	req := `{"jsonrpc":"2.0","id":1,"method":"elicitation/create"}`

	// 100ms of the backend's time, then 600ms of the agent's: within a
	// 300ms budget.
	srv := newAskingServer(t, 100*time.Millisecond, req)
	if _, err := newTestClient(t, nil).Do(authedContext(), conn(srv.URL)); err != nil {
		t.Fatalf("Do = %v, want no timeout while the agent answers", err)
	}

	// The budget still binds the backend's own time.
	slow := newAskingServer(t, 400*time.Millisecond, req)
	if _, err := newTestClient(t, nil).Do(authedContext(), conn(slow.URL)); !IsTimeout(err) {
		t.Fatalf("Do = %v, want a timeout", err)
	}
}
