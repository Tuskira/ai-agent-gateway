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
	if c := got[2]; c.Outcome != ToolCallRestricted || c.Server != "alpha" || c.Tool != "slow" || c.ConnectorID != h.conn.ID {
		t.Errorf("restricted = %+v", c)
	}
}

// A restricted key's refused call is reported with a server and an
// unprefixed tool, as a routed call is: the connector's slug and id when
// the namespace names one (by name, here distinct from the slug, or when
// unhealthy), the namespace itself when it names none, and nothing for a
// name with no namespace -- an event with no server would be refused by
// the receiver, with the rest of its batch.
func TestRestrictedToolCallEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ti := &store.Connector{TenantID: testTenant, Name: "threat_intel", Slug: "threat-intel",
		Endpoint: h.conn.Endpoint, TimeoutMS: 5000, Status: "unhealthy", Metadata: map[string]any{}}
	if err := h.store.Connectors().Create(ctx, ti); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []ToolCall
	h.orch.deps.OnToolCall = func(_ context.Context, c ToolCall) {
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
	}
	sess := h.initialize(t)
	// Unhealthy past its cool-down (flipped after initialize, which
	// would otherwise heal it): its next routed call is the probe.
	ti.Status = "unhealthy"
	if err := h.store.Connectors().Update(ctx, ti); err != nil {
		t.Fatal(err)
	}
	p := *h.principal
	p.ToolsRestricted = true

	for _, name := range []string{"alpha__slow", "threat_intel__lookup_cve", "nowhere__lookup", "gateway__skill", "bare"} {
		in := Request{JSONRPC: slowCall(9, `{"name":"`+name+`","arguments":{}}`), Principal: &p, Session: sess, Backends: sess}
		r := h.orch.Handle(h.ctx(), in).Response
		if r.Error == nil || r.Error.Code != mcp.ErrorCodeToolNotAllowed || r.Error.Message != restrictedMessage {
			t.Fatalf("%s: restricted call = %+v", name, r)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	want := []ToolCall{
		{Server: "alpha", ConnectorID: h.conn.ID, Tool: "slow"},
		{Server: "threat-intel", ConnectorID: ti.ID, Tool: "lookup_cve"},
		{Server: "nowhere", Tool: "lookup"},
		{Server: "gateway", Tool: "skill"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		c := got[i]
		if c.Outcome != ToolCallRestricted || c.Server != w.Server || c.ConnectorID != w.ConnectorID || c.Tool != w.Tool ||
			c.TenantID != testTenant || c.SessionID != sess.ID {
			t.Errorf("event %d = %+v, want server %q connector %q tool %q", i, c, w.Server, w.ConnectorID, w.Tool)
		}
	}
	// The refusal spent no half-open probe of the unhealthy connector.
	if _, probe, err := h.orch.deps.Router.ResolveConnector(ctx, testTenant, "threat_intel"); err != nil || !probe {
		t.Errorf("unhealthy connector's probe after a restricted call: probe %v, err %v", probe, err)
	}
}
