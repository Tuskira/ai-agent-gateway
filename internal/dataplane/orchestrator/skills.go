package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/reqctx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/skills"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// This file is the gateway-native half of "skills & commands on agent
// profiles" (Phase 2 of the feature; the registry itself and profile
// attachment live in internal/api/handlers.Skills/Profiles and
// internal/dataplane/profile): the initialize instructions a profile's
// attached skills produce, the native gateway__skill tool that loads
// one, and native commands served as prompts/list and prompts/get
// entries alongside connectors' own.
//
// Nothing here is forwarded to a connector -- gateway__skill and a
// native command name (one with no "__") are both intercepted before
// routing, in orchestrator.go/prompts.go, and answered entirely from the
// resolved profile.AllowList the Enforcer already loaded.

const (
	// nativeSkillToolName is the one native tool the gateway itself
	// serves. "gateway" is a connector name/slug the control-plane API
	// refuses to register (internal/api/handlers.Connectors), precisely
	// so "gateway__skill" can never collide with a real connector's tool.
	nativeSkillToolName = "gateway__skill"

	// maxSkillIndexEntries and maxSkillIndexBytes cap the skill index
	// initialize's instructions carry: enough for an agent to know what
	// it can load, not a full catalog dump for a profile with hundreds of
	// skills attached.
	maxSkillIndexEntries = 50
	maxSkillIndexBytes   = 8 * 1024
)

// nativeSkillTool describes gateway__skill for tools/list.
func nativeSkillTool() mcp.Tool {
	return mcp.Tool{
		Name:        nativeSkillToolName,
		Description: "Load the content of a skill attached to this profile. See the server's instructions for the list of skills and their descriptions.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "The skill's name, exactly as listed in the server's instructions.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "A file within the skill. Defaults to SKILL.md.",
				},
			},
			Required: []string{"name"},
		},
	}
}

// handleGatewaySkillTool answers a tools/call for gateway__skill: it is
// never routed to a connector. allow is the caller's already-resolved
// profile (nil when none applies).
func (o *Orchestrator) handleGatewaySkillTool(ctx context.Context, in Request, allow *profile.AllowList, params mcp.ToolsCallParams) Result {
	name, _ := params.Arguments["name"].(string)
	if name == "" {
		return o.fail(in, mcp.NewInvalidParamsError(`"name" is required`))
	}
	path, _ := params.Arguments["path"].(string)
	if path == "" {
		path = skills.SkillMDPath
	}

	resolved, found := findAttachedSkill(allow, name)
	if !found {
		return o.fail(in, mcp.NewError(mcp.ErrorCodeToolNotAllowed,
			"skill not attached to this profile: "+name, map[string]any{
				"skill":   name,
				"profile": profileNameOf(allow),
			}))
	}

	content, ok := findSkillFile(resolved.Version, path)
	if !ok {
		return o.fail(in, mcp.NewInvalidParamsError(fmt.Sprintf("skill %q has no file %q", name, path)))
	}

	reqctx.From(ctx).SetSkill(name)
	return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, mcp.ToolsCallResult{
		Content: []mcp.Content{mcp.TextContent(content)},
	})}
}

// findAttachedSkill looks up name among allow's attached skills. A nil
// or not-Found allow (no profile in play, or a named profile that
// doesn't exist) attaches nothing.
func findAttachedSkill(allow *profile.AllowList, name string) (profile.ResolvedSkill, bool) {
	if allow == nil || !allow.Found {
		return profile.ResolvedSkill{}, false
	}
	return allow.FindSkill(name)
}

// findAttachedCommand is findAttachedSkill's twin for the kind="command"
// side (prompts).
func findAttachedCommand(allow *profile.AllowList, name string) (profile.ResolvedSkill, bool) {
	if allow == nil || !allow.Found {
		return profile.ResolvedSkill{}, false
	}
	return allow.FindCommand(name)
}

// profileNameOf renders allow.Name for a denial's "profile" field, ""
// when there was no profile to name at all.
func profileNameOf(allow *profile.AllowList) string {
	if allow == nil {
		return ""
	}
	return allow.Name
}

// findSkillFile returns the content of path within v's files.
func findSkillFile(v *store.SkillVersion, path string) (string, bool) {
	if v == nil {
		return "", false
	}
	for _, f := range v.Files {
		if f.Path == path {
			return f.Content, true
		}
	}
	return "", false
}

// nativeCommandPrompts renders commands (allow.Commands) as the mcp.Prompt
// entries prompts/list merges alongside connectors' own.
func nativeCommandPrompts(commands []profile.ResolvedSkill) []mcp.Prompt {
	out := make([]mcp.Prompt, 0, len(commands))
	for _, rs := range commands {
		args := make([]mcp.PromptArgument, 0, len(rs.Skill.Arguments))
		for _, a := range rs.Skill.Arguments {
			args = append(args, mcp.PromptArgument{Name: a.Name, Description: a.Description, Required: a.Required})
		}
		out = append(out, mcp.Prompt{Name: rs.Skill.Name, Description: rs.Skill.Description, Arguments: args})
	}
	return out
}

