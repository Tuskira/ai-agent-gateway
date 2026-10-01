package client

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

func TestOpenStreamSendsTheCallsHeadersAndReadsMessagesWithTheirIDs(t *testing.T) {
	seen := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		seen <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, ": keep-alive\n\n")
		_, _ = fmt.Fprint(w, "id: 7\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/resources/updated\",\"params\":{\"uri\":\"x\"}}\n\n")
		// No id: the previous one still stands, per the SSE spec.
		_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"r1\",\"method\":\"roots/list\"}\n\n")
		_, _ = fmt.Fprint(w, "event: other\ndata: ignored\n\n")
	}))
	defer srv.Close()

	c := newTestClient(t, nil)
	st, err := c.OpenStream(authedContext(), Call{
		Connector: connector(srv.URL), SessionID: "backend-1", ProtocolVersion: mcp.ProtocolVersionLegacy,
	}, "6")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	h := <-seen
	for name, want := range map[string]string{
		"Accept":                  "text/event-stream",
		mcp.HeaderSessionID:       "backend-1",
		mcp.HeaderProtocolVersion: mcp.ProtocolVersionLegacy,
		HeaderLastEventID:         "6",
		HeaderTenantID:            "tenant-a",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	for _, want := range []struct{ id, method string }{{"7", "notifications/resources/updated"}, {"7", "roots/list"}} {
		msg, err := st.Next()
		if err != nil {
			t.Fatal(err)
		}
		if msg.EventID != want.id || !strings.Contains(string(msg.Raw), want.method) {
			t.Fatalf("message = %q / %s, want id %q carrying %s", msg.EventID, msg.Raw, want.id, want.method)
		}
	}
	if _, err := st.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last message: %v, want io.EOF", err)
	}
}

func TestOpenStreamClassifiesRefusals(t *testing.T) {
	for _, tc := range []struct {
		status      int
		unsupported bool
	}{
		{http.StatusMethodNotAllowed, true},
		{http.StatusNotFound, true},
		{http.StatusBadGateway, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", tc.status)
		}))
		_, err := newTestClient(t, nil).OpenStream(authedContext(), Call{Connector: connector(srv.URL)}, "")
		srv.Close()

		if got := errors.Is(err, ErrStreamUnsupported); got != tc.unsupported {
			t.Errorf("status %d: ErrStreamUnsupported = %v, want %v (err %v)", tc.status, got, tc.unsupported, err)
		}
		var statusErr *StreamStatusError
		if !tc.unsupported && (!errors.As(err, &statusErr) || statusErr.Status != tc.status) {
			t.Errorf("status %d: err = %v, want a StreamStatusError", tc.status, err)
		}
	}
}

func TestOpenStreamBoundsOnlyTheWaitForHeaders(t *testing.T) {
	// Headers that never come are a timeout.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)

	conn := connector(slow.URL)
	conn.TimeoutMS = 50
	if _, err := newTestClient(t, nil).OpenStream(authedContext(), Call{Connector: conn}, ""); !IsTimeout(err) {
		t.Fatalf("err = %v, want a timeout", err)
	}

	// Headers that come promptly start a stream that may then sit idle
	// well past the timeout.
	idle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/prompts/list_changed\"}\n\n")
	}))
	defer idle.Close()
	conn = connector(idle.URL)
	conn.TimeoutMS = 50
	st, err := newTestClient(t, nil).OpenStream(authedContext(), Call{Connector: conn}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if msg, err := st.Next(); err != nil || !strings.Contains(string(msg.Raw), "list_changed") {
		t.Fatalf("Next after idling past the timeout = %s, %v", msg.Raw, err)
	}
}
