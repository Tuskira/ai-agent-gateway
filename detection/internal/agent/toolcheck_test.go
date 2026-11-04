package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire"
)

func toolCall() wire.ToolCallCheck {
	return wire.ToolCallCheck{V: 1, ID: "req-9", TenantID: "t1", SessionID: "mcp-1", KeyID: "key-1", Principal: "key-1",
		ClientIP: "198.51.100.7", At: time.Now().UTC(), Profile: "reader", ConnectorID: "c1", Server: "alpha",
		Tool: "deploy", Outcome: wire.OutcomeAllowed, Arguments: `{"key":"` + fakeSecret + `","target":"prod"}`}
}

func postToolCheck(t *testing.T, url string, body []byte) (int, wire.ToolCallVerdict, string) {
	t.Helper()
	resp, err := http.Post(url+wire.PathToolCallCheck, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var v wire.ToolCallVerdict
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("verdict %s: %v", raw, err)
		}
	}
	return resp.StatusCode, v, string(raw)
}

func checkBody(c wire.ToolCallCheck) []byte {
	b, _ := json.Marshal(c)
	return b
}

// A tenant whose policy asks for inline tool-call checks has each call
// sent to the engine with its arguments redacted and its client_ip, and
// gets the engine's verdict back.
func TestToolCheckForwardsRedacted(t *testing.T) {
	f := newFakeEngine(t)
	f.policies["t1"] = wire.PolicyResponse{InlineToolCalls: true}
	f.toolVerdict = wire.ToolCallVerdict{Block: &wire.Block{RuleID: "r1", Title: "Out of scope", Detail: "prod is not in scope"}}
	a, url := startAgent(t, f, Config{})
	code, v, raw := postToolCheck(t, url, checkBody(toolCall()))
	if code != http.StatusOK || v.Block == nil || v.Block.RuleID != "r1" {
		t.Fatalf("status %d verdict %s", code, raw)
	}
	f.mu.Lock()
	got := f.toolChecks
	f.mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("engine got %d checks", len(got))
	}
	c := got[0]
	if strings.Contains(c.Arguments, fakeSecret) || !strings.Contains(c.Arguments, "[REDACTED:") || !strings.Contains(c.Arguments, "prod") {
		t.Errorf("arguments = %s; want the secret redacted", c.Arguments)
	}
	if c.ID != "req-9" || c.ClientIP != "198.51.100.7" || c.Server != "alpha" || c.Tool != "deploy" || c.V != 1 {
		t.Errorf("check = %+v", c)
	}
	if h := a.Health(); h.ToolChecksSent != 1 || h.ToolChecksBlocked != 1 || h.ToolChecksFailedOpen != 0 {
		t.Errorf("health = %+v", h)
	}
}

