package skillsext_test

import (
	"context"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/skillsext"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/skills"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const tenant = "tenant-a"

type fixture struct {
	resolver *skillsext.Resolver
	store    *dptest.Store
}

func newFixture(t *testing.T, opts skillsext.Options) *fixture {
	t.Helper()
	st := dptest.New()
	return &fixture{
		resolver: skillsext.New(st.AgentProfiles(), st.Skills(), opts),
		store:    st,
	}
}

// createSkill persists a valid, minimal skill (or command) row with one
// SKILL.md file, mirroring what internal/skills.ValidateFiles and
// ParseFrontmatter would have already produced for an API-created row.
func (f *fixture) createSkill(t *testing.T, name, kind string, enabled bool, extra ...store.SkillFile) *store.Skill {
	t.Helper()
	ctx := context.Background()

	skillMD := "---\nname: " + name + "\ndescription: Test skill " + name + ".\n---\n\nBody of " + name + ".\n"
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
		TenantID: tenant, Name: name, Kind: kind,
		Description: skills.DescriptionFrom(fm), Frontmatter: fm, Enabled: enabled,
	}
	if err := f.store.Skills().Create(ctx, sk, validated, "tester"); err != nil {
		t.Fatalf("Skills().Create: %v", err)
	}
	return sk
}

func (f *fixture) createProfile(t *testing.T, name string, items []store.ProfileSkill) *store.AgentProfile {
	t.Helper()
	ctx := context.Background()
	p := &store.AgentProfile{TenantID: tenant, Name: name, Slug: profile.Slug(tenant, name)}
	if err := f.store.AgentProfiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	for i := range items {
		items[i].AgentProfileID = p.ID
	}
	if err := f.store.AgentProfiles().SetSkills(ctx, p.ID, items); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveGrantsOnlyAttachedSkills(t *testing.T) {
	f := newFixture(t, skillsext.Options{})
	granted := f.createSkill(t, "code-review", "skill", true)
	f.createSkill(t, "not-attached", "skill", true)
	f.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: granted.ID}})

	resolved, err := f.resolver.Resolve(context.Background(), tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Found {
		t.Fatal("profile was not found")
	}
	sk, ok := resolved.Get("code-review")
	if !ok {
		t.Fatal("the attached skill was not resolved")
	}
	if sk.Version.Version != 1 || len(sk.Version.Files) != 1 {
		t.Errorf("resolved version = %+v, want version 1 with SKILL.md", sk.Version)
	}
	if _, ok := resolved.Get("not-attached"); ok {
		t.Error("a skill that was never attached was resolved")
	}
	if got := resolved.Names(); len(got) != 1 || got[0] != "code-review" {
		t.Errorf("Names() = %v, want [code-review]", got)
	}
}

func TestResolveSkipsCommandKindRows(t *testing.T) {
	// The MCP Skills Extension surfaces skills, not commands: a command
	// is Phase 2's native-prompt surface (prompts/list, prompts/get).
	f := newFixture(t, skillsext.Options{})
	cmd := f.createSkill(t, "greet", "command", true)
	f.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: cmd.ID}})

	resolved, err := f.resolver.Resolve(context.Background(), tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolved.Get("greet"); ok {
		t.Fatal("a command-kind attachment was resolved as a skill")
	}
	if len(resolved.Skills) != 0 {
		t.Errorf("Skills = %v, want empty", resolved.Skills)
	}
}

func TestResolveSkipsDisabledSkills(t *testing.T) {
	f := newFixture(t, skillsext.Options{})
	disabled := f.createSkill(t, "retired", "skill", false)
	f.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: disabled.ID}})

	resolved, err := f.resolver.Resolve(context.Background(), tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolved.Get("retired"); ok {
		t.Fatal("a disabled skill was resolved")
	}
}

func TestResolveHonoursAPinnedVersionOrFallsBackToLatest(t *testing.T) {
	f := newFixture(t, skillsext.Options{})
	sk := f.createSkill(t, "runbook", "skill", true)
	ctx := context.Background()
	skillMDv2 := "---\nname: runbook\ndescription: v2.\n---\n\nv2 body.\n"
	filesV2, err := skills.ValidateFiles([]store.SkillFile{{Path: "SKILL.md", Content: skillMDv2}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Skills().AddVersion(ctx, tenant, sk.ID, filesV2, "tester"); err != nil {
		t.Fatal(err)
	}

	pinned := 1
	f.createProfile(t, "Pinned", []store.ProfileSkill{{SkillID: sk.ID, Version: &pinned}})
	f.createProfile(t, "Latest", []store.ProfileSkill{{SkillID: sk.ID}})

	resolvedPinned, err := f.resolver.Resolve(ctx, tenant, "Pinned")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := resolvedPinned.Get("runbook")
	if got.Version.Version != 1 {
		t.Errorf("pinned resolution = version %d, want 1", got.Version.Version)
	}

	resolvedLatest, err := f.resolver.Resolve(ctx, tenant, "Latest")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = resolvedLatest.Get("runbook")
	if got.Version.Version != 2 {
		t.Errorf("unpinned resolution = version %d, want the latest (2)", got.Version.Version)
	}
}

func TestResolveSkipsADanglingAttachment(t *testing.T) {
	f := newFixture(t, skillsext.Options{})
	f.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: "skill-gone"}})

	resolved, err := f.resolver.Resolve(context.Background(), tenant, "Reader")
	if err != nil {
		t.Fatalf("a dangling attachment must not fail resolution: %v", err)
	}
	if len(resolved.Skills) != 0 {
		t.Errorf("Skills = %v, want empty", resolved.Skills)
	}
}

