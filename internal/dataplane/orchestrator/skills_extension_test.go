package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/router"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/session"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/skillsext"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/skills"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/session/memory"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const skillsTenant = "tenant-skills"

// skillsHarness is a from-scratch Orchestrator harness for the MCP Skills
// Extension (SEP-2640): a real dptest.Store, no connectors (skills never
// route to one), and a skillsext.Resolver the caller configures directly
// -- so pagination tests can use a small page size without going through
// the production default.
type skillsHarness struct {
	orch      *Orchestrator
	store     *dptest.Store
	principal *pkgauth.Principal
}

func newSkillsExtHarness(t *testing.T, resolverOpts skillsext.Options) *skillsHarness {
	t.Helper()
	ctx := context.Background()

	st := dptest.New()
	if err := st.Tenants().Create(ctx, &store.Tenant{ID: skillsTenant, Slug: "skills", Name: "Skills"}); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sessions := session.NewManager(memory.New(), session.Options{Logger: logger})
	cl := client.New(client.Options{Logger: logger})
	t.Cleanup(func() { _ = cl.Close() })

	resolver := skillsext.New(st.AgentProfiles(), st.Skills(), resolverOpts)

	orch, err := New(Deps{
		Sessions:   sessions,
		Router:     router.New(st.Connectors(), router.Options{}),
		Client:     cl,
		Connectors: st.Connectors(),
		Logger:     logger,
		Skills:     resolver,
	})
	if err != nil {
		t.Fatal(err)
	}

	return &skillsHarness{
		orch:  orch,
		store: st,
		principal: &pkgauth.Principal{
			Subject: "key-1", TenantID: skillsTenant, Roles: []string{"agent"}, AuthMethod: "apikey",
		},
	}
}

func (h *skillsHarness) handle(profileName string, req *mcp.Request) Result {
	return h.orch.Handle(pkgauth.WithPrincipal(context.Background(), h.principal), Request{
		JSONRPC: req, Principal: h.principal, ProfileName: profileName,
	})
}

// createSkill persists a valid, minimal "skill" kind row with SKILL.md
// plus any extra files, the same shape internal/skills.ValidateFiles and
// ParseFrontmatter would have produced for an API-created row.
func (h *skillsHarness) createSkill(t *testing.T, name string, enabled bool, extra ...store.SkillFile) *store.Skill {
	t.Helper()
	ctx := context.Background()

	skillMD := fmt.Sprintf("---\nname: %s\ndescription: Test skill %s.\n---\n\nBody of %s.\n", name, name, name)
	files := append([]store.SkillFile{{Path: "SKILL.md", Content: skillMD}}, extra...)
	validated, err := skills.ValidateFiles(files)
	if err != nil {
		t.Fatalf("ValidateFiles: %v", err)
	}
	fm, err := skills.ParseFrontmatter(skillMD, name)
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}

	sk := &store.Skill{
		TenantID: skillsTenant, Name: name, Kind: "skill",
		Description: skills.DescriptionFrom(fm), Frontmatter: fm, Enabled: enabled,
	}
	if err := h.store.Skills().Create(ctx, sk, validated, "tester"); err != nil {
		t.Fatalf("Skills().Create: %v", err)
	}
	return sk
}

func (h *skillsHarness) createProfile(t *testing.T, name string, items []store.ProfileSkill) *store.AgentProfile {
	t.Helper()
	ctx := context.Background()
	p := &store.AgentProfile{TenantID: skillsTenant, Name: name, Slug: profile.Slug(skillsTenant, name)}
	if err := h.store.AgentProfiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	for i := range items {
		items[i].AgentProfileID = p.ID
	}
	if err := h.store.AgentProfiles().SetSkills(ctx, p.ID, items); err != nil {
		t.Fatal(err)
	}
	return p
}

func skillsRPC(method string, id int, params string) *mcp.Request {
	req := &mcp.Request{JSONRPC: mcp.Version, ID: id, Method: method}
	if params != "" {
		req.Params = json.RawMessage(params)
	}
	return req
}

