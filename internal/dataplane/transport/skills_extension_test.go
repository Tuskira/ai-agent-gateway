package transport_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/skills"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// attachSkillForExtension creates a valid "skill" kind row and attaches it to the
// fixture's "Reader" profile, end to end through the real store facets
// dataplane.New wires the plane's skillsext.Resolver over -- this is the
// one test file in this package that exercises the MCP Skills Extension
// (SEP-2640) through the actual HTTP plane, rather than the lower-level
// orchestrator harness in internal/dataplane/orchestrator/skills_test.go.
func (f *fixture) attachSkillForExtension(t *testing.T, name string) *store.Skill {
	t.Helper()
	ctx := context.Background()

	skillMD := fmt.Sprintf("---\nname: %s\ndescription: Test skill %s.\n---\n\nBody of %s.\n", name, name, name)
	files, err := skills.ValidateFiles([]store.SkillFile{{Path: "SKILL.md", Content: skillMD}})
	if err != nil {
		t.Fatal(err)
	}
	fm, err := skills.ParseFrontmatter(skillMD, name)
	if err != nil {
		t.Fatal(err)
	}
	sk := &store.Skill{
		TenantID: tenantID, Name: name, Kind: "skill",
		Description: skills.DescriptionFrom(fm), Frontmatter: fm, Enabled: true,
	}
	if err := f.store.Skills().Create(ctx, sk, files, "tester"); err != nil {
		t.Fatal(err)
	}

	prof, err := f.store.AgentProfiles().GetBySlug(ctx, tenantID, profile.Slug(tenantID, "Reader"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.AgentProfiles().SetSkills(ctx, prof.ID, []store.ProfileSkill{{SkillID: sk.ID}}); err != nil {
		t.Fatal(err)
	}
	return sk
}

func TestSkillsExtensionIsWiredThroughThePlane(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.attachSkillForExtension(t, "code-review")
	reader := map[string]string{profile.Header: "Reader"}

	_, decoded := f.call(t, rpc(mcp.MethodInitialize, 1,
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`), reader)
	result := decodeResult[mcp.InitializeResult](t, decoded)
	if result.Capabilities.Extensions == nil || result.Capabilities.Extensions[mcp.ExtensionSkills] == nil {
		t.Fatalf("capabilities = %+v, want the skills extension advertised", result.Capabilities)
	}
	if result.Capabilities.Resources == nil {
		t.Error("resources capability must be advertised alongside the skills extension")
	}

	_, decoded = f.call(t, rpc(mcp.MethodSkillsList, 2, ""), reader)
	list := decodeResult[mcp.SkillsListResult](t, decoded)
	if len(list.Skills) != 1 || list.Skills[0].URI != "skill://code-review/SKILL.md" {
		t.Fatalf("skills/list = %+v", list)
	}

	params, _ := json.Marshal(mcp.ResourcesReadParams{URI: "skill://code-review/SKILL.md"})
	_, decoded = f.call(t, rpc(mcp.MethodResourcesRead, 3, string(params)), reader)
	read := decodeResult[mcp.ResourcesReadResult](t, decoded)
	if len(read.Contents) != 1 || read.Contents[0].Text == nil || *read.Contents[0].Text == "" {
		t.Fatalf("resources/read = %+v", read)
	}
	if read.Contents[0].MimeType != "text/markdown" {
		t.Errorf("mimeType = %q, want text/markdown", read.Contents[0].MimeType)
	}
}

func TestSkillsExtensionIsNotAdvertisedWithoutAnyAttachedSkill(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	reader := map[string]string{profile.Header: "Reader"}

	_, decoded := f.call(t, rpc(mcp.MethodInitialize, 1,
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`), reader)
	result := decodeResult[mcp.InitializeResult](t, decoded)
	if result.Capabilities.Extensions != nil {
		t.Errorf("extensions = %+v, want none: the profile has no attached skills", result.Capabilities.Extensions)
	}

	_, decoded = f.call(t, rpc(mcp.MethodSkillsGet, 2, `{"uri":"skill://code-review/SKILL.md"}`), reader)
	wantCode(t, decoded, mcp.ErrorCodeToolNotAllowed)
}
