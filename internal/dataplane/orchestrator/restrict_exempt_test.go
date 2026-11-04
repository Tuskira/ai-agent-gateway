package orchestrator

import (
	"context"
	"sync"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// A restricted key may still call the tools of a connector marked
// restrict_exempt: the call takes the normal path (forwarded, reported with
// its profile outcome). A connector that is not exempt, or exempt but
// disabled, is refused as before.
func TestRestrictedKey_ExemptConnector(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var mu sync.Mutex
	var got []ToolCall
	h.orch.deps.OnToolCall = func(_ context.Context, c ToolCall) {
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
	}
	sess := h.initialize(t)
	p := *h.principal
	p.ToolsRestricted = true
	call := func() *mcp.Response {
		in := Request{JSONRPC: slowCall(11, `{"name":"alpha__slow","arguments":{"steps":0}}`), Principal: &p, Session: sess, Backends: sess}
		return h.orch.Handle(h.ctx(), in).Response
	}
	setMeta := func(meta map[string]any) {
		h.conn.Metadata = meta
		if err := h.store.Connectors().Update(ctx, h.conn); err != nil {
			t.Fatal(err)
		}
	}

	// Not exempt: refused, nothing forwarded.
	before := h.backend.Calls(mcp.MethodToolsCall)
	if r := call(); r.Error == nil || r.Error.Code != mcp.ErrorCodeToolNotAllowed || r.Error.Message != restrictedMessage {
		t.Fatalf("not exempt: %+v", r)
	}
	// Exempt: forwarded.
	setMeta(map[string]any{"restrict_exempt": true})
	if r := call(); r.Error != nil {
		t.Fatalf("exempt: refused %+v", r.Error)
	}
	if n := h.backend.Calls(mcp.MethodToolsCall) - before; n != 1 {
		t.Fatalf("backend tools/call count = %d; want 1 (the exempt call only)", n)
	}
	// Exempt but disabled: refused.
	setMeta(map[string]any{"restrict_exempt": true, "enabled": false})
	if r := call(); r.Error == nil || r.Error.Code != mcp.ErrorCodeToolNotAllowed || r.Error.Message != restrictedMessage {
		t.Fatalf("exempt but disabled: %+v", r)
	}
	// A non-bool value is not exempt.
	setMeta(map[string]any{"restrict_exempt": "true"})
	if r := call(); r.Error == nil || r.Error.Message != restrictedMessage {
		t.Fatalf("string value: %+v", r)
	}
	if n := h.backend.Calls(mcp.MethodToolsCall) - before; n != 1 {
		t.Errorf("backend tools/call count = %d; want 1", n)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{ToolCallRestricted, ToolCallAllowed, ToolCallRestricted, ToolCallRestricted}
	if len(got) != len(want) {
		t.Fatalf("events = %+v; want outcomes %v", got, want)
	}
	for i, w := range want {
		if got[i].Outcome != w || got[i].Server != "alpha" || got[i].Tool != "slow" {
			t.Errorf("event %d = %+v; want outcome %s", i, got[i], w)
		}
	}
}