func decodeSkillsResult[T any](t *testing.T, res Result) T {
	t.Helper()
	var out T
	if res.Response == nil {
		t.Fatal("no response")
	}
	if res.Response.Error != nil {
		t.Fatalf("request failed: %+v", res.Response.Error)
	}
	if err := json.Unmarshal(res.Response.Result, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func wantSkillsError(t *testing.T, res Result, code int) *mcp.Error {
	t.Helper()
	if res.Response == nil || res.Response.Error == nil || res.Response.Error.Code != code {
		var got *mcp.Error
		if res.Response != nil {
			got = res.Response.Error
		}
		t.Fatalf("error = %+v, want code %d", got, code)
	}
	return res.Response.Error
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// initialize
// ---------------------------------------------------------------------------

func TestInitializeAdvertisesTheSkillsExtensionOnlyWithAttachedSkills(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	sk := h.createSkill(t, "code-review", true)
	h.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: sk.ID}})
	h.createProfile(t, "Empty", nil)

	initParams := `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}`

	for _, tc := range []struct {
		name        string
		profile     string
		wantAdvert  bool
		description string
	}{
		{"no profile header", "", false, "no profile header must not advertise the extension"},
		{"empty profile", "Empty", false, "a profile with no attached skills must not advertise the extension"},
		{"unknown profile", "Nope", false, "an unknown profile must not advertise the extension"},
		{"profile with a skill", "Reader", true, "a profile with an attached skill must advertise it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := h.handle(tc.profile, skillsRPC(mcp.MethodInitialize, 1, initParams))
			result := decodeSkillsResult[mcp.InitializeResult](t, res)

			hasExt := result.Capabilities.Extensions != nil && result.Capabilities.Extensions[mcp.ExtensionSkills] != nil
			if hasExt != tc.wantAdvert {
				t.Errorf("%s: extensions = %+v", tc.description, result.Capabilities.Extensions)
			}
			if tc.wantAdvert && result.Capabilities.Resources == nil {
				t.Error("the resources capability must be advertised alongside the skills extension")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// skills/list
// ---------------------------------------------------------------------------

func TestSkillsListShapeIncludesFrontmatterAndVerifiableDigests(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	notes := store.SkillFile{Path: "references/notes.md", Content: "# Notes\n\nSome reference notes.\n"}
	sk := h.createSkill(t, "code-review", true, notes)
	h.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: sk.ID}})

	res := h.handle("Reader", skillsRPC(mcp.MethodSkillsList, 1, ""))
	result := decodeSkillsResult[mcp.SkillsListResult](t, res)

	if result.ResultType != mcp.ResultTypeComplete {
		t.Errorf("resultType = %q, want %q", result.ResultType, mcp.ResultTypeComplete)
	}
	if result.TTLMs != 30000 {
		t.Errorf("ttlMs = %d, want 30000", result.TTLMs)
	}
	if result.CacheScope != mcp.CacheScopePrivate {
		t.Errorf("cacheScope = %q, want %q", result.CacheScope, mcp.CacheScopePrivate)
	}
	if result.NextCursor != "" {
		t.Errorf("nextCursor = %q, want none (only one skill)", result.NextCursor)
	}
	if len(result.Skills) != 1 {
		t.Fatalf("skills = %+v, want exactly one", result.Skills)
	}

	entry := result.Skills[0]
	if entry.URI != "skill://code-review/SKILL.md" {
		t.Errorf("uri = %q", entry.URI)
	}
	if entry.Frontmatter["name"] != "code-review" || entry.Frontmatter["description"] == "" {
		t.Errorf("frontmatter = %+v", entry.Frontmatter)
	}
	if len(entry.Resources) != 2 {
		t.Fatalf("resources = %+v, want SKILL.md and references/notes.md", entry.Resources)
	}
	// SKILL.md is always first.
	if entry.Resources[0].URI != "skill://code-review/SKILL.md" {
		t.Errorf("resources[0] = %+v, want SKILL.md first", entry.Resources[0])
	}
	skillMDContent := fmt.Sprintf("---\nname: %s\ndescription: Test skill %s.\n---\n\nBody of %s.\n",
		sk.Name, sk.Name, sk.Name)
	for _, r := range entry.Resources {
		switch r.URI {
		case "skill://code-review/SKILL.md":
			want := "sha256:" + sha256Hex(skillMDContent)
			if r.Digest != want || r.Size != len(skillMDContent) {
				t.Errorf("SKILL.md manifest = %+v, want digest %q size %d", r, want, len(skillMDContent))
			}
		case "skill://code-review/references/notes.md":
			want := "sha256:" + sha256Hex(notes.Content)
			if r.Digest != want || r.Size != len(notes.Content) {
				t.Errorf("notes.md manifest = %+v, want digest %q size %d", r, want, len(notes.Content))
			}
		default:
			t.Errorf("unexpected resource %q", r.URI)
		}
	}
}

func TestSkillsListPaginatesAcrossPages(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{PageSize: 2})
	var items []store.ProfileSkill
	for _, name := range []string{"alpha", "beta", "gamma"} {
		sk := h.createSkill(t, name, true)
		items = append(items, store.ProfileSkill{SkillID: sk.ID})
	}
	h.createProfile(t, "Reader", items)

	res := h.handle("Reader", skillsRPC(mcp.MethodSkillsList, 1, ""))
	page1 := decodeSkillsResult[mcp.SkillsListResult](t, res)
	if len(page1.Skills) != 2 || page1.NextCursor == "" {
		t.Fatalf("page 1 = %+v, want 2 entries and a nextCursor", page1)
	}

	res = h.handle("Reader", skillsRPC(mcp.MethodSkillsList, 2, `{"cursor":"`+page1.NextCursor+`"}`))
	page2 := decodeSkillsResult[mcp.SkillsListResult](t, res)
	if len(page2.Skills) != 1 || page2.NextCursor != "" {
		t.Fatalf("page 2 = %+v, want 1 entry and no nextCursor", page2)
	}

	seen := map[string]bool{}
	for _, e := range append(page1.Skills, page2.Skills...) {
		seen[e.URI] = true
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if !seen["skill://"+name+"/SKILL.md"] {
			t.Errorf("%s missing across the two pages: %v", name, seen)
		}
	}
}