// With DETECTION_FORWARD_CLIENT_IP off, the check reaches the engine
// without client_ip.
func TestToolCheckOmitsClientIP(t *testing.T) {
	f := newFakeEngine(t)
	f.policies["t1"] = wire.PolicyResponse{InlineToolCalls: true}
	_, url := startAgent(t, f, Config{OmitClientIP: true})
	if code, _, raw := postToolCheck(t, url, checkBody(toolCall())); code != http.StatusOK {
		t.Fatalf("status %d %s", code, raw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.toolChecks) != 1 || f.toolChecks[0].ClientIP != "" {
		t.Fatalf("checks = %+v; want client_ip blanked", f.toolChecks)
	}
}

// A tenant whose policy does not ask is answered at once, with no block
// and no engine call.
func TestToolCheckPolicyOff(t *testing.T) {
	f := newFakeEngine(t)
	f.policies["t1"] = wire.PolicyResponse{Inline: true}
	_, url := startAgent(t, f, Config{})
	code, v, raw := postToolCheck(t, url, checkBody(toolCall()))
	if code != http.StatusOK || v.Block != nil || strings.TrimSpace(raw) != "{}" {
		t.Fatalf("status %d verdict %s", code, raw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.toolChecks) != 0 {
		t.Fatalf("engine was asked: %+v", f.toolChecks)
	}
}

// An engine that fails (5xx, 503 when full, past the deadline, or a
// route an older engine does not have) fails open: 200 with no block, counted.
func TestToolCheckFailsOpen(t *testing.T) {
	for name, set := range map[string]func(*fakeEngine){
		"500":     func(f *fakeEngine) { f.toolStatus = http.StatusInternalServerError },
		"503":     func(f *fakeEngine) { f.toolStatus = http.StatusServiceUnavailable },
		"404":     func(f *fakeEngine) { f.toolStatus = http.StatusNotFound },
		"timeout": func(f *fakeEngine) { f.toolDelay = time.Second },
	} {
		f := newFakeEngine(t)
		f.policies["t1"] = wire.PolicyResponse{InlineToolCalls: true}
		f.toolVerdict = wire.ToolCallVerdict{Block: &wire.Block{RuleID: "r1"}}
		set(f)
		a, url := startAgent(t, f, Config{ToolCheckTimeout: 100 * time.Millisecond})
		code, v, raw := postToolCheck(t, url, checkBody(toolCall()))
		if code != http.StatusOK || v.Block != nil {
			t.Errorf("%s: status %d verdict %s; want 200 with no block", name, code, raw)
		}
		if h := a.Health(); h.ToolChecksFailedOpen != 1 || h.ToolChecksSent != 0 {
			t.Errorf("%s: health %+v", name, h)
		}
	}
}

// A malformed check, or one missing tenant_id, server or tool, is a 400.
func TestToolCheckMalformed(t *testing.T) {
	f := newFakeEngine(t)
	_, url := startAgent(t, f, Config{})
	bad := func(mut func(*wire.ToolCallCheck)) []byte {
		c := toolCall()
		mut(&c)
		return checkBody(c)
	}
	for name, body := range map[string][]byte{
		"not json":     []byte(`{"tenant_id":`),
		"no tenant":    bad(func(c *wire.ToolCallCheck) { c.TenantID = "" }),
		"no server":    bad(func(c *wire.ToolCallCheck) { c.Server = "" }),
		"no tool":      bad(func(c *wire.ToolCallCheck) { c.Tool = "" }),
		"newer v":      bad(func(c *wire.ToolCallCheck) { c.V = 2 }),
		"wrong fields": []byte(`{"tenant_id":1}`),
	} {
		if code, _, raw := postToolCheck(t, url, body); code != http.StatusBadRequest {
			t.Errorf("%s: status %d %s; want 400", name, code, raw)
		}
	}
}

// With no inline slot free, the check is answered at once unchecked.
func TestToolCheckBusy(t *testing.T) {
	f := newFakeEngine(t)
	f.policies["t1"] = wire.PolicyResponse{InlineToolCalls: true}
	a, url := startAgent(t, f, Config{MaxInFlight: 1})
	a.inline <- struct{}{} // the only slot is taken
	defer func() { <-a.inline }()
	code, v, _ := postToolCheck(t, url, checkBody(toolCall()))
	if code != http.StatusOK || v.Block != nil {
		t.Fatalf("status %d block %+v", code, v.Block)
	}
	if h := a.Health(); h.ToolChecksBusy != 1 {
		t.Errorf("health %+v", h)
	}
}

// The policy's inline_tool_calls passes through the agent to the gateway.
func TestPolicyInlineToolCallsPassThrough(t *testing.T) {
	f := newFakeEngine(t)
	f.policies["t1"] = wire.PolicyResponse{InlineToolCalls: true}
	_, url := startAgent(t, f, Config{})
	resp, err := http.Get(url + wire.PathPolicy + "?tenant_id=t1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil || raw["inline_tool_calls"] != true {
		t.Fatalf("policy %v err %v", raw, err)
	}
}
