package transport_test

import (
	"context"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// attachSkill creates a minimal skill/command row and attaches it,
// unpinned, to the fixture's "Reader" profile.
func (f *fixture) attachSkill(t *testing.T, name, kind, description string, args []store.CommandArgument) {
	t.Helper()
	ctx := context.Background()

	prof, err := f.store.AgentProfiles().GetBySlug(ctx, tenantID, profile.Slug(tenantID, "Reader"))
	if err != nil {
		t.Fatal(err)
	}
	sk := &store.Skill{TenantID: tenantID, Name: name, Kind: kind, Description: description, Arguments: args, Enabled: true, Metadata: map[string]any{}}
	files := []store.SkillFile{{Path: "SKILL.md", Content: "---\nname: " + name + "\ndescription: " + description + "\n---\n" + description}}
	if err := f.store.Skills().Create(ctx, sk, files, "test"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AgentProfiles().SetSkills(ctx, prof.ID, []store.ProfileSkill{{AgentProfileID: prof.ID, SkillID: sk.ID}}); err != nil {
		t.Fatal(err)
	}
}

// TestGatewaySkillToolCallIsCapturedWithSkillName is an end-to-end check,
// over real HTTP against a real dataplane.Plane (no mocks), that a
// gateway__skill tools/call is captured with sink.AccessLog.SkillName
// set to the skill's name -- the one field Phase 2 adds to the access
// log specifically for this path (see docs/profiles.md).
func TestGatewaySkillToolCallIsCapturedWithSkillName(t *testing.T) {
	f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}})
	f.attachSkill(t, "runbook", "skill", "How to triage", nil)
	reader := map[string]string{profile.Header: "Reader"}

	_, decoded := f.call(t, rpc(mcp.MethodToolsCall, 1, `{"name":"gateway__skill","arguments":{"name":"runbook"}}`), reader)
	if decoded.Error != nil {
		t.Fatalf("tools/call gateway__skill failed: %+v", decoded.Error)
	}

	records := f.sink.all()
	if len(records) == 0 {
		t.Fatal("no access log record was written")
	}
	rec := records[len(records)-1]
	if rec.ToolName != "gateway__skill" {
		t.Errorf("ToolName = %q, want gateway__skill", rec.ToolName)
	}
	if rec.SkillName != "runbook" {
		t.Errorf("SkillName = %q, want runbook", rec.SkillName)
	}
	if rec.ConnectorID != "" {
		t.Errorf("ConnectorID = %q, want empty: gateway__skill is never routed to a connector", rec.ConnectorID)
	}
}

// TestNativeCommandPromptsGetIsCapturedWithSkillName is
// TestGatewaySkillToolCallIsCapturedWithSkillName's twin for a native
// command's prompts/get.
func TestNativeCommandPromptsGetIsCapturedWithSkillName(t *testing.T) {
	f := newFixture(t, fixtureOptions{ProfileTools: []string{"echo"}})
	f.attachSkill(t, "summarize", "command", "Summarize a case", []store.CommandArgument{{Name: "id", Required: true}})
	reader := map[string]string{profile.Header: "Reader"}

	_, decoded := f.call(t, rpc(mcp.MethodPromptsGet, 1, `{"name":"summarize","arguments":{"id":"CASE-1"}}`), reader)
	if decoded.Error != nil {
		t.Fatalf("prompts/get summarize failed: %+v", decoded.Error)
	}

	records := f.sink.all()
	if len(records) == 0 {
		t.Fatal("no access log record was written")
	}
	rec := records[len(records)-1]
	if rec.SkillName != "summarize" {
		t.Errorf("SkillName = %q, want summarize", rec.SkillName)
	}
}
