package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/router"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/memory"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// newSkillsHarness is newHarness's (inflight_test.go) twin for the native
// skills/commands surface: no connector is registered at all (neither
// gateway__skill nor a native command ever reaches one), but a real
// profile.Enforcer backs a real dptest.Store, so resolution runs exactly
// as it does in production, not through a mock.
func newSkillsHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	st := dptest.New()
	if err := st.Tenants().Create(ctx, &store.Tenant{ID: testTenant, Slug: "a", Name: "A"}); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sessions := session.NewManager(memory.New(), session.Options{Logger: logger})
	cl := client.New(client.Options{Logger: logger})
	t.Cleanup(func() { _ = cl.Close() })

	profiles := profile.New(st.AgentProfiles(), st.Connectors(), st.Skills(), profile.Options{})

	orch, err := New(Deps{
		Sessions:   sessions,
		Profiles:   profiles,
		Router:     router.New(st.Connectors(), router.Options{}),
		Client:     cl,
		Connectors: st.Connectors(),
		Logger:     logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	return &harness{
		orch: orch, store: st, sessions: sessions,
		principal: &pkgauth.Principal{Subject: "key-1", TenantID: testTenant, Roles: []string{"agent"}, AuthMethod: "apikey"},
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func createTestProfile(t *testing.T, st *dptest.Store, name, instructions string) *store.AgentProfile {
	t.Helper()
	p := &store.AgentProfile{TenantID: testTenant, Name: name, Slug: profile.Slug(testTenant, name), Instructions: instructions}
	if err := st.AgentProfiles().Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func createTestSkill(t *testing.T, st *dptest.Store, tenantID, name, kind, description string, args []store.CommandArgument, extra ...store.SkillFile) *store.Skill {
	t.Helper()
	sk := &store.Skill{
		TenantID: tenantID, Name: name, Kind: kind, Description: description,
		Arguments: args, Enabled: true, Metadata: map[string]any{},
	}
	files := append([]store.SkillFile{{Path: "SKILL.md", Content: "---\nname: " + name + "\ndescription: " + description + "\n---\n" + description}}, extra...)
	if err := st.Skills().Create(context.Background(), sk, files, "test"); err != nil {
		t.Fatal(err)
	}
	return sk
}

func (h *harness) handleWithProfile(req *mcp.Request, profileName string) Result {
	return h.orch.Handle(h.ctx(), Request{JSONRPC: req, Principal: h.principal, ProfileName: profileName})
}

func decodeErr(t *testing.T, r Result) *mcp.Error {
	t.Helper()
	if r.Response == nil {
		t.Fatal("no response")
	}
	return r.Response.Error
}

// ---------------------------------------------------------------------------
// tools/list gating
// ---------------------------------------------------------------------------

func TestToolsList_NativeSkillToolListedOnlyWhenProfileHasSkills(t *testing.T) {
	h := newSkillsHarness(t)
	withSkills := createTestProfile(t, h.store, "with-skills", "")
	createTestSkill(t, h.store, testTenant, "runbook", "skill", "how to triage", nil)
	if err := h.store.AgentProfiles().SetSkills(context.Background(), withSkills.ID, []store.ProfileSkill{
		{AgentProfileID: withSkills.ID, SkillID: mustSkillID(t, h.store, "runbook")},
	}); err != nil {
		t.Fatal(err)
	}
	createTestProfile(t, h.store, "no-skills", "")

	list := func(req *mcp.Request, profileName string) []mcp.Tool {
		res := h.handleWithProfile(req, profileName)
		if res.Response == nil || res.Response.Error != nil {
			t.Fatalf("tools/list failed: %+v", res.Response)
		}
		var out mcp.ToolsListResult
		if err := json.Unmarshal(res.Response.Result, &out); err != nil {
			t.Fatal(err)
		}
		return out.Tools
	}

	hasNative := func(tools []mcp.Tool) bool {
		for _, tl := range tools {
			if tl.Name == nativeSkillToolName {
				return true
			}
		}
		return false
	}

	req := &mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsList}
	if !hasNative(list(req, "with-skills")) {
		t.Error("gateway__skill was not listed for a profile with an attached skill")
	}
	if hasNative(list(req, "no-skills")) {
		t.Error("gateway__skill was listed for a profile with no attached skills")
	}
	if hasNative(list(req, "")) {
		t.Error("gateway__skill was listed with no profile at all")
	}
}

// mustSkillID looks a just-created skill up by name so callers don't have
// to thread the *store.Skill returned by createTestSkill everywhere.
func mustSkillID(t *testing.T, st *dptest.Store, name string) string {
	t.Helper()
	sk, err := st.Skills().GetByName(context.Background(), testTenant, name)
	if err != nil {
		t.Fatal(err)
	}
	return sk.ID
}

// ---------------------------------------------------------------------------
// gateway__skill
// ---------------------------------------------------------------------------

func TestGatewaySkillTool_LoadsAttachedSkillContent(t *testing.T) {
	h := newSkillsHarness(t)
	p := createTestProfile(t, h.store, "Reader", "")
	sk := createTestSkill(t, h.store, testTenant, "runbook", "skill", "how to triage",
		nil, store.SkillFile{Path: "notes.txt", Content: "extra notes"})
	if err := h.store.AgentProfiles().SetSkills(context.Background(), p.ID, []store.ProfileSkill{{AgentProfileID: p.ID, SkillID: sk.ID}}); err != nil {
		t.Fatal(err)
	}

	call := func(args map[string]any) Result {
		return h.handleWithProfile(&mcp.Request{
			JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsCall,
			Params: mustJSON(t, map[string]any{"name": nativeSkillToolName, "arguments": args}),
		}, "Reader")
	}

	res := call(map[string]any{"name": "runbook"})
	if res.Response == nil || res.Response.Error != nil {
		t.Fatalf("default-path call failed: %+v", res.Response)
	}
	var out mcp.ToolsCallResult
	if err := json.Unmarshal(res.Response.Result, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "---\nname: runbook\ndescription: how to triage\n---\nhow to triage" {
		t.Errorf("SKILL.md content = %+v", out.Content)
	}

	res = call(map[string]any{"name": "runbook", "path": "notes.txt"})
	if err := json.Unmarshal(res.Response.Result, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "extra notes" {
		t.Errorf("notes.txt content = %+v", out.Content)
	}
}

func TestGatewaySkillTool_UnknownPathIsInvalidParams(t *testing.T) {
	h := newSkillsHarness(t)
	p := createTestProfile(t, h.store, "Reader", "")
	sk := createTestSkill(t, h.store, testTenant, "runbook", "skill", "d", nil)
	if err := h.store.AgentProfiles().SetSkills(context.Background(), p.ID, []store.ProfileSkill{{AgentProfileID: p.ID, SkillID: sk.ID}}); err != nil {
		t.Fatal(err)
	}

	res := h.handleWithProfile(&mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsCall,
		Params: mustJSON(t, map[string]any{"name": nativeSkillToolName, "arguments": map[string]any{"name": "runbook", "path": "missing.txt"}}),
	}, "Reader")

	if err := decodeErr(t, res); err == nil || err.Code != mcp.ErrorCodeInvalidParams {
		t.Fatalf("error = %+v, want -32602", err)
	}
}

func TestGatewaySkillTool_UnattachedSkillIs32003WithData(t *testing.T) {
	h := newSkillsHarness(t)
	createTestProfile(t, h.store, "Reader", "")
	// A skill exists, but is never attached to "Reader".
	createTestSkill(t, h.store, testTenant, "other", "skill", "d", nil)

	res := h.handleWithProfile(&mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsCall,
		Params: mustJSON(t, map[string]any{"name": nativeSkillToolName, "arguments": map[string]any{"name": "other"}}),
	}, "Reader")

	err := decodeErr(t, res)
	if err == nil || err.Code != mcp.ErrorCodeToolNotAllowed {
		t.Fatalf("error = %+v, want -32003", err)
	}
	var data map[string]any
	if jsonErr := json.Unmarshal(err.Data, &data); jsonErr != nil {
		t.Fatal(jsonErr)
	}
	if data["skill"] != "other" || data["profile"] != "Reader" {
		t.Errorf("error.Data = %+v, want {skill: other, profile: Reader}", data)
	}
}

func TestGatewaySkillTool_NoProfileIs32003(t *testing.T) {
	h := newSkillsHarness(t)
	createTestSkill(t, h.store, testTenant, "runbook", "skill", "d", nil)

	res := h.handleWithProfile(&mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodToolsCall,
		Params: mustJSON(t, map[string]any{"name": nativeSkillToolName, "arguments": map[string]any{"name": "runbook"}}),
	}, "")

	if err := decodeErr(t, res); err == nil || err.Code != mcp.ErrorCodeToolNotAllowed {
		t.Fatalf("error = %+v, want -32003", err)
	}
}

// ---------------------------------------------------------------------------
// native commands (prompts/list, prompts/get)
// ---------------------------------------------------------------------------

func TestNativeCommand_ListedAndRendered(t *testing.T) {
	h := newSkillsHarness(t)
	p := createTestProfile(t, h.store, "Reader", "")
	cmd := createTestSkill(t, h.store, testTenant, "summarize", "command", "Summarize a case",
		[]store.CommandArgument{{Name: "case_id", Required: true}, {Name: "tone"}})
	if err := h.store.AgentProfiles().SetSkills(context.Background(), p.ID, []store.ProfileSkill{{AgentProfileID: p.ID, SkillID: cmd.ID}}); err != nil {
		t.Fatal(err)
	}

	res := h.handleWithProfile(&mcp.Request{JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodPromptsList}, "Reader")
	if res.Response == nil || res.Response.Error != nil {
		t.Fatalf("prompts/list failed: %+v", res.Response)
	}
	var list mcp.PromptsListResult
	if err := json.Unmarshal(res.Response.Result, &list); err != nil {
		t.Fatal(err)
	}
	var found *mcp.Prompt
	for i := range list.Prompts {
		if list.Prompts[i].Name == "summarize" {
			found = &list.Prompts[i]
		}
	}
	if found == nil || len(found.Arguments) != 2 {
		t.Fatalf("prompts/list = %+v, want \"summarize\" with 2 arguments", list.Prompts)
	}

	res = h.handleWithProfile(&mcp.Request{
		JSONRPC: mcp.Version, ID: 2, Method: mcp.MethodPromptsGet,
		Params: mustJSON(t, map[string]any{"name": "summarize", "arguments": map[string]string{"case_id": "CASE-1"}}),
	}, "Reader")
	if res.Response == nil || res.Response.Error != nil {
		t.Fatalf("prompts/get failed: %+v", res.Response)
	}
	var got mcp.PromptsGetResult
	if err := json.Unmarshal(res.Response.Result, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("Messages = %+v", got.Messages)
	}
	var content mcp.Content
	if err := json.Unmarshal(got.Messages[0].Content, &content); err != nil {
		t.Fatal(err)
	}
	if content.Text != "Summarize a case" {
		t.Errorf("rendered text = %q", content.Text)
	}
}

func TestNativeCommand_MissingRequiredArgumentIsInvalidParams(t *testing.T) {
	h := newSkillsHarness(t)
	p := createTestProfile(t, h.store, "Reader", "")
	cmd := createTestSkill(t, h.store, testTenant, "summarize", "command", "Summarize {{case_id}}",
		[]store.CommandArgument{{Name: "case_id", Required: true}})
	if err := h.store.AgentProfiles().SetSkills(context.Background(), p.ID, []store.ProfileSkill{{AgentProfileID: p.ID, SkillID: cmd.ID}}); err != nil {
		t.Fatal(err)
	}

	res := h.handleWithProfile(&mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodPromptsGet,
		Params: mustJSON(t, map[string]any{"name": "summarize"}),
	}, "Reader")

	if err := decodeErr(t, res); err == nil || err.Code != mcp.ErrorCodeInvalidParams {
		t.Fatalf("error = %+v, want -32602", err)
	}
}

func TestNativeCommand_NotAttachedIs32003(t *testing.T) {
	h := newSkillsHarness(t)
	createTestProfile(t, h.store, "Reader", "")

	res := h.handleWithProfile(&mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodPromptsGet,
		Params: mustJSON(t, map[string]any{"name": "summarize"}),
	}, "Reader")

	if err := decodeErr(t, res); err == nil || err.Code != mcp.ErrorCodeToolNotAllowed {
		t.Fatalf("error = %+v, want -32003", err)
	}
}