func TestSkillsListInvalidCursorIsInvalidParams(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	sk := h.createSkill(t, "code-review", true)
	h.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: sk.ID}})

	res := h.handle("Reader", skillsRPC(mcp.MethodSkillsList, 1, `{"cursor":"not-a-number"}`))
	wantSkillsError(t, res, mcp.ErrorCodeInvalidParams)
}

func TestSkillsListWithoutAProfileHeaderIsEmpty(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	sk := h.createSkill(t, "code-review", true)
	h.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: sk.ID}})

	res := h.handle("", skillsRPC(mcp.MethodSkillsList, 1, ""))
	if res.Response.Error != nil {
		t.Fatalf("skills/list without a profile must not error: %+v", res.Response.Error)
	}
	if !strings.Contains(string(res.Response.Result), `"skills":[]`) {
		t.Errorf("result = %s, want an empty array on the wire", res.Response.Result)
	}
}

func TestSkillsListWithAnUnknownProfileIsEmpty(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	sk := h.createSkill(t, "code-review", true)
	h.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: sk.ID}})

	res := h.handle("Typo", skillsRPC(mcp.MethodSkillsList, 1, ""))
	result := decodeSkillsResult[mcp.SkillsListResult](t, res)
	if len(result.Skills) != 0 {
		t.Errorf("skills = %+v, want none for an unknown profile", result.Skills)
	}
}

// ---------------------------------------------------------------------------
// skills/get
// ---------------------------------------------------------------------------

