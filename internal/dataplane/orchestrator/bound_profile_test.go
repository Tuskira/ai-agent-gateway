package orchestrator

import (
	"context"
	"encoding/json"
	"testing"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// A key bound to a profile has that profile enforced: no header serves the
// bound profile, the same profile's header is fine, another profile's header
// is rejected with -32003, and a binding to a deleted profile fails closed.
func TestBoundKeyProfileEnforced(t *testing.T) {
	h := newSkillsHarness(t)
	a := createTestProfile(t, h.store, "alpha", "")
	b := createTestProfile(t, h.store, "beta", "")
	createTestSkill(t, h.store, testTenant, "runbook", "skill", "how to triage", nil)
	if err := h.store.AgentProfiles().SetSkills(context.Background(), a.ID, []store.ProfileSkill{
		{AgentProfileID: a.ID, SkillID: mustSkillID(t, h.store, "runbook")},
	}); err != nil {
		t.Fatal(err)
	}

	bound := *h.principal
	bound.ProfileID = a.ID
	list := func(p *pkgauth.Principal, header string) Result {
		req := &mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsList}
		ctx := pkgauth.WithPrincipal(context.Background(), p)
		return h.orch.Handle(ctx, Request{JSONRPC: req, Principal: p, ProfileName: header})
	}
	hasNative := func(r Result) bool {
		if r.Response == nil || r.Response.Error != nil {
			t.Fatalf("tools/list failed: %+v", r.Response)
		}
		var out mcp.ToolsListResult
		if err := json.Unmarshal(r.Response.Result, &out); err != nil {
			t.Fatal(err)
		}
		for _, tl := range out.Tools {
			if tl.Name == nativeSkillToolName {
				return true
			}
		}
		return false
	}

	if !hasNative(list(&bound, "")) {
		t.Error("bound key without header: profile alpha (has a skill) was not enforced")
	}
	if !hasNative(list(&bound, "Alpha")) {
		t.Error("bound key naming its own profile (different spelling) was refused")
	}
	rej := list(&bound, "beta")
	if rej.Response == nil || rej.Response.Error == nil || rej.Response.Error.Code != mcp.ErrorCodeToolNotAllowed {
		t.Errorf("bound key naming another profile = %+v, want -32003", rej.Response)
	}

	// Unbound key: the header still selects the profile, as before.
	if hasNative(list(h.principal, "beta")) {
		t.Error("unbound key with header beta should see beta (no skills)")
	}
	if !hasNative(list(h.principal, "alpha")) {
		t.Error("unbound key with header alpha should see alpha")
	}
	_ = b

	// Dangling binding (profile soft-deleted) fails closed, never "every tool".
	if err := h.store.AgentProfiles().SoftDelete(context.Background(), testTenant, a.ID); err != nil {
		t.Fatal(err)
	}
	// The bound-profile row is cached for the enforcer TTL; a fresh
	// principal id forces a lookup of a profile that is gone.
	gone := *h.principal
	gone.ProfileID = "00000000-0000-0000-0000-000000000000"
	rej = list(&gone, "")
	if rej.Response == nil || rej.Response.Error == nil || rej.Response.Error.Code != mcp.ErrorCodeToolNotAllowed {
		t.Errorf("dangling binding = %+v, want -32003", rej.Response)
	}
}