// ---------------------------------------------------------------------------
// initialize: instructions, skill index, prompts capability
// ---------------------------------------------------------------------------

func TestInitialize_InstructionsCarryProfileTextAndSkillIndex(t *testing.T) {
	h := newSkillsHarness(t)
	p := createTestProfile(t, h.store, "Analyst", "Always cite the alert id.")
	sk := createTestSkill(t, h.store, testTenant, "triage-guide", "skill", "How to triage an alert", nil)
	cmd := createTestSkill(t, h.store, testTenant, "summarize", "command", "Summarize a case", []store.CommandArgument{{Name: "id"}})
	if err := h.store.AgentProfiles().SetSkills(context.Background(), p.ID, []store.ProfileSkill{
		{AgentProfileID: p.ID, SkillID: sk.ID}, {AgentProfileID: p.ID, SkillID: cmd.ID},
	}); err != nil {
		t.Fatal(err)
	}

	res := h.handleWithProfile(&mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
		Params: mustJSON(t, map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}}),
	}, "Analyst")
	if res.Response == nil || res.Response.Error != nil {
		t.Fatalf("initialize failed: %+v", res.Response)
	}
	var out mcp.InitializeResult
	if err := json.Unmarshal(res.Response.Result, &out); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"Always cite the alert id.", instructions, "triage-guide: How to triage an alert"} {
		if !containsSubstring(out.Instructions, want) {
			t.Errorf("instructions = %q, want it to contain %q", out.Instructions, want)
		}
	}
	if containsSubstring(out.Instructions, "summarize:") {
		t.Error("the skill index must not list commands")
	}
	if out.Capabilities.Prompts == nil {
		t.Error("Capabilities.Prompts was not advertised for a profile with an attached command")
	}
}

