package llmplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// inlineAgent is a detection agent stub for inline prevention: it serves
// the tenant policy and a verdict, and takes turns.
type inlineAgent struct {
	url             string
	policyCalls     atomic.Int32
	verdictCalls    atomic.Int32
	inline          bool
	block           *teeRuleBlock
	slow            time.Duration
	mu              sync.Mutex
	turns           []teeTurn
	lastTurnRequest teeTurnRequest
}

func newInlineAgent(t *testing.T, inline bool, block *teeRuleBlock) *inlineAgent {
	t.Helper()
	a := &inlineAgent{inline: inline, block: block}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/policy":
			a.policyCalls.Add(1)
			_ = json.NewEncoder(w).Encode(teePolicy{Inline: a.inline, WantResponse: true})
		case "POST /v1/turns/request":
			a.verdictCalls.Add(1)
			time.Sleep(a.slow)
			a.mu.Lock()
			_ = json.Unmarshal(b, &a.lastTurnRequest)
			a.mu.Unlock()
			_ = json.NewEncoder(w).Encode(teeVerdict{Block: a.block, WantResponse: true})
		case "POST /v1/turns":
			var tt teeTurn
			_ = json.Unmarshal(b, &tt)
			a.mu.Lock()
			a.turns = append(a.turns, tt)
			a.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a.url = srv.URL
	return a
}

func (a *inlineAgent) waitTurn(t *testing.T) teeTurn {
	t.Helper()
	for range 200 {
		a.mu.Lock()
		if len(a.turns) > 0 {
			tt := a.turns[0]
			a.mu.Unlock()
			return tt
		}
		a.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no turn")
	return teeTurn{}
}

// countingUpstream counts the calls it answers.
func countingUpstream(t *testing.T, n *atomic.Int32) string {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(teeResp))
	}))
	t.Cleanup(up.Close)
	return up.URL
}

// A blocking verdict refuses the call with 400 and the rule's message;
// nothing reaches the upstream, and the refusal is captured.
func TestInline_BlockRefusesBeforeForwarding(t *testing.T) {
	p := 0.97
	agent := newInlineAgent(t, true, &teeRuleBlock{RuleID: "example-rule", OWASP: "LLM01", Title: "Example rule",
		Probability: &p, Threshold: 0.9})
	var upCalls atomic.Int32
	rec := &lastRec{}
	gw := gateway(t, Config{UpstreamBaseURL: countingUpstream(t, &upCalls),
		DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url, Inline: true})}, rec)
	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, teeReq)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	want := "Blocked by gateway rule example-rule (OWASP LLM01: Example rule), probability 0.97 >= 0.90. The request was not sent to the model."
	var env struct {
		Error struct{ Message, Type string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Error.Message != want || env.Error.Type != "invalid_request_error" {
		t.Fatalf("body %s; want %q", body, want)
	}
	if upCalls.Load() != 0 {
		t.Fatalf("upstream called %d times", upCalls.Load())
	}
	if c := rec.wait(t); c.Error != "rule_blocked: example-rule" || c.StatusCode != http.StatusBadRequest {
		t.Fatalf("captured %+v", c)
	}
	agent.mu.Lock()
	tr := agent.lastTurnRequest
	agent.mu.Unlock()
	if string(tr.Request) != teeReq || tr.Path != "/v1/messages" {
		t.Fatalf("turn request %+v", tr)
	}
}

// A verdict that does not block forwards the call; the turn then carries
// request_judged so the agent does not judge the request again.
func TestInline_AllowForwardsAndMarksJudged(t *testing.T) {
	agent := newInlineAgent(t, true, nil)
	var upCalls atomic.Int32
	gw := gateway(t, Config{UpstreamBaseURL: countingUpstream(t, &upCalls),
		DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url, Inline: true})}, &lastRec{})
	resp, body := do(t, http.MethodPost, gw.URL+"/v1/messages",
		map[string]string{"x-api-key": "sk", "User-Agent": "claude-cli/2.1.0 (external, cli)"}, teeReq)
	if resp.StatusCode != http.StatusOK || body != teeResp || upCalls.Load() != 1 {
		t.Fatalf("status %d upstream %d", resp.StatusCode, upCalls.Load())
	}
	tt := agent.waitTurn(t)
	if !tt.RequestJudged || tt.ResponseType != "application/json" || tt.Headers["user-agent"] != "claude-cli/2.1.0 (external, cli)" {
		t.Fatalf("turn = judged %v type %q headers %v", tt.RequestJudged, tt.ResponseType, tt.Headers)
	}
	if _, ok := tt.Headers["x-api-key"]; ok {
		t.Fatal("credential header sent")
	}
}

// A tenant whose policy is not inline is never held: no verdict asked,
// the policy cached.
func TestInline_TenantNotInline(t *testing.T) {
	agent := newInlineAgent(t, false, &teeRuleBlock{RuleID: "x"})
	var upCalls atomic.Int32
	gw := gateway(t, Config{UpstreamBaseURL: countingUpstream(t, &upCalls),
		DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url, Inline: true})}, &lastRec{})
	for range 3 {
		if resp, _ := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, teeReq); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	if agent.verdictCalls.Load() != 0 || agent.policyCalls.Load() != 1 {
		t.Fatalf("verdicts %d policies %d", agent.verdictCalls.Load(), agent.policyCalls.Load())
	}
	if tt := agent.waitTurn(t); tt.RequestJudged {
		t.Fatal("request_judged set without an inline judgment")
	}
}

// Inline off: nothing on the request path talks to the agent.
func TestInline_OffMakesNoRoundTrip(t *testing.T) {
	agent := newInlineAgent(t, true, &teeRuleBlock{RuleID: "x"})
	gw := gateway(t, Config{UpstreamBaseURL: teeUpstream(t, http.StatusOK, teeResp),
		DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: agent.url})}, &lastRec{})
	if resp, _ := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, teeReq); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	agent.waitTurn(t)
	if agent.policyCalls.Load() != 0 || agent.verdictCalls.Load() != 0 {
		t.Fatalf("policies %d verdicts %d", agent.policyCalls.Load(), agent.verdictCalls.Load())
	}
}

// Fail open: an agent that is down, or slower than the inline timeout,
// lets the call through.
func TestInline_FailsOpen(t *testing.T) {
	t.Run("down", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		url := dead.URL
		dead.Close()
		gw := gateway(t, Config{UpstreamBaseURL: teeUpstream(t, http.StatusOK, teeResp),
			DetectionTee: newTee(t, DetectionTeeConfig{AgentURL: url, Inline: true, InlineTimeout: time.Second})}, &lastRec{})
		if resp, _ := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, teeReq); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("slow", func(t *testing.T) {
		agent := newInlineAgent(t, true, &teeRuleBlock{RuleID: "x"})
		agent.slow = 500 * time.Millisecond
		tee := newTee(t, DetectionTeeConfig{AgentURL: agent.url, Inline: true, InlineTimeout: 100 * time.Millisecond})
		gw := gateway(t, Config{UpstreamBaseURL: teeUpstream(t, http.StatusOK, teeResp), DetectionTee: tee}, &lastRec{})
		if resp, _ := do(t, http.MethodPost, gw.URL+"/v1/messages", map[string]string{"x-api-key": "sk"}, teeReq); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if tee.inlineFailed.Load() == 0 {
			t.Fatal("inline failure not counted")
		}
	})
}
