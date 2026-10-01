package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/reqctx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/skillsext"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// The MCP Skills Extension (SEP-2640,
// https://modelcontextprotocol.io/extensions/skills/overview) lets a
// profile's attached skills (pkg/store.Skill rows of Kind "skill") be
// discovered and read the same way an MCP client already discovers and
// reads resources: skills/list and skills/get describe them,
// resources/read ("skill://<name>/<path>" URIs, handled here and
// dispatched from handleResourcesRead in prompts.go) serves their file
// contents.
//
// Skills never route to a connector -- they are served entirely by the
// gateway itself, resolved by internal/dataplane/skillsext against the
// caller's agent profile, independently of resolveProfile's tool
// allow-list.
//
// See internal/dataplane/skillsext's package doc for the wire shapes and
// the caching/error-code rationale; this file only renders them.
const (
	skillsTTLMs      = 30000
	skillsCacheScope = mcp.CacheScopePrivate
)

// resolveSkills loads the caller's attached skills. o.deps.Skills is nil
// only for an Orchestrator built without the extension (some tests);
// skills/list and skills/get then answer method-not-found, and
// initialize simply never advertises it.
func (o *Orchestrator) resolveSkills(ctx context.Context, in Request) (skillsext.Resolved, error) {
	if o.deps.Skills == nil {
		return skillsext.Resolved{}, nil
	}
	return o.deps.Skills.Resolve(ctx, in.Principal.TenantID, in.ProfileName)
}

// advertiseSkills adds the resources capability (if no connector already
// did) and the Skills Extension capability to caps, when the caller's
// resolved profile has at least one attached skill.
//
// A resolution failure here degrades to "no skills" rather than failing
// the handshake, the same philosophy fanOutInitialize documents for a
// connector that is down: a client that cannot initialize at all is
// worse than one that connects without an extension it will simply not
// use. skills/list and skills/get, by contrast, fail loudly on a store
// error (see handleSkillsList/handleSkillsGet) -- there the caller
// already believes the extension exists, and a silently empty answer
// would be indistinguishable from "this profile has no skills".
func (o *Orchestrator) advertiseSkills(ctx context.Context, in Request, caps *mcp.ServerCapabilities) {
	if o.deps.Skills == nil {
		return
	}
	resolved, err := o.resolveSkills(ctx, in)
	if err != nil {
		o.log.Warn("failed to resolve profile skills for initialize; not advertising the skills extension",
			"profile", in.ProfileName, "error", err)
		return
	}
	if len(resolved.Skills) == 0 {
		return
	}
	if caps.Resources == nil {
		caps.Resources = &mcp.ResourcesCapability{}
	}
	caps.Extensions = map[string]any{mcp.ExtensionSkills: map[string]any{}}
}

// ---------------------------------------------------------------------------
// skills/list, skills/get
// ---------------------------------------------------------------------------

func (o *Orchestrator) handleSkillsList(ctx context.Context, in Request) Result {
	if o.deps.Skills == nil {
		return o.fail(in, mcp.NewMethodNotFoundError(mcp.MethodSkillsList))
	}
	var params mcp.SkillsListParams
	if err := decodeParams(in.JSONRPC.Params, &params); err != nil {
		return o.fail(in, mcp.NewInvalidParamsError(err.Error()))
	}

	resolved, err := o.resolveSkills(ctx, in)
	if err != nil {
		o.log.Error("failed to resolve profile skills", "profile", in.ProfileName, "error", err)
		return o.fail(in, mcp.NewInternalError("failed to resolve agent profile's skills"))
	}

	names := resolved.Names()
	start, err := decodeSkillsCursor(params.Cursor, len(names))
	if err != nil {
		return o.fail(in, mcp.NewInvalidParamsError(err.Error()))
	}

	pageSize := o.deps.Skills.PageSize()
	end := start + pageSize
	if end > len(names) {
		end = len(names)
	}

	entries := make([]mcp.SkillEntry, 0, end-start)
	for _, name := range names[start:end] {
		sk, _ := resolved.Get(name)
		entries = append(entries, skillEntry(sk))
	}

	result := mcp.SkillsListResult{
		ResultType: mcp.ResultTypeComplete,
		Skills:     entries,
		TTLMs:      skillsTTLMs,
		CacheScope: skillsCacheScope,
	}
	if end < len(names) {
		result.NextCursor = strconv.Itoa(end)
	}

	return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, result)}
}

// decodeSkillsCursor turns a skills/list "cursor" into the start index of
// the next page: "" (no cursor) starts from the beginning; anything else
// must be a previously-issued nextCursor (one of this package's own
// decimal offsets, 0..n inclusive) or the request is malformed.
func decodeSkillsCursor(cursor string, n int) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	i, err := strconv.Atoi(cursor)
	if err != nil || i < 0 || i > n {
		return 0, fmt.Errorf("invalid cursor %q", cursor)
	}
	return i, nil
}