func TestInitialize_NoProfileHasNoInstructionsAddition(t *testing.T) {
	h := newSkillsHarness(t)

	res := h.handleWithProfile(&mcp.Request{
		JSONRPC: mcp.Version, ID: 1, Method: mcp.MethodInitialize,
		Params: mustJSON(t, map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}}),
	}, "")
	if res.Response == nil || res.Response.Error != nil {
		t.Fatalf("initialize failed: %+v", res.Response)
	}
	var out mcp.InitializeResult
	if err := json.Unmarshal(res.Response.Result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Instructions != instructions {
		t.Errorf("instructions = %q, want exactly the static pointer text", out.Instructions)
	}
	if out.Capabilities.Prompts != nil {
		t.Error("Capabilities.Prompts was advertised with no profile and no connectors")
	}
}

func containsSubstring(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// buildSkillIndex (pure)
// ---------------------------------------------------------------------------

func TestBuildSkillIndex_CapsAtMaxEntries(t *testing.T) {
	list := make([]profile.ResolvedSkill, maxSkillIndexEntries+5)
	for i := range list {
		list[i] = profile.ResolvedSkill{Skill: store.Skill{Name: "skill", Description: "d"}}
	}
	out := buildSkillIndex(list)
	if !containsSubstring(out, "… and 5 more") {
		t.Errorf("buildSkillIndex did not cap at %d entries: %q", maxSkillIndexEntries, out)
	}
}

func TestBuildSkillIndex_CapsAtMaxBytes(t *testing.T) {
	big := make([]profile.ResolvedSkill, 3)
	longDesc := ""
	for i := 0; i < maxSkillIndexBytes; i++ {
		longDesc += "x"
	}
	for i := range big {
		big[i] = profile.ResolvedSkill{Skill: store.Skill{Name: "skill", Description: longDesc}}
	}
	out := buildSkillIndex(big)
	if len(out) > maxSkillIndexBytes+64 { // header + trailer slack
		t.Errorf("buildSkillIndex exceeded the byte cap: %d bytes", len(out))
	}
	if !containsSubstring(out, "more") {
		t.Error("buildSkillIndex did not summarize the entries it dropped")
	}
}
