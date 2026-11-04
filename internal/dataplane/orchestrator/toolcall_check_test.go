package orchestrator

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/reqctx"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/trace"
)

// checkHarness is a harness whose CheckToolCall answers verdict and records
// what it was asked, and whose OnToolCall records the events.
type checkHarness struct {
	*harness
	mu      sync.Mutex
	asked   []ToolCall
	events  []ToolCall
	block   *ToolCallBlock
	checked bool
}

func newCheckHarness(t *testing.T) *checkHarness {
	h := &checkHarness{harness: newHarness(t), checked: true}
	h.orch.deps.CheckToolCall = func(_ context.Context, c ToolCall) (*ToolCallBlock, bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.asked = append(h.asked, c)
		return h.block, h.checked
	}
	h.orch.deps.OnToolCall = func(_ context.Context, c ToolCall) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, c)
	}
	return h
}

func (h *checkHarness) call(t *testing.T, name, args, profileName string, restricted bool) *mcp.Response {
	t.Helper()
	sess := h.initialize(t)
	p := *h.principal
	p.ToolsRestricted = restricted
	ctx := reqctx.With(h.ctx(), reqctx.New("req-42", "", trace.Context{}, "198.51.100.7", "", time.Now()))
	in := Request{JSONRPC: slowCall(5, `{"name":"`+name+`","arguments":`+args+`}`), Principal: &p, Session: sess,
		Backends: sess, ProfileName: profileName}
	return h.orch.Handle(ctx, in).Response
}

// A blocking verdict refuses the call with -32003 and the rule's message;
// the backend is never called, and the event says blocked and checked,
// under the request id.
func TestToolCallCheck_Block(t *testing.T) {
	h := newCheckHarness(t)
	h.block = &ToolCallBlock{RuleID: "r1", Title: "Out of scope", Detail: "prod is not in scope"}
	before := h.backend.Calls(mcp.MethodToolsCall)
	r := h.call(t, "alpha__slow", `{"steps":0,"target":"prod"}`, "", false)
	want := "Blocked by detection rule r1 (Out of scope): prod is not in scope. The tool call was not sent to the connector."
	if r.Error == nil || r.Error.Code != mcp.ErrorCodeToolNotAllowed || r.Error.Message != want {
		t.Fatalf("response = %+v; want -32003 %q", r, want)
	}
	var data map[string]any
	_ = json.Unmarshal(r.Error.Data, &data)
	if data["tool"] != "alpha__slow" || data["rule_id"] != "r1" || data["profile"] != "" {
		t.Errorf("error data = %v", r.Error.Data)
	}
	if n := h.backend.Calls(mcp.MethodToolsCall) - before; n != 0 {
		t.Fatalf("backend called %d times", n)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.asked) != 1 || h.asked[0].ID != "req-42" || h.asked[0].Arguments != `{"steps":0,"target":"prod"}` ||
		h.asked[0].ClientIP != "198.51.100.7" || h.asked[0].Server != "alpha" || h.asked[0].Tool != "slow" || h.asked[0].Outcome != ToolCallAllowed {
		t.Errorf("asked = %+v", h.asked)
	}
	if len(h.events) != 1 || h.events[0].Outcome != ToolCallBlocked || !h.events[0].Checked || h.events[0].ID != "req-42" ||
		h.events[0].Arguments != "" {
		t.Errorf("events = %+v; want one blocked, checked, id req-42, no arguments (not monitored)", h.events)
	}
}

// No block: the call is forwarded; checked follows the checker's answer
// (false when it failed open).
func TestToolCallCheck_AllowAndFailOpen(t *testing.T) {
	for _, checked := range []bool{true, false} {
		h := newCheckHarness(t)
		h.checked = checked
		before := h.backend.Calls(mcp.MethodToolsCall)
		if r := h.call(t, "alpha__slow", `{"steps":0}`, "", false); r.Error != nil {
			t.Fatalf("checked %v: refused %+v", checked, r.Error)
		}
		if n := h.backend.Calls(mcp.MethodToolsCall) - before; n != 1 {
			t.Fatalf("checked %v: backend called %d times", checked, n)
		}
		h.mu.Lock()
		if len(h.events) != 1 || h.events[0].Outcome != ToolCallAllowed || h.events[0].Checked != checked || h.events[0].ID != "req-42" {
			t.Errorf("checked %v: events = %+v", checked, h.events)
		}
		h.mu.Unlock()
	}
}

// Only a granted call is checked: a call the profile denies, a restricted
// key's refused call and the native gateway__skill tool are not; a
// monitored call is, with outcome monitored and the profile it named.
func TestToolCallCheck_OnlyGranted(t *testing.T) {
	h := newCheckHarness(t)
	h.orch.deps.Profiles = profile.New(h.store.AgentProfiles(), h.store.Connectors(), nil, profile.Options{})
	mon := createTestProfile(t, h.store, "watcher", "")
	if err := h.store.AgentProfiles().SetTools(context.Background(), testTenant, mon.ID, []store.ProfileTool{
		{AgentProfileID: mon.ID, ConnectorID: h.conn.ID, ToolName: dptest.SlowTool, Permission: store.PermissionMonitor},
	}); err != nil {
		t.Fatal(err)
	}
	createTestProfile(t, h.store, "empty", "")

	if r := h.call(t, "alpha__slow", `{"steps":0}`, "empty", false); r.Error == nil {
		t.Fatal("denied call went through")
	}
	if r := h.call(t, "alpha__slow", `{"steps":0}`, "", true); r.Error == nil || r.Error.Message != restrictedMessage {
		t.Fatalf("restricted call = %+v", r)
	}
	_ = h.call(t, "gateway__skill", `{"name":"x"}`, "", false) // native: answered by the gateway, whatever it says
	h.mu.Lock()
	if len(h.asked) != 0 {
		t.Fatalf("checked a call that is not granted: %+v", h.asked)
	}
	h.mu.Unlock()

	if r := h.call(t, "alpha__slow", `{"steps":0}`, "watcher", false); r.Error != nil {
		t.Fatalf("monitored call refused: %+v", r.Error)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.asked) != 1 || h.asked[0].Outcome != ToolCallMonitored || h.asked[0].Profile != "watcher" {
		t.Fatalf("asked = %+v; want the monitored call", h.asked)
	}
}

// A restricted key's call to a restrict_exempt connector is granted, so it
// is checked like any other.
func TestToolCallCheck_ExemptConnectorChecked(t *testing.T) {
	h := newCheckHarness(t)
	h.conn.Metadata = map[string]any{"restrict_exempt": true}
	if err := h.store.Connectors().Update(context.Background(), h.conn); err != nil {
		t.Fatal(err)
	}
	h.block = &ToolCallBlock{RuleID: "r2", Title: "No"}
	r := h.call(t, "alpha__slow", `{"steps":0}`, "", true)
	if r.Error == nil || r.Error.Message != "Blocked by detection rule r2 (No). The tool call was not sent to the connector." {
		t.Fatalf("response = %+v", r)
	}
}
