package orchestrator

import (
	"context"
	"sync"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Every routed tools/call is reported with its profile outcome; a
// monitored grant runs and carries its arguments; a restricted key is
// refused before routing.
func TestToolCallOutcomes(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var got []ToolCall
	h.orch.deps.OnToolCall = func(_ context.Context, c ToolCall) {
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
	}
	h.orch.deps.Profiles = profile.New(h.store.AgentProfiles(), h.store.Connectors(), nil, profile.Options{})
	mon := createTestProfile(t, h.store, "watcher", "")
	if err := h.store.AgentProfiles().SetTools(context.Background(), testTenant, mon.ID, []store.ProfileTool{
		{AgentProfileID: mon.ID, ConnectorID: h.conn.ID, ToolName: dptest.SlowTool, Permission: store.PermissionMonitor},
	}); err != nil {
		t.Fatal(err)
	}
	createTestProfile(t, h.store, "empty", "")
	sess := h.initialize(t)

	call := func(profileName string, restricted bool) *mcp.Response {
		p := *h.principal
		p.ToolsRestricted = restricted
		in := Request{JSONRPC: slowCall(7, `{"name":"alpha__slow","arguments":{"steps":0}}`), Principal: &p,
			Session: sess, Backends: sess, ProfileName: profileName}
		return h.orch.Handle(h.ctx(), in).Response
	}
	if r := call("watcher", false); r.Error != nil {
		t.Fatalf("monitored call refused: %+v", r.Error)
	}
	if r := call("empty", false); r.Error == nil || r.Error.Code != mcp.ErrorCodeToolNotAllowed {
		t.Fatalf("ungranted call = %+v", r)
	}
	if r := call("watcher", true); r.Error == nil || r.Error.Code != mcp.ErrorCodeToolNotAllowed || r.Error.Message != restrictedMessage {
		t.Fatalf("restricted call = %+v", r)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	if c := got[0]; c.Outcome != ToolCallMonitored || c.Profile != "watcher" || c.Server != "alpha" || c.Tool != dptest.SlowTool ||
		c.Arguments != `{"steps":0}` || c.TenantID != testTenant || c.KeyID != h.principal.KeyID || c.SessionID != sess.ID {
		t.Errorf("monitored = %+v", c)
	}
	if c := got[1]; c.Outcome != ToolCallDenied || c.Arguments != "" {
		t.Errorf("denied = %+v", c)
	}
	if c := got[2]; c.Outcome != ToolCallRestricted || c.Tool != "alpha__slow" {
		t.Errorf("restricted = %+v", c)
	}
}
