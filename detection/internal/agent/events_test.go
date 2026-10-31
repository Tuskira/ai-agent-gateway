package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/turn"
	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire"
)

// fakeSecret is assembled so secret scanners of this file stay quiet.
var fakeSecret = "AKIA" + "Z7QW3RT5YU2IOP4A"

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 300 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for " + what)
}

// The gateway's events reach the engine in a batch, with a monitored
// call's arguments redacted.
func TestEventsForwardedRedacted(t *testing.T) {
	f := newFakeEngine(t)
	_, url := startAgent(t, f, Config{})
	b, _ := json.Marshal(wire.EventBatch{Events: []wire.Event{
		{Kind: wire.EventToolCall, TenantID: "t1", Plane: "mcp", Tool: "deploy", Outcome: wire.OutcomeMonitored,
			Arguments: `{"key":"` + fakeSecret + `"}`, At: time.Now()},
		{Kind: wire.EventRefused, TenantID: "t1", Plane: "llm", Outcome: wire.OutcomeRefused, Reason: "key suspended", At: time.Now()},
	}})
	resp, err := http.Post(url+wire.PathEvents, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	waitUntil(t, "events", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.events) == 2 })
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.events[0].Arguments; strings.Contains(a, fakeSecret) || !strings.Contains(a, "[REDACTED:") || !json.Valid([]byte(a)) {
		t.Fatalf("arguments = %s", a)
	}
	if f.events[1].Reason != "key suspended" {
		t.Fatalf("event = %+v", f.events[1])
	}
}

// The agent serves the tenant's engine policy to the gateway.
func TestPolicyServedToGateway(t *testing.T) {
	f := newFakeEngine(t)
	f.policies["t1"] = wire.PolicyResponse{Inline: true, WantResponse: true, Observe: true}
	_, url := startAgent(t, f, Config{})
	resp, err := http.Get(url + wire.PathPolicy + "?tenant_id=t1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var p wire.PolicyResponse
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil || !p.Inline || !p.Observe {
		t.Fatalf("policy %+v err %v", p, err)
	}
}

// A turn whose request the gateway had judged inline sends only its
// response stage.
func TestTurnRequestJudgedSkipsRequestStage(t *testing.T) {
	f := newFakeEngine(t)
	_, url := startAgent(t, f, Config{})
	tr := fullTurn("summarise this", true)
	tr.RequestJudged = true
	if code := postTurn(t, url, tr); code != http.StatusAccepted {
		t.Fatalf("code %d", code)
	}
	if got := f.next(t); got.req.Turn.Stage != turn.StageResponse {
		t.Fatalf("stage %s; want response only", got.req.Turn.Stage)
	}
	f.none(t)
}

// With observe on, each generation turn reaches the engine as an observation
// with its headers and its bodies redacted; with it off, nothing is sent.
func TestObservation(t *testing.T) {
	f := newFakeEngine(t)
	f.policies["t1"] = wire.PolicyResponse{Observe: true}
	_, url := startAgent(t, f, Config{})
	tr := fullTurn("deploy with "+fakeSecret, true)
	tr.Headers = map[string]string{"user-agent": "claude-cli/2.1.0 (external, cli)"}
	tr.ResponseType = "application/json"
	if code := postTurn(t, url, tr); code != http.StatusAccepted {
		t.Fatalf("code %d", code)
	}
	waitUntil(t, "observation", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.observations) == 1 })
	f.mu.Lock()
	o := f.observations[0]
	f.mu.Unlock()
	if o.Meta.RequestID != "req-1" || o.Headers["user-agent"] == "" || o.ResponseType != "application/json" ||
		bytes.Contains(o.Request, []byte(fakeSecret)) || !bytes.Contains(o.Request, []byte("[REDACTED:")) || len(o.Response) == 0 {
		t.Fatalf("observation = %+v (request %s)", o, o.Request)
	}

	g := newFakeEngine(t)
	_, url2 := startAgent(t, g, Config{})
	if code := postTurn(t, url2, fullTurn("hi", true)); code != http.StatusAccepted {
		t.Fatalf("code %d", code)
	}
	g.next(t)
	g.next(t)
	time.Sleep(100 * time.Millisecond)
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.observations) != 0 {
		t.Fatalf("observed without the policy asking: %d", len(g.observations))
	}
}