func TestSkillsGetReturnsTheSameShapeAsList(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	sk := h.createSkill(t, "code-review", true)
	h.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: sk.ID}})

	res := h.handle("Reader", skillsRPC(mcp.MethodSkillsGet, 1, `{"uri":"skill://code-review/SKILL.md"}`))
	result := decodeSkillsResult[mcp.SkillsGetResult](t, res)

	if result.ResultType != mcp.ResultTypeComplete || result.TTLMs != 30000 || result.CacheScope != mcp.CacheScopePrivate {
		t.Errorf("result = %+v", result)
	}
	if result.Skill.URI != "skill://code-review/SKILL.md" || len(result.Skill.Resources) != 1 {
		t.Errorf("skill = %+v", result.Skill)
	}
}

func TestSkillsGetOnAnUnattachedSkillIs32003(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	sk := h.createSkill(t, "code-review", true)
	h.createProfile(t, "Reader", nil) // exists, grants nothing

	res := h.handle("Reader", skillsRPC(mcp.MethodSkillsGet, 1, `{"uri":"skill://code-review/SKILL.md"}`))
	mcpErr := wantSkillsError(t, res, mcp.ErrorCodeToolNotAllowed)

	var data struct {
		Skill   string `json:"skill"`
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal(mcpErr.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Skill != "code-review" || data.Profile != "Reader" {
		t.Errorf("error data = %+v, want skill=code-review profile=Reader", data)
	}
	_ = sk
}

func TestSkillsGetWithoutAProfileIs32003(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	sk := h.createSkill(t, "code-review", true)
	h.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: sk.ID}})

	res := h.handle("", skillsRPC(mcp.MethodSkillsGet, 1, `{"uri":"skill://code-review/SKILL.md"}`))
	wantSkillsError(t, res, mcp.ErrorCodeToolNotAllowed)
}

func TestSkillsGetOnANonRootURIIs32602(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	notes := store.SkillFile{Path: "references/notes.md", Content: "notes\n"}
	sk := h.createSkill(t, "code-review", true, notes)
	h.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: sk.ID}})

	for _, uri := range []string{
		"skill://code-review/references/notes.md",
		"not-a-skill-uri",
		"skill://code-review/",
	} {
		res := h.handle("Reader", skillsRPC(mcp.MethodSkillsGet, 1, `{"uri":"`+uri+`"}`))
		wantSkillsError(t, res, mcp.ErrorCodeInvalidParams)
	}
}

// ---------------------------------------------------------------------------
// resources/read (skill:// URIs)
// ---------------------------------------------------------------------------

func TestResourcesReadServesASkillFileWithAVerifiableDigest(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	notes := store.SkillFile{Path: "references/notes.md", Content: "# Notes\n\nSome reference notes.\n"}
	sk := h.createSkill(t, "code-review", true, notes)
	h.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: sk.ID}})

	listRes := h.handle("Reader", skillsRPC(mcp.MethodSkillsList, 1, ""))
	list := decodeSkillsResult[mcp.SkillsListResult](t, listRes)
	var notesDigest string
	for _, r := range list.Skills[0].Resources {
		if r.URI == "skill://code-review/references/notes.md" {
			notesDigest = r.Digest
		}
	}
	if notesDigest == "" {
		t.Fatal("skills/list did not manifest the supporting file")
	}

	params, _ := json.Marshal(mcp.ResourcesReadParams{URI: "skill://code-review/references/notes.md"})
	res := h.handle("Reader", skillsRPC(mcp.MethodResourcesRead, 2, string(params)))
	result := decodeSkillsResult[mcp.ResourcesReadResult](t, res)

	if len(result.Contents) != 1 || result.Contents[0].Text == nil || *result.Contents[0].Text != notes.Content {
		t.Fatalf("contents = %+v, want %q", result.Contents, notes.Content)
	}
	if result.Contents[0].MimeType != "text/markdown" {
		t.Errorf("mimeType = %q, want text/markdown for a .md file", result.Contents[0].MimeType)
	}
	if got := "sha256:" + sha256Hex(*result.Contents[0].Text); got != notesDigest {
		t.Errorf("served content's digest %q does not match the manifest's %q", got, notesDigest)
	}
	if result.ResultType != mcp.ResultTypeComplete || result.TTLMs != 30000 || result.CacheScope != mcp.CacheScopePrivate {
		t.Errorf("caching fields = resultType=%q ttlMs=%d cacheScope=%q", result.ResultType, result.TTLMs, result.CacheScope)
	}
}

