package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire"
)

// The caller's address the gateway sends on a turn, an inline stage and an
// event reaches the engine in the detect request's meta, the observation's
// meta and the event; with OmitClientIP set it is blanked in all of them.
func TestClientIPForwardedOrBlanked(t *testing.T) {
	for _, omit := range []bool{false, true} {
		want := "198.51.100.7"
		if omit {
			want = ""
		}
		f := newFakeEngine(t)
		f.policies["t1"] = wire.PolicyResponse{Inline: true, WantResponse: true, Observe: true}
		_, url := startAgent(t, f, Config{OmitClientIP: omit})

		// A complete turn: request and response stages, and an observation.
		tr := fullTurn("hello", true)
		tr.ClientIP = "198.51.100.7"
		if code := postTurn(t, url, tr); code != http.StatusAccepted {
			t.Fatalf("turn: %d", code)
		}
		for range 2 {
			if got := f.next(t); got.req.Meta.ClientIP != want {
				t.Errorf("omit %v: turn stage %s client_ip %q; want %q", omit, got.req.Turn.Stage, got.req.Meta.ClientIP, want)
			}
		}
		waitUntil(t, "observation", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.observations) == 1 })

		// An inline request stage.
		u := userTurn("t1", "hello")
		u.ClientIP = "198.51.100.7"
		if code, _ := post(t, url, wire.PathTurnRequest, u); code != http.StatusOK {
			t.Fatalf("stage: %d", code)
		}
		if got := f.next(t); got.req.Meta.ClientIP != want {
			t.Errorf("omit %v: inline stage client_ip %q; want %q", omit, got.req.Meta.ClientIP, want)
		}

		// An event.
		b, _ := json.Marshal(wire.EventBatch{Events: []wire.Event{{Kind: wire.EventToolCall, TenantID: "t1", Plane: "mcp",
			ClientIP: "198.51.100.7", Server: "alpha", Tool: "echo", Outcome: wire.OutcomeAllowed, At: time.Now()}}})
		resp, err := http.Post(url+wire.PathEvents, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		waitUntil(t, "event", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.events) == 1 })

		f.mu.Lock()
		if got := f.observations[0].Meta.ClientIP; got != want {
			t.Errorf("omit %v: observation client_ip %q; want %q", omit, got, want)
		}
		if got := f.events[0].ClientIP; got != want {
			t.Errorf("omit %v: event client_ip %q; want %q", omit, got, want)
		}
		f.mu.Unlock()
	}
}