func TestMissingProfileGrantsNothing(t *testing.T) {
	f := newFixture(t, skillsext.Options{})
	granted := f.createSkill(t, "code-review", "skill", true)
	f.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: granted.ID}})

	resolved, err := f.resolver.Resolve(context.Background(), tenant, "Typo")
	if err != nil {
		t.Fatalf("a missing profile must not be an error: %v", err)
	}
	if resolved.Found {
		t.Fatal("a profile that does not exist was reported as found")
	}
	if len(resolved.Skills) != 0 {
		t.Fatal("a missing profile granted something")
	}
}

func TestEmptyProfileNameGrantsNothing(t *testing.T) {
	// "No profile header -> no skills" is decided above the store, not
	// derived from a lookup of an empty-named row.
	f := newFixture(t, skillsext.Options{})

	resolved, err := f.resolver.Resolve(context.Background(), tenant, "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Found || len(resolved.Skills) != 0 {
		t.Fatalf("resolved = %+v, want zero skills and Found=false", resolved)
	}
}

func TestResolveCachesForTheConfiguredWindow(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	f := newFixture(t, skillsext.Options{TTL: 30 * time.Second, Now: clock})
	sk := f.createSkill(t, "code-review", "skill", true)
	prof := f.createProfile(t, "Reader", nil)
	ctx := context.Background()

	resolved, err := f.resolver.Resolve(ctx, tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Skills) != 0 {
		t.Fatal("expected nothing attached yet")
	}

	// Attach behind the cache's back.
	if err := f.store.AgentProfiles().SetSkills(ctx, prof.ID, []store.ProfileSkill{{SkillID: sk.ID}}); err != nil {
		t.Fatal(err)
	}

	resolved, err = f.resolver.Resolve(ctx, tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolved.Get("code-review"); ok {
		t.Fatal("the cache was bypassed within its TTL")
	}

	now = now.Add(31 * time.Second)
	resolved, err = f.resolver.Resolve(ctx, tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolved.Get("code-review"); !ok {
		t.Fatal("the cache did not expire after its TTL")
	}
}

func TestInvalidateDropsTheCachedBundle(t *testing.T) {
	f := newFixture(t, skillsext.Options{})
	sk := f.createSkill(t, "code-review", "skill", true)
	prof := f.createProfile(t, "Reader", []store.ProfileSkill{{SkillID: sk.ID}})
	ctx := context.Background()

	if _, err := f.resolver.Resolve(ctx, tenant, "Reader"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AgentProfiles().SetSkills(ctx, prof.ID, nil); err != nil {
		t.Fatal(err)
	}
	f.resolver.Invalidate(tenant, "Reader")

	resolved, err := f.resolver.Resolve(ctx, tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolved.Get("code-review"); ok {
		t.Fatal("Invalidate did not drop the cached bundle")
	}
}

func TestPageSizeDefaultsTo50(t *testing.T) {
	f := newFixture(t, skillsext.Options{})
	if got := f.resolver.PageSize(); got != 50 {
		t.Errorf("PageSize() = %d, want 50", got)
	}
	f2 := newFixture(t, skillsext.Options{PageSize: 2})
	if got := f2.resolver.PageSize(); got != 2 {
		t.Errorf("PageSize() = %d, want 2", got)
	}
}

func TestParseURI(t *testing.T) {
	cases := []struct {
		uri      string
		name     string
		path     string
		wantOK   bool
		testName string
	}{
		{"skill://code-review/SKILL.md", "code-review", "SKILL.md", true, "root file"},
		{"skill://code-review/references/checklist.md", "code-review", "references/checklist.md", true, "nested file"},
		{"gw://alpha/SKILL.md", "", "", false, "wrong scheme"},
		{"skill://", "", "", false, "no name or path"},
		{"skill://code-review", "", "", false, "no path"},
		{"skill://code-review/", "", "", false, "empty path"},
		{"skill:///SKILL.md", "", "", false, "empty name"},
	}
	for _, tc := range cases {
		t.Run(tc.testName, func(t *testing.T) {
			name, path, ok := skillsext.ParseURI(tc.uri)
			if ok != tc.wantOK || name != tc.name || path != tc.path {
				t.Errorf("ParseURI(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.uri, name, path, ok, tc.name, tc.path, tc.wantOK)
			}
		})
	}
}

func TestURIRoundTripsWithParseURI(t *testing.T) {
	uri := skillsext.URI("code-review", "references/checklist.md")
	if uri != "skill://code-review/references/checklist.md" {
		t.Fatalf("URI() = %q", uri)
	}
	name, path, ok := skillsext.ParseURI(uri)
	if !ok || name != "code-review" || path != "references/checklist.md" {
		t.Fatalf("ParseURI(URI(...)) = (%q, %q, %v)", name, path, ok)
	}
}

func TestMimeType(t *testing.T) {
	cases := map[string]string{
		"SKILL.md":               "text/markdown",
		"references/notes.md":    "text/markdown",
		"references/data.json":   "text/plain",
		"references/table.csv":   "text/plain",
		"references/config.yaml": "text/plain",
	}
	for path, want := range cases {
		if got := skillsext.MimeType(path); got != want {
			t.Errorf("MimeType(%q) = %q, want %q", path, got, want)
		}
	}
}