func (o *Orchestrator) handleSkillsGet(ctx context.Context, in Request) Result {
	if o.deps.Skills == nil {
		return o.fail(in, mcp.NewMethodNotFoundError(mcp.MethodSkillsGet))
	}
	var params mcp.SkillsGetParams
	if err := decodeParams(in.JSONRPC.Params, &params); err != nil {
		return o.fail(in, mcp.NewInvalidParamsError(err.Error()))
	}
	if params.URI == "" {
		return o.fail(in, mcp.NewInvalidParamsError(`"uri" is required`))
	}
	reqctx.From(ctx).SetTool(params.URI)

	name, path, ok := skillsext.ParseURI(params.URI)
	if !ok || path != skillsext.SkillMDPath {
		// A skill's identity is its SKILL.md URI -- the same one
		// skills/list's entries carry; anything else does not name a
		// skill at all.
		return o.fail(in, mcp.NewInvalidParamsError(fmt.Sprintf("invalid skill uri %q", params.URI)))
	}

	resolved, err := o.resolveSkills(ctx, in)
	if err != nil {
		o.log.Error("failed to resolve profile skills", "profile", in.ProfileName, "error", err)
		return o.fail(in, mcp.NewInternalError("failed to resolve agent profile's skills"))
	}
	sk, ok := resolved.Get(name)
	if !ok {
		return o.fail(in, skillDenied(in, name))
	}

	return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, mcp.SkillsGetResult{
		ResultType: mcp.ResultTypeComplete,
		Skill:      skillEntry(sk),
		TTLMs:      skillsTTLMs,
		CacheScope: skillsCacheScope,
	})}
}

// ---------------------------------------------------------------------------
// resources/read (skill:// URIs)
// ---------------------------------------------------------------------------

// handleSkillResourceRead serves resources/read for a skill:// URI. It is
// called from handleResourcesRead (prompts.go) before that method's
// gw://<connector>/ routing, since a skill file is never a connector
// resource.
func (o *Orchestrator) handleSkillResourceRead(ctx context.Context, in Request, uri string) Result {
	name, path, ok := skillsext.ParseURI(uri)
	if !ok {
		return o.fail(in, mcp.NewInvalidParamsError(fmt.Sprintf("invalid skill uri %q", uri)))
	}

	resolved, err := o.resolveSkills(ctx, in)
	if err != nil {
		o.log.Error("failed to resolve profile skills", "profile", in.ProfileName, "error", err)
		return o.fail(in, mcp.NewInternalError("failed to resolve agent profile's skills"))
	}
	sk, ok := resolved.Get(name)
	if !ok {
		return o.fail(in, skillDenied(in, name))
	}

	content, ok := skillFileContent(sk, path)
	if !ok {
		return o.fail(in, mcp.NewInvalidParamsError(fmt.Sprintf("skill %q has no file %q", name, path)))
	}

	return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, mcp.ResourcesReadResult{
		Contents: []mcp.ResourceContents{{
			URI:      uri,
			MimeType: skillsext.MimeType(path),
			Text:     &content,
		}},
		ResultType: mcp.ResultTypeComplete,
		TTLMs:      skillsTTLMs,
		CacheScope: skillsCacheScope,
	})}
}

func skillFileContent(sk skillsext.Skill, path string) (string, bool) {
	for _, f := range sk.Version.Files {
		if f.Path == path {
			return f.Content, true
		}
	}
	return "", false
}

// skillEntry renders a resolved skill as the wire shape skills/list and
// skills/get share: the SKILL.md URI, its full frontmatter, and a
// complete manifest (URI, sha256 digest, byte size) for every file in the
// resolved version, SKILL.md first -- see SEP-2640, "A manifest MUST
// include SKILL.md and every supporting file."
func skillEntry(sk skillsext.Skill) mcp.SkillEntry {
	skillMDURI := skillsext.URI(sk.Row.Name, skillsext.SkillMDPath)

	resources := make([]mcp.SkillManifestResource, 0, len(sk.Version.Files))
	for _, f := range sk.Version.Files {
		resources = append(resources, mcp.SkillManifestResource{
			URI:    skillsext.URI(sk.Row.Name, f.Path),
			Digest: "sha256:" + f.SHA256,
			Size:   f.Size,
		})
	}
	sort.Slice(resources, func(i, j int) bool {
		if resources[i].URI == skillMDURI {
			return true
		}
		if resources[j].URI == skillMDURI {
			return false
		}
		return resources[i].URI < resources[j].URI
	})

	return mcp.SkillEntry{
		URI:         skillMDURI,
		Frontmatter: sk.Row.Frontmatter,
		Resources:   resources,
	}
}

// skillDenied builds the -32003 profile-denial error for an unattached
// (or wholly unknown) skill, matching catalogDenied's shape for
// prompts/resources: -32003 covers both "does not exist" and "exists but
// not granted", since the profile enforcement layer never distinguishes
// them (see pkg/mcp.ErrorCodeToolNotAllowed's doc comment).
func skillDenied(in Request, name string) *mcp.Error {
	return mcp.NewError(mcp.ErrorCodeToolNotAllowed,
		fmt.Sprintf("skill %q not allowed by profile", name),
		map[string]any{"skill": name, "profile": in.ProfileName})
}
