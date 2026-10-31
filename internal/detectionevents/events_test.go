package detectionevents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestEmitterBatchesToAgent(t *testing.T) {
	var mu sync.Mutex
	var got []Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/events" {
			http.NotFound(w, r)
			return
		}
		var b batch
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		got = append(got, b.Events...)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	e := New(Config{AgentURL: srv.URL, FlushInterval: 20 * time.Millisecond})
	e.Emit(Event{Kind: KindToolCall, TenantID: "t", Plane: "mcp", Tool: "page", Outcome: OutcomeMonitored, Arguments: `{"a":1}`})
	e.Emit(Event{Kind: KindRefused, TenantID: "t", Plane: "llm", Outcome: OutcomeRefused, Reason: "key suspended"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	e.Close(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].V != 1 || got[0].At.IsZero() || got[1].Reason != "key suspended" {
		t.Fatalf("got %+v", got)
	}
	if st := e.Status(); st["sent"] != uint64(2) {
		t.Fatalf("status %v", st)
	}
}

func TestEmitterNeverBlocksAndNilIsNoop(t *testing.T) {
	var nilE *Emitter
	nilE.Emit(Event{Kind: KindToolCall})
	nilE.Close(context.Background())

	e := New(Config{AgentURL: "http://127.0.0.1:1", QueueSize: 1, Timeout: 50 * time.Millisecond})
	for range 100 {
		e.Emit(Event{Kind: KindToolCall, TenantID: "t"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e.Close(ctx)
	if st := e.Status(); st["dropped"].(uint64)+st["failed"].(uint64) == 0 {
		t.Fatalf("status %v", st)
	}
}
