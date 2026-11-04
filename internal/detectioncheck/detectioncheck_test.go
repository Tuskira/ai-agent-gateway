package detectioncheck

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// agent is a stand-in detection agent: policy answers GET /v1/policy and
// check answers POST /v1/tool-calls/check.
type agent struct {
	url      string
	policies atomic.Int32
	checks   atomic.Int32
	mu       sync.Mutex
	last     ToolCallCheck
	lastRaw  map[string]any
	auth     string
}

func newAgent(t *testing.T, policy func(w http.ResponseWriter), check func(w http.ResponseWriter)) *agent {
	t.Helper()
	a := &agent{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.auth = r.Header.Get("Authorization")
		a.mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/policy":
			a.policies.Add(1)
			policy(w)
		case "POST /v1/tool-calls/check":
			a.checks.Add(1)
			b, _ := io.ReadAll(r.Body)
			a.mu.Lock()
			_ = json.Unmarshal(b, &a.last)
			_ = json.Unmarshal(b, &a.lastRaw)
			a.mu.Unlock()
			check(w)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a.url = srv.URL
	return a
}

func policyOn(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"inline":false,"inline_tool_calls":true}`))
}
func policyOff(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"inline":true,"want_response":true}`)) }
func allow(w http.ResponseWriter)     { _, _ = w.Write([]byte(`{}`)) }
func blockRule(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"block":{"rule_id":"r1","owasp":"LLM06","title":"Out of scope","threshold":0,"detail":"prod is not in scope"}}`))
}

func call() ToolCallCheck {
	return ToolCallCheck{ID: "req-1", TenantID: "t1", Server: "alpha", Tool: "deploy", ConnectorID: "c1",
		Outcome: OutcomeAllowed, Arguments: `{"target":"prod"}`, ClientIP: "198.51.100.7"}
}

// A blocking verdict comes back checked; the check carries v, at, the
// token and every field.
func TestCheckBlock(t *testing.T) {
	a := newAgent(t, policyOn, blockRule)
	c := New(Config{AgentURL: a.url + "/", AgentToken: "tok"})
	b, checked := c.Check(context.Background(), call())
	if !checked || b == nil || b.RuleID != "r1" || b.Title != "Out of scope" || b.Detail != "prod is not in scope" {
		t.Fatalf("block %+v checked %v", b, checked)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last.V != 1 || a.last.At.IsZero() || a.last.ID != "req-1" || a.last.ClientIP != "198.51.100.7" ||
		a.last.Arguments != `{"target":"prod"}` || a.auth != "Bearer tok" {
		t.Errorf("check sent = %+v (auth %q)", a.last, a.auth)
	}
	if st := c.Status(); st["checked"] != uint64(1) || st["blocked"] != uint64(1) || st["failed"] != uint64(0) {
		t.Errorf("status %v", st)
	}
}

// A verdict with no block is checked and lets the call through.
func TestCheckAllow(t *testing.T) {
	a := newAgent(t, policyOn, allow)
	c := New(Config{AgentURL: a.url})
	if b, checked := c.Check(context.Background(), call()); b != nil || !checked {
		t.Fatalf("block %+v checked %v", b, checked)
	}
}

// A tenant whose policy does not ask is not checked: no POST; the policy is
// cached (one lookup for several calls).
func TestCheckPolicyOff(t *testing.T) {
	a := newAgent(t, policyOff, blockRule)
	c := New(Config{AgentURL: a.url})
	for range 3 {
		if b, checked := c.Check(context.Background(), call()); b != nil || checked {
			t.Fatalf("block %+v checked %v", b, checked)
		}
	}
	if a.checks.Load() != 0 || a.policies.Load() != 1 {
		t.Fatalf("checks %d policies %d; want 0 and 1", a.checks.Load(), a.policies.Load())
	}
	if st := c.Status(); st["failed"] != uint64(0) {
		t.Errorf("status %v", st)
	}
}

// Every failure fails open, counted: an agent down, a timeout, a non-200,
// an answer that is not JSON, one over 1 MiB, and an older agent that has
// no such route.
func TestCheckFailsOpen(t *testing.T) {
	for name, check := range map[string]func(w http.ResponseWriter){
		"500":      func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) },
		"404":      func(w http.ResponseWriter) { http.NotFound(w, nil) },
		"bad json": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"block":`)) },
		"too big": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"block":{"rule_id":"` + strings.Repeat("x", 1<<20) + `"}}`))
		},
		"timeout": func(w http.ResponseWriter) { time.Sleep(300 * time.Millisecond); blockRule(w) },
	} {
		a := newAgent(t, policyOn, check)
		c := New(Config{AgentURL: a.url, Timeout: 100 * time.Millisecond})
		if b, checked := c.Check(context.Background(), call()); b != nil || checked {
			t.Errorf("%s: block %+v checked %v; want fail open", name, b, checked)
		}
		if st := c.Status(); st["failed"] != uint64(1) || st["checked"] != uint64(0) {
			t.Errorf("%s: status %v", name, st)
		}
	}

	// Agent down: the policy lookup fails, is cached as off, and counted.
	c := New(Config{AgentURL: "http://127.0.0.1:1", Timeout: 200 * time.Millisecond})
	for range 2 {
		if b, checked := c.Check(context.Background(), call()); b != nil || checked {
			t.Fatalf("agent down: block %+v checked %v", b, checked)
		}
	}
	if st := c.Status(); st["failed"] != uint64(1) {
		t.Errorf("agent down: status %v; want one failure (the lookup is cached)", st)
	}
}

// A failed policy lookup is cached for 5s as off, a good one for 15s.
func TestPolicyCache(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	a := newAgent(t, func(w http.ResponseWriter) {
		if fail.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		policyOn(w)
	}, allow)
	c := New(Config{AgentURL: a.url})
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	if _, checked := c.Check(context.Background(), call()); checked {
		t.Fatal("checked with the policy down")
	}
	fail.Store(false)
	now = now.Add(4 * time.Second)
	if _, checked := c.Check(context.Background(), call()); checked {
		t.Fatal("failed lookup not cached")
	}
	now = now.Add(2 * time.Second)
	if _, checked := c.Check(context.Background(), call()); !checked {
		t.Fatal("not checked after the failure TTL")
	}
	if a.policies.Load() != 2 {
		t.Errorf("policy lookups %d; want 2", a.policies.Load())
	}
}

// A nil Checker checks nothing.
func TestNilChecker(t *testing.T) {
	var c *Checker
	if b, checked := c.Check(context.Background(), call()); b != nil || checked {
		t.Fatal("nil checker checked")
	}
	if st := c.Status(); st["enabled"] != false {
		t.Errorf("status %v", st)
	}
}

// Arguments over 64 KiB are cut on a UTF-8 boundary.
func TestCutArguments(t *testing.T) {
	if s, cut := CutArguments(`{"a":1}`); cut || s != `{"a":1}` {
		t.Errorf("short: %q %v", s, cut)
	}
	long := `{"a":"` + strings.Repeat("é", MaxArguments) + `"}`
	s, cut := CutArguments(long)
	if !cut || len(s) > MaxArguments || len(s) < MaxArguments-3 || !utf8.ValidString(s) {
		t.Errorf("long: len %d cut %v valid %v", len(s), cut, utf8.ValidString(s))
	}
}

// The check is pinned by testdata/tool_call_check.json, whose
// byte-identical copy the detection agent's wire tests read: a check built
// from fixed inputs marshals to it, and it decodes back to the same check.
func TestContractFixture(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "tool_call_check.json"))
	if err != nil {
		t.Fatal(err)
	}
	built := ToolCallCheck{V: 1, ID: "req_0123456789abcdef", TenantID: "tenant-1", SessionID: "mcp-session-1",
		KeyID: "key-1", Principal: "user-1", ClientIP: "192.0.2.10", At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Profile: "reader", ConnectorID: "connector-1", Server: "alpha", Tool: "lookup", Outcome: OutcomeMonitored,
		Arguments: `{"host":"db-1","limit":5}`, ArgumentsTruncated: true}
	gb, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	var g, w map[string]any
	if err := json.Unmarshal(gb, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("marshaled = %s\nwant %s", gb, want)
	}
	var back ToolCallCheck
	if err := json.Unmarshal(want, &back); err != nil || !reflect.DeepEqual(back, built) {
		t.Errorf("fixture decodes to %+v (%v)", back, err)
	}
}
