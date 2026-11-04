package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/clientip"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/orchestrator"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/detectionevents"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// eventsAgent is a stand-in detection agent that records the events posted
// to it.
func eventsAgent(t *testing.T) (*detectionevents.Emitter, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Events []map[string]any `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		got = append(got, b.Events...)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	e := detectionevents.New(detectionevents.Config{AgentURL: srv.URL, FlushInterval: 10 * time.Millisecond})
	return e, func() []map[string]any {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		e.Close(ctx)
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

// Refused and tool_call events carry the caller's address as the gateway
// resolves it: through a trusted proxy, the forwarded client.
func TestDetectionEventsCarryClientIP(t *testing.T) {
	e, wait := eventsAgent(t)
	resolver := clientip.MustNew("10.0.0.0/8")

	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.RemoteAddr = "10.1.2.3:4567"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	refusedEvents(e, resolver.IP)(r, &store.APIKey{ID: "key-1", TenantID: "t1"}, "key suspended")
	toolCallEvents(e)(context.Background(), orchestrator.ToolCall{TenantID: "t1", KeyID: "key-1", ClientIP: "203.0.113.9",
		Server: "alpha", Tool: "echo", Outcome: orchestrator.ToolCallAllowed})

	got := wait()
	if len(got) != 2 {
		t.Fatalf("events = %v", got)
	}
	if got[0]["kind"] != "refused" || got[0]["client_ip"] != "198.51.100.7" {
		t.Errorf("refused = %v; want client_ip 198.51.100.7", got[0])
	}
	if got[1]["kind"] != "tool_call" || got[1]["client_ip"] != "203.0.113.9" {
		t.Errorf("tool_call = %v; want client_ip 203.0.113.9", got[1])
	}
}

// An unknown address is left out, not sent empty.
func TestDetectionEventsOmitUnknownClientIP(t *testing.T) {
	e, wait := eventsAgent(t)
	toolCallEvents(e)(context.Background(), orchestrator.ToolCall{TenantID: "t1", Server: "alpha", Tool: "echo", Outcome: "allowed"})
	got := wait()
	if len(got) != 1 {
		t.Fatalf("events = %v", got)
	}
	if _, ok := got[0]["client_ip"]; ok {
		t.Errorf("tool_call = %v; want no client_ip", got[0])
	}
}