func TestResourcesReadMimeTypeForANonMarkdownFile(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	data := store.SkillFile{Path: "references/data.json", Content: `{"a":1}`}
	sk := h.createSkill(t, "code-review", true, data)
	h.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: sk.ID}})

	params, _ := json.Marshal(mcp.ResourcesReadParams{URI: "skill://code-review/references/data.json"})
	res := h.handle("Reader", skillsRPC(mcp.MethodResourcesRead, 1, string(params)))
	result := decodeSkillsResult[mcp.ResourcesReadResult](t, res)
	if result.Contents[0].MimeType != "text/plain" {
		t.Errorf("mimeType = %q, want text/plain", result.Contents[0].MimeType)
	}
}

func TestResourcesReadOnAnUnattachedSkillIs32003(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	sk := h.createSkill(t, "code-review", true)
	h.createProfile(t, "Reader", nil)

	params, _ := json.Marshal(mcp.ResourcesReadParams{URI: "skill://code-review/SKILL.md"})
	res := h.handle("Reader", skillsRPC(mcp.MethodResourcesRead, 1, string(params)))
	wantSkillsError(t, res, mcp.ErrorCodeToolNotAllowed)
	_ = sk
}

func TestResourcesReadOnAnUnknownFileWithinAnAttachedSkillIs32602(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	sk := h.createSkill(t, "code-review", true)
	h.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: sk.ID}})

	params, _ := json.Marshal(mcp.ResourcesReadParams{URI: "skill://code-review/references/missing.md"})
	res := h.handle("Reader", skillsRPC(mcp.MethodResourcesRead, 1, string(params)))
	wantSkillsError(t, res, mcp.ErrorCodeInvalidParams)
}

func TestResourcesReadOnAMalformedSkillURIIs32602(t *testing.T) {
	h := newSkillsExtHarness(t, skillsext.Options{})
	sk := h.createSkill(t, "code-review", true)
	h.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: sk.ID}})

	params, _ := json.Marshal(mcp.ResourcesReadParams{URI: "skill://"})
	res := h.handle("Reader", skillsRPC(mcp.MethodResourcesRead, 1, string(params)))
	wantSkillsError(t, res, mcp.ErrorCodeInvalidParams)
}

// ---------------------------------------------------------------------------
// extension disabled
// ---------------------------------------------------------------------------

func TestSkillsMethodsAnswerMethodNotFoundWithoutTheExtensionWired(t *testing.T) {
	st := dptest.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sessions := session.NewManager(memory.New(), session.Options{Logger: logger})
	cl := client.New(client.Options{Logger: logger})
	t.Cleanup(func() { _ = cl.Close() })

	orch, err := New(Deps{
		Sessions: sessions, Router: router.New(st.Connectors(), router.Options{}),
		Client: cl, Connectors: st.Connectors(), Logger: logger, // Skills left nil
	})
	if err != nil {
		t.Fatal(err)
	}
	principal := &pkgauth.Principal{Subject: "key-1", TenantID: skillsTenant, Roles: []string{"agent"}, AuthMethod: "apikey"}

	for _, method := range []string{mcp.MethodSkillsList, mcp.MethodSkillsGet} {
		res := orch.Handle(context.Background(), Request{JSONRPC: skillsRPC(method, 1, ""), Principal: principal})
		wantSkillsError(t, res, mcp.ErrorCodeMethodNotFound)
	}
}
