package profile_test

import (
	"context"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const tenant = "tenant-a"

type fixture struct {
	enforcer *profile.Enforcer
	store    *dptest.Store
	connA    *store.Connector
	connB    *store.Connector
}

func newFixture(t *testing.T, opts profile.Options) *fixture {
	t.Helper()
	ctx := context.Background()
	st := dptest.New()

	connA := &store.Connector{TenantID: tenant, Name: "alpha", Slug: "alpha", Endpoint: "http://a", Status: "healthy"}
	connB := &store.Connector{TenantID: tenant, Name: "beta", Slug: "beta", Endpoint: "http://b", Status: "healthy"}
	for _, c := range []*store.Connector{connA, connB} {
		if err := st.Connectors().Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	return &fixture{
		enforcer: profile.New(st.AgentProfiles(), st.Connectors(), st.Skills(), opts),
		store:    st,
		connA:    connA,
		connB:    connB,
	}
}

func (f *fixture) createProfile(t *testing.T, name string, tools []store.ProfileTool) *store.AgentProfile {
	t.Helper()
	ctx := context.Background()
	p := &store.AgentProfile{TenantID: tenant, Name: name, Slug: profile.Slug(tenant, name)}
	if err := f.store.AgentProfiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AgentProfiles().SetTools(ctx, tenant, p.ID, tools); err != nil {
		t.Fatal(err)
	}
	return p
}

// createProfileWithInstructions is createProfile's twin for tests that
// also need AgentProfile.Instructions set.
func (f *fixture) createProfileWithInstructions(t *testing.T, name, instructions string) *store.AgentProfile {
	t.Helper()
	p := &store.AgentProfile{TenantID: tenant, Name: name, Slug: profile.Slug(tenant, name), Instructions: instructions}
	if err := f.store.AgentProfiles().Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

// createSkill persists a minimal, valid skill/command row directly
// (dptest's Create doesn't re-derive frontmatter, so a one-line SKILL.md
// body is enough).
func (f *fixture) createSkill(t *testing.T, tenantID, name, kind, description string, args []store.CommandArgument, enabled bool) *store.Skill {
	t.Helper()
	sk := &store.Skill{
		TenantID: tenantID, Name: name, Kind: kind, Description: description,
		Arguments: args, Enabled: enabled, Metadata: map[string]any{},
	}
	files := []store.SkillFile{{Path: "SKILL.md", Content: "---\nname: " + name + "\ndescription: " + description + "\n---\nbody of " + name}}
	if err := f.store.Skills().Create(context.Background(), sk, files, "test"); err != nil {
		t.Fatal(err)
	}
	return sk
}

func (f *fixture) attachSkills(t *testing.T, profileID string, items []store.ProfileSkill) {
	t.Helper()
	if err := f.store.AgentProfiles().SetSkills(context.Background(), profileID, items); err != nil {
		t.Fatal(err)
	}
}

func TestResolveGrantsOnlyTheListedTools(t *testing.T) {
	f := newFixture(t, profile.Options{})
	f.createProfile(t, "Reader", []store.ProfileTool{
		{ConnectorID: f.connA.ID, ToolName: "search"},
	})

	allow, err := f.enforcer.Resolve(context.Background(), tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if !allow.Found {
		t.Fatal("profile was not found")
	}
	if !allow.Allows(f.connA.ID, "search") {
		t.Error("the granted tool was denied")
	}
	if allow.Allows(f.connA.ID, "write") {
		t.Error("an ungranted tool on the same connector was allowed")
	}
	if allow.Size() != 1 {
		t.Errorf("Size() = %d, want 1", allow.Size())
	}
}

func TestGrantsAreScopedToOneConnector(t *testing.T) {
	// Granting "search" on alpha must not unlock beta's "search":
	// two backends commonly expose the same tool name, and one of them
	// may be the one the profile was written to keep out.
	f := newFixture(t, profile.Options{})
	f.createProfile(t, "Reader", []store.ProfileTool{
		{ConnectorID: f.connA.ID, ToolName: "search"},
	})

	allow, err := f.enforcer.Resolve(context.Background(), tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if allow.Allows(f.connB.ID, "search") {
		t.Fatal("a grant on one connector leaked to another")
	}
}

func TestAllowsConnectorNeedsOneGrantedToolOnThatConnector(t *testing.T) {
	// Prompts and resources have no grants of their own: a connector's
	// are in scope exactly when the profile grants any of its tools.
	f := newFixture(t, profile.Options{})
	f.createProfile(t, "Reader", []store.ProfileTool{{ConnectorID: f.connA.ID, ToolName: "search"}})
	f.createProfile(t, "Empty", nil)

	allow, err := f.enforcer.Resolve(context.Background(), tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if !allow.AllowsConnector(f.connA.ID) {
		t.Error("a connector with a granted tool was not allowed")
	}
	if allow.AllowsConnector(f.connB.ID) {
		t.Error("a connector with no granted tool was allowed")
	}

	for _, name := range []string{"Empty", "Missing"} {
		allow, err := f.enforcer.Resolve(context.Background(), tenant, name)
		if err != nil {
			t.Fatal(err)
		}
		if allow.AllowsConnector(f.connA.ID) {
			t.Errorf("profile %q allowed a connector", name)
		}
	}
}

func TestMissingProfileGrantsNothing(t *testing.T) {
	// The gateway's predecessor fell back to every tool in the tenant
	// here, so a typo in a header silently WIDENED access.
	f := newFixture(t, profile.Options{})
	f.createProfile(t, "Reader", []store.ProfileTool{{ConnectorID: f.connA.ID, ToolName: "search"}})

	allow, err := f.enforcer.Resolve(context.Background(), tenant, "Raeder")
	if err != nil {
		t.Fatalf("a missing profile must not be an error: %v", err)
	}
	if allow.Found {
		t.Fatal("a profile that does not exist was reported as found")
	}
	if allow.Allows(f.connA.ID, "search") || allow.Size() != 0 {
		t.Fatal("a missing profile granted something")
	}
}

func TestProfileWithNoToolsGrantsNothing(t *testing.T) {
	f := newFixture(t, profile.Options{})
	f.createProfile(t, "Empty", nil)

	allow, err := f.enforcer.Resolve(context.Background(), tenant, "Empty")
	if err != nil {
		t.Fatal(err)
	}
	if !allow.Found {
		t.Fatal("an empty profile still exists")
	}
	if allow.Allows(f.connA.ID, "search") {
		t.Fatal("an empty profile granted a tool")
	}
}

func TestAlreadyQualifiedToolNamesAreAccepted(t *testing.T) {
	// A profile row may have been written with the runtime name.
	f := newFixture(t, profile.Options{})
	f.createProfile(t, "Reader", []store.ProfileTool{
		{ConnectorID: f.connA.ID, ToolName: "alpha__search"},
	})

	allow, err := f.enforcer.Resolve(context.Background(), tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if !allow.Allows(f.connA.ID, "search") {
		t.Fatal("a qualified grant did not match the backend's own tool name")
	}
}

func TestGrantsForADeletedConnectorAreSkipped(t *testing.T) {
	f := newFixture(t, profile.Options{})
	f.createProfile(t, "Reader", []store.ProfileTool{
		{ConnectorID: f.connA.ID, ToolName: "search"},
		{ConnectorID: "conn-gone", ToolName: "ghost"},
	})

	allow, err := f.enforcer.Resolve(context.Background(), tenant, "Reader")
	if err != nil {
		t.Fatalf("a dangling grant must not fail resolution: %v", err)
	}
	if !allow.Allows(f.connA.ID, "search") {
		t.Error("the live grant was lost")
	}
	if allow.Allows("conn-gone", "ghost") {
		t.Error("a grant for a deleted connector was honoured")
	}
}

func TestResolveCachesForTheConfiguredWindow(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	f := newFixture(t, profile.Options{TTL: 30 * time.Second, Now: clock})
	p := f.createProfile(t, "Reader", []store.ProfileTool{{ConnectorID: f.connA.ID, ToolName: "search"}})
	ctx := context.Background()

	if _, err := f.enforcer.Resolve(ctx, tenant, "Reader"); err != nil {
		t.Fatal(err)
	}

	// Widen the grant behind the cache's back.
	if err := f.store.AgentProfiles().SetTools(ctx, tenant, p.ID, []store.ProfileTool{
		{ConnectorID: f.connA.ID, ToolName: "search"},
		{ConnectorID: f.connA.ID, ToolName: "write"},
	}); err != nil {
		t.Fatal(err)
	}

	allow, err := f.enforcer.Resolve(ctx, tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if allow.Allows(f.connA.ID, "write") {
		t.Fatal("the cache was bypassed within its TTL")
	}

	now = now.Add(31 * time.Second)
	allow, err = f.enforcer.Resolve(ctx, tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if !allow.Allows(f.connA.ID, "write") {
		t.Fatal("the cache did not expire after its TTL")
	}
}

func TestInvalidateDropsTheCachedAllowList(t *testing.T) {
	f := newFixture(t, profile.Options{})
	p := f.createProfile(t, "Reader", []store.ProfileTool{{ConnectorID: f.connA.ID, ToolName: "search"}})
	ctx := context.Background()

	if _, err := f.enforcer.Resolve(ctx, tenant, "Reader"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AgentProfiles().SetTools(ctx, tenant, p.ID, nil); err != nil {
		t.Fatal(err)
	}
	f.enforcer.Invalidate(tenant, "Reader")

	allow, err := f.enforcer.Resolve(ctx, tenant, "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if allow.Allows(f.connA.ID, "search") {
		t.Fatal("Invalidate did not drop the cached allow-list")
	}
}

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"SOC Analyst (L1)": "soc-analyst-l1",
		"soc_analyst-l1":   "soc-analyst-l1",
		"  Spaced  Out  ":  "spaced-out",
		"Read-Only":        "read-only",
		"already-slugged":  "already-slugged",
		"!!!":              "",
		"a":                "a",
	}
	for in, want := range cases {
		if got := profile.NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Slug must match the row key POST /profiles stores: truncated to the
// 100-char slug column, and "item" for a name with no letters or digits.
// Otherwise such a profile can never be resolved by header.
func TestSlugMatchesStoredRowKey(t *testing.T) {
	tid := "f2c0297b-e18a-44d9-b12f-892140829e77"
	long := profile.Slug(tid, "Security Operations Center Tier One Analyst Read Only Investigation Profile")
	if want := tid + "-security-operations-center-tier-one-analyst-read-only-investiga"; long != want {
		t.Errorf("Slug(long) = %q, want %q", long, want)
	}
	if got := profile.Slug(tid, "!!!"); got != tid+"-item" {
		t.Errorf("Slug(%q) = %q, want %q", "!!!", got, tid+"-item")
	}
	if got := profile.Slug(tid, "SOC Analyst (L1)"); got != tid+"-soc-analyst-l1" {
		t.Errorf("Slug(short) = %q, want unchanged %q", got, tid+"-soc-analyst-l1")
	}
}

func TestSlugIsTenantScoped(t *testing.T) {
	// Two tenants may both have a profile called "Reader"; the row key
	// must not collide.
	if profile.Slug("t1", "Reader") == profile.Slug("t2", "Reader") {
		t.Fatal("profile slugs are not tenant-scoped")
	}
	if got, want := profile.Slug("t1", "Read Only"), "t1-read-only"; got != want {
		t.Fatalf("Slug = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Skills & commands (Phase 2)
// ---------------------------------------------------------------------------

func TestResolveCarriesInstructionsSkillsAndCommands(t *testing.T) {
	f := newFixture(t, profile.Options{})
	p := f.createProfileWithInstructions(t, "Analyst", "Always cite the source alert id.")

	skill := f.createSkill(t, tenant, "triage-guide", "skill", "How to triage an alert", nil, true)
	cmd := f.createSkill(t, tenant, "summarize", "command", "Summarize a case", []store.CommandArgument{{Name: "case_id", Required: true}}, true)
	f.attachSkills(t, p.ID, []store.ProfileSkill{{AgentProfileID: p.ID, SkillID: skill.ID}, {AgentProfileID: p.ID, SkillID: cmd.ID}})

	allow, err := f.enforcer.Resolve(context.Background(), tenant, "Analyst")
	if err != nil {
		t.Fatal(err)
	}
	if allow.Instructions != "Always cite the source alert id." {
		t.Errorf("Instructions = %q", allow.Instructions)
	}
	if len(allow.Skills) != 1 || allow.Skills[0].Skill.Name != "triage-guide" {
		t.Fatalf("Skills = %+v, want one entry named triage-guide", allow.Skills)
	}
	if len(allow.Commands) != 1 || allow.Commands[0].Skill.Name != "summarize" {
		t.Fatalf("Commands = %+v, want one entry named summarize", allow.Commands)
	}
	if _, ok := allow.FindSkill("triage-guide"); !ok {
		t.Error("FindSkill did not find the attached skill")
	}
	if _, ok := allow.FindCommand("summarize"); !ok {
		t.Error("FindCommand did not find the attached command")
	}
	if _, ok := allow.FindSkill("summarize"); ok {
		t.Error("FindSkill found a command")
	}
}

func TestResolveSkipsDisabledAndDanglingSkills(t *testing.T) {
	f := newFixture(t, profile.Options{})
	p := f.createProfileWithInstructions(t, "Analyst", "")

	live := f.createSkill(t, tenant, "live", "skill", "still enabled", nil, true)
	disabled := f.createSkill(t, tenant, "off", "skill", "disabled", nil, false)
	f.attachSkills(t, p.ID, []store.ProfileSkill{
		{AgentProfileID: p.ID, SkillID: live.ID},
		{AgentProfileID: p.ID, SkillID: disabled.ID},
		{AgentProfileID: p.ID, SkillID: "skill-gone"}, // never existed
	})

	allow, err := f.enforcer.Resolve(context.Background(), tenant, "Analyst")
	if err != nil {
		t.Fatalf("a dangling/disabled skill grant must not fail resolution: %v", err)
	}
	if len(allow.Skills) != 1 || allow.Skills[0].Skill.Name != "live" {
		t.Fatalf("Skills = %+v, want only \"live\"", allow.Skills)
	}
}

func TestResolveVersionPinningVsLatest(t *testing.T) {
	f := newFixture(t, profile.Options{})
	pinned := f.createProfileWithInstructions(t, "Pinned", "")
	floating := f.createProfileWithInstructions(t, "Floating", "")

	sk := f.createSkill(t, tenant, "runbook", "skill", "v1", nil, true)
	v1 := sk.LatestVersion
	if _, err := f.store.Skills().AddVersion(context.Background(), tenant, sk.ID, []store.SkillFile{
		{Path: "SKILL.md", Content: "---\nname: runbook\ndescription: v2\n---\nbody v2"},
	}, "test"); err != nil {
		t.Fatal(err)
	}

	f.attachSkills(t, pinned.ID, []store.ProfileSkill{{AgentProfileID: pinned.ID, SkillID: sk.ID, Version: &v1}})
	f.attachSkills(t, floating.ID, []store.ProfileSkill{{AgentProfileID: floating.ID, SkillID: sk.ID}})

	pinnedAllow, err := f.enforcer.Resolve(context.Background(), tenant, "Pinned")
	if err != nil {
		t.Fatal(err)
	}
	rs, ok := pinnedAllow.FindSkill("runbook")
	if !ok || rs.Version == nil || rs.Version.Version != v1 {
		t.Fatalf("pinned resolution = %+v, want version %d", rs, v1)
	}

	floatingAllow, err := f.enforcer.Resolve(context.Background(), tenant, "Floating")
	if err != nil {
		t.Fatal(err)
	}
	rs, ok = floatingAllow.FindSkill("runbook")
	if !ok || rs.Version == nil || rs.Version.Version != v1+1 {
		t.Fatalf("floating resolution = %+v, want the latest version %d", rs, v1+1)
	}
}

func TestResolveSeesPlatformSkills(t *testing.T) {
	f := newFixture(t, profile.Options{})
	p := f.createProfileWithInstructions(t, "Analyst", "")

	platformSkill := f.createSkill(t, "", "shared-runbook", "skill", "platform default", nil, true)
	f.attachSkills(t, p.ID, []store.ProfileSkill{{AgentProfileID: p.ID, SkillID: platformSkill.ID}})

	allow, err := f.enforcer.Resolve(context.Background(), tenant, "Analyst")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := allow.FindSkill("shared-runbook"); !ok {
		t.Error("a platform skill attached to a tenant profile did not resolve")
	}
}

func TestInvalidateTenantDropsEveryProfileInTheTenant(t *testing.T) {
	f := newFixture(t, profile.Options{})
	a := f.createProfile(t, "A", []store.ProfileTool{{ConnectorID: f.connA.ID, ToolName: "search"}})
	b := f.createProfile(t, "B", []store.ProfileTool{{ConnectorID: f.connA.ID, ToolName: "search"}})
	ctx := context.Background()

	if _, err := f.enforcer.Resolve(ctx, tenant, "A"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enforcer.Resolve(ctx, tenant, "B"); err != nil {
		t.Fatal(err)
	}

	// Widen both behind the cache's back.
	for _, p := range []*store.AgentProfile{a, b} {
		if err := f.store.AgentProfiles().SetTools(ctx, tenant, p.ID, []store.ProfileTool{
			{ConnectorID: f.connA.ID, ToolName: "search"},
			{ConnectorID: f.connA.ID, ToolName: "write"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	f.enforcer.InvalidateTenant(tenant)

	for _, name := range []string{"A", "B"} {
		allow, err := f.enforcer.Resolve(ctx, tenant, name)
		if err != nil {
			t.Fatal(err)
		}
		if !allow.Allows(f.connA.ID, "write") {
			t.Errorf("InvalidateTenant did not drop the cached allow-list for %q", name)
		}
	}
}

func TestInvalidateSlugIsCorrectAfterARename(t *testing.T) {
	// A profile's Slug is fixed at creation and does not follow a later
	// Name change (see docs/profiles.md); InvalidateSlug must key off the
	// stored Slug, not a Name-derived recomputation, to stay correct
	// after one.
	f := newFixture(t, profile.Options{})
	p := f.createProfile(t, "Original Name", []store.ProfileTool{{ConnectorID: f.connA.ID, ToolName: "search"}})
	ctx := context.Background()

	if _, err := f.enforcer.Resolve(ctx, tenant, "Original Name"); err != nil {
		t.Fatal(err)
	}

	p.Name = "Renamed"
	if err := f.store.AgentProfiles().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AgentProfiles().SetTools(ctx, tenant, p.ID, []store.ProfileTool{
		{ConnectorID: f.connA.ID, ToolName: "search"},
		{ConnectorID: f.connA.ID, ToolName: "write"},
	}); err != nil {
		t.Fatal(err)
	}

	f.enforcer.InvalidateSlug(tenant, p.Slug)

	allow, err := f.enforcer.Resolve(ctx, tenant, "Original Name")
	if err != nil {
		t.Fatal(err)
	}
	if !allow.Allows(f.connA.ID, "write") {
		t.Fatal("InvalidateSlug did not drop the cached allow-list keyed by the row's own slug")
	}
}