// handleNativePromptGet answers a prompts/get whose name carries no
// "__" -- a native command, never a connector's. allow is resolved here
// (routeCatalog/fanOutCatalog aren't used: there is no connector to
// route to).
func (o *Orchestrator) handleNativePromptGet(ctx context.Context, in Request, params mcp.PromptsGetParams) Result {
	allow, mcpErr := o.resolveProfile(ctx, in)
	if mcpErr != nil {
		return o.fail(in, mcpErr)
	}

	resolved, found := findAttachedCommand(allow, params.Name)
	if !found {
		return o.fail(in, mcp.NewError(mcp.ErrorCodeToolNotAllowed,
			"command not attached to this profile: "+params.Name, map[string]any{
				"skill":   params.Name,
				"profile": profileNameOf(allow),
			}))
	}

	text, err := renderCommand(resolved, params.Arguments)
	if err != nil {
		return o.fail(in, mcp.NewInvalidParamsError(err.Error()))
	}

	contentJSON, err := json.Marshal(mcp.TextContent(text))
	if err != nil {
		return o.fail(in, mcp.NewInternalError(err.Error()))
	}

	reqctx.From(ctx).SetSkill(params.Name)
	o.log.Debug("native command rendered", "command", params.Name, "profile", profileNameOf(allow))

	return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, mcp.PromptsGetResult{
		Description: resolved.Skill.Description,
		Messages:    []mcp.PromptMessage{{Role: "user", Content: contentJSON}},
	})}
}

// renderCommand renders rs's SKILL.md body (after its frontmatter) with
// args, after checking every declared required argument was supplied.
func renderCommand(rs profile.ResolvedSkill, args map[string]string) (string, error) {
	if rs.Version == nil {
		return "", fmt.Errorf("command %q has no content", rs.Skill.Name)
	}
	content, ok := skills.FindFile(rs.Version.Files, skills.SkillMDPath)
	if !ok {
		return "", fmt.Errorf("command %q is missing %s", rs.Skill.Name, skills.SkillMDPath)
	}
	_, body, err := skills.SplitFrontmatter(content)
	if err != nil {
		return "", err
	}

	for _, a := range rs.Skill.Arguments {
		if a.Required && strings.TrimSpace(args[a.Name]) == "" {
			return "", fmt.Errorf("missing required argument %q", a.Name)
		}
	}

	return skills.RenderTemplate(body, args), nil
}

// resolveProfileForInitialize resolves the caller's profile purely to
// shape initialize's instructions text and prompts capability -- never
// to enforce anything (that stays on tools/list, tools/call, prompts/*
// and resources/*, unchanged). A missing profile name, a nil Profiles
// enforcer, or a resolution failure are all treated as "no profile":
// initialize must not fail because of them, matching its existing
// behaviour of not gating on mcp.require_profile.
func (o *Orchestrator) resolveProfileForInitialize(ctx context.Context, in Request) *profile.AllowList {
	if in.ProfileName == "" || o.deps.Profiles == nil {
		return nil
	}
	allow, err := o.deps.Profiles.Resolve(ctx, in.Principal.TenantID, in.ProfileName)
	if err != nil {
		o.log.Warn("failed to resolve agent profile for initialize instructions", "profile", in.ProfileName, "error", err)
		return nil
	}
	return &allow
}

// buildInstructions composes initialize's "instructions" field: the
// profile's own free text (if any), then the gateway's static pointer
// text, then a skill index when the profile has skills attached.
func buildInstructions(allow *profile.AllowList) string {
	parts := make([]string, 0, 3)
	if allow != nil && allow.Found && strings.TrimSpace(allow.Instructions) != "" {
		parts = append(parts, strings.TrimSpace(allow.Instructions))
	}
	parts = append(parts, instructions)
	if allow != nil && allow.Found && len(allow.Skills) > 0 {
		parts = append(parts, buildSkillIndex(allow.Skills))
	}
	return strings.Join(parts, "\n\n")
}

// buildSkillIndex renders the "Skills available to this profile" block,
// capped at maxSkillIndexEntries entries and maxSkillIndexBytes bytes,
// whichever is hit first -- a profile with more than either is summarized
// with a trailing "… and N more" rather than listed in full.
func buildSkillIndex(list []profile.ResolvedSkill) string {
	var b strings.Builder
	b.WriteString("Skills available to this profile. Load one with the tool `gateway__skill` " +
		"(argument `name`, optional `path`, default SKILL.md):\n")

	shown := 0
	for _, rs := range list {
		line := fmt.Sprintf("- %s: %s\n", rs.Skill.Name, rs.Skill.Description)
		if shown >= maxSkillIndexEntries || b.Len()+len(line) > maxSkillIndexBytes {
			break
		}
		b.WriteString(line)
		shown++
	}
	if shown < len(list) {
		fmt.Fprintf(&b, "… and %d more\n", len(list)-shown)
	}

	return strings.TrimRight(b.String(), "\n")
}
