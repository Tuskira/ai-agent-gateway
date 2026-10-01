// Package profile enforces agent profiles: the per-caller allow-list of
// (connector, tool) pairs that bounds what an agent can see and run.
//
// Two rules make this an enforcement mechanism rather than a filter:
//
//   - the allow-list is checked on tools/call, not only on tools/list.
//     Filtering a list an agent can simply ignore is a suggestion; a
//     denial on the call is a control.
//   - a profile name that does not resolve grants NOTHING. Falling back
//     to every tool in the tenant would let a typo in a header silently
//     widen access instead of narrowing it.
package profile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Header is the inbound header naming the profile to enforce.
const Header = "X-Agent-Profile-Name"

// defaultTTL is how long a resolved allow-list is cached per
// (tenant, slug). Short enough that revoking a tool takes effect in
// seconds, long enough that a chatty agent does not re-query Postgres on
// every call.
const defaultTTL = 30 * time.Second

// ResolvedSkill is one skill or command attached to a profile, together
// with the effective SkillVersion it resolved to: the pinned version's
// content when the attachment pinned one, else the skill's current
// LatestVersion. Skill.Kind says which family it belongs to ("skill" or
// "command"); AllowList splits them into Skills and Commands for callers
// that only want one.
type ResolvedSkill struct {
	Skill   store.Skill
	Version *store.SkillVersion
}

// AllowList is a resolved profile: which tools of which connectors the
// caller may see and run, plus (Phase 2) its free-text Instructions and
// its attached Skills/Commands.
//
// The zero value denies everything, which is what a caller holds after
// naming a profile that does not exist.
type AllowList struct {
	// Name is the profile name as the caller spelled it.
	Name string
	// Slug is the row key the name resolved to.
	Slug string
	// Found reports whether a profile row actually exists. A false
	// Found is the "named profile is missing" case: it denies
	// everything, and the distinction is kept for logging.
	Found bool

	// Instructions is the profile's own free-text, prepended to the
	// gateway's static initialize instructions (internal/dataplane/
	// orchestrator). Empty when the profile sets none.
	Instructions string
	// Skills are the profile's attached kind="skill" rows, enabled only,
	// sorted by name. Loaded exactly once per Resolve (and so shares its
	// cache TTL) -- a disabled or deleted-out-from-under-the-profile
	// skill is silently omitted, the same tolerance load() already gives
	// a dangling tool grant.
	Skills []ResolvedSkill
	// Commands are the profile's attached kind="command" rows, same
	// rules as Skills.
	Commands []ResolvedSkill

	// tools maps connector id to the set of unprefixed tool names
	// granted on that connector. Keying on the connector id, rather
	// than on a bare tool name, is what stops a grant of "search" on
	// one connector from also unlocking "search" on another.
	tools map[string]map[string]struct{}
}

// FindSkill returns the attached kind="skill" row named name, if any.
func (a AllowList) FindSkill(name string) (ResolvedSkill, bool) {
	return findResolvedSkill(a.Skills, name)
}

// FindCommand returns the attached kind="command" row named name, if any.
func (a AllowList) FindCommand(name string) (ResolvedSkill, bool) {
	return findResolvedSkill(a.Commands, name)
}

func findResolvedSkill(list []ResolvedSkill, name string) (ResolvedSkill, bool) {
	for _, rs := range list {
		if rs.Skill.Name == name {
			return rs, true
		}
	}
	return ResolvedSkill{}, false
}

// Allows reports whether the profile grants toolName (unprefixed) on the
// given connector.
func (a AllowList) Allows(connectorID, toolName string) bool {
	if !a.Found {
		return false
	}
	_, ok := a.tools[connectorID][toolName]
	return ok
}

// AllowsConnector reports whether the profile grants at least one tool
// on the given connector. It is the gate for that connector's prompts
// and resources: they carry no grants of their own (yet), so a profile
// that can use a connector at all can read its prompts and resources,
// and one that cannot sees none of them.
func (a AllowList) AllowsConnector(connectorID string) bool {
	if !a.Found {
		return false
	}
	return len(a.tools[connectorID]) > 0
}

// Size returns how many (connector, tool) pairs the profile grants.
func (a AllowList) Size() int {
	var n int
	for _, set := range a.tools {
		n += len(set)
	}
	return n
}

// Enforcer resolves profile names to allow-lists, with a short cache.
type Enforcer struct {
	profiles   store.AgentProfileStore
	connectors store.ConnectorStore
	skills     store.SkillStore

	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
	// bound caches profile rows by "tenant|id" for API keys bound to a
	// profile (see Bound).
	bound map[string]boundEntry
}

type boundEntry struct {
	prof      *store.AgentProfile // nil: no live profile with that id
	expiresAt time.Time
}

// Bound returns the live profile row with the given id in tenantID, or nil
// when there is none (deleted, or another tenant's). It backs API keys
// bound to a profile: a nil result means the binding is dangling and the
// caller must fail closed. Results are cached for the enforcer's TTL.
func (e *Enforcer) Bound(ctx context.Context, tenantID, profileID string) (*store.AgentProfile, error) {
	key := tenantID + "|" + profileID
	now := e.now()
	e.mu.Lock()
	if entry, ok := e.bound[key]; ok && now.Before(entry.expiresAt) {
		e.mu.Unlock()
		return entry.prof, nil
	}
	e.mu.Unlock()

	prof, err := e.profiles.Get(ctx, tenantID, profileID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("profile: look up bound profile %s: %w", profileID, err)
		}
		prof = nil
	}
	e.mu.Lock()
	e.bound[key] = boundEntry{prof: prof, expiresAt: now.Add(e.ttl)}
	e.mu.Unlock()
	return prof, nil
}

// NameForSlug returns the profile name under which Resolve reaches the
// row stored at slug: the slug minus its "<tenant id>-" prefix.
func NameForSlug(tenantID, slug string) string {
	return strings.TrimPrefix(slug, tenantID+"-")
}

// Matches reports whether a caller-supplied profile name designates prof.
func Matches(tenantID string, prof *store.AgentProfile, name string) bool {
	return Slug(tenantID, name) == prof.Slug || NormalizeName(name) == NormalizeName(prof.Name)
}

type cacheEntry struct {
	allow     AllowList
	expiresAt time.Time
}

// Options configures an Enforcer.
type Options struct {
	// TTL is how long a resolved allow-list is cached. Zero uses 30s.
	TTL time.Duration
	// Now returns the current time; tests override it.
	Now func() time.Time
}

// New returns an Enforcer over the profile, connector and skill stores.
// skills may be nil (a store.Store that doesn't wire one) -- a resolved
// profile then simply carries no Skills/Commands, same as one with none
// attached.
func New(profiles store.AgentProfileStore, connectors store.ConnectorStore, skills store.SkillStore, opts Options) *Enforcer {
	if opts.TTL <= 0 {
		opts.TTL = defaultTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Enforcer{
		profiles:   profiles,
		connectors: connectors,
		skills:     skills,
		ttl:        opts.TTL,
		now:        opts.Now,
		cache:      make(map[string]cacheEntry),
		bound:      make(map[string]boundEntry),
	}
}

// Resolve returns the allow-list for a profile name within a tenant.
//
// A missing profile is not an error: it returns an AllowList with
// Found false, which denies everything. An error is returned only when
// the store itself failed, which must not be confused with "denied" --
// the caller turns a store failure into an internal error, not into a
// silent empty tool list.
func (e *Enforcer) Resolve(ctx context.Context, tenantID, profileName string) (AllowList, error) {
	slug := Slug(tenantID, profileName)
	key := tenantID + "|" + slug

	now := e.now()
	e.mu.Lock()
	if entry, ok := e.cache[key]; ok && now.Before(entry.expiresAt) {
		e.mu.Unlock()
		return entry.allow, nil
	}
	e.mu.Unlock()

	allow, err := e.load(ctx, tenantID, profileName, slug)
	if err != nil {
		return AllowList{Name: profileName, Slug: slug}, err
	}

	e.mu.Lock()
	e.cache[key] = cacheEntry{allow: allow, expiresAt: now.Add(e.ttl)}
	e.mu.Unlock()

	return allow, nil
}

func (e *Enforcer) load(ctx context.Context, tenantID, profileName, slug string) (AllowList, error) {
	allow := AllowList{Name: profileName, Slug: slug}

	prof, err := e.profiles.GetBySlug(ctx, tenantID, slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return allow, nil // Found stays false: grants nothing.
		}
		return allow, fmt.Errorf("profile: look up %q: %w", slug, err)
	}

	tools, err := e.profiles.GetTools(ctx, tenantID, prof.ID)
	if err != nil {
		return allow, fmt.Errorf("profile: list tools of %q: %w", slug, err)
	}

	allow.Found = true
	allow.Instructions = prof.Instructions
	allow.tools = make(map[string]map[string]struct{}, len(tools))

	// A profile row may store a tool name either bare ("echo") or
	// already qualified ("everything__echo"), depending on which
	// version of the admin API wrote it. Normalize to the bare name
	// against the connector's own name so both shapes enforce the same.
	names := make(map[string]string, len(tools))
	for _, pt := range tools {
		connName, ok := names[pt.ConnectorID]
		if !ok {
			conn, err := e.connectors.Get(ctx, tenantID, pt.ConnectorID)
			switch {
			case errors.Is(err, store.ErrNotFound):
				// The connector was deleted out from under the
				// profile. Skip the grant rather than fail: the
				// tool it names cannot be routed to anyway.
				names[pt.ConnectorID] = ""
				continue
			case err != nil:
				return allow, fmt.Errorf("profile: look up connector %s: %w", pt.ConnectorID, err)
			}
			connName = client.Qualifier(conn)
			names[pt.ConnectorID] = connName
		}
		if connName == "" {
			continue
		}

		bare := strings.TrimPrefix(pt.ToolName, connName+"__")
		if allow.tools[pt.ConnectorID] == nil {
			allow.tools[pt.ConnectorID] = make(map[string]struct{})
		}
		allow.tools[pt.ConnectorID][bare] = struct{}{}
	}

	if err := e.loadSkills(ctx, tenantID, prof.ID, &allow); err != nil {
		return allow, err
	}

	return allow, nil
}

// loadSkills resolves prof's attached skills/commands into allow.Skills
// and allow.Commands: each ProfileSkill.SkillID is looked up (tenant row
// or platform row), disabled or no-longer-visible rows are silently
// skipped -- attachment is a grant, and a grant onto a row that vanished
// or was disabled grants nothing, the same tolerance the tool grants
// above give a deleted connector -- and the effective SkillVersion is
// resolved: the pinned Version when the attachment set one, else the
// skill's current LatestVersion.
func (e *Enforcer) loadSkills(ctx context.Context, tenantID, profileID string, allow *AllowList) error {
	if e.skills == nil {
		return nil
	}

	items, err := e.profiles.GetSkills(ctx, profileID)
	if err != nil {
		return fmt.Errorf("profile: list skills of %q: %w", allow.Slug, err)
	}

	for _, item := range items {
		sk, err := e.skills.Get(ctx, tenantID, item.SkillID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue // deleted, or no longer visible to this tenant.
		case err != nil:
			return fmt.Errorf("profile: look up skill %s: %w", item.SkillID, err)
		}
		if !sk.Enabled {
			continue
		}

		version := sk.LatestVersion
		if item.Version != nil {
			version = *item.Version
		}
		ver, err := e.skills.GetVersion(ctx, sk.ID, version)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // a pinned version that no longer exists.
			}
			return fmt.Errorf("profile: look up version %d of skill %s: %w", version, item.SkillID, err)
		}

		resolved := ResolvedSkill{Skill: *sk, Version: ver}
		if sk.Kind == "command" {
			allow.Commands = append(allow.Commands, resolved)
		} else {
			allow.Skills = append(allow.Skills, resolved)
		}
	}

	sortResolvedSkills(allow.Skills)
	sortResolvedSkills(allow.Commands)
	return nil
}

func sortResolvedSkills(list []ResolvedSkill) {
	sort.Slice(list, func(i, j int) bool { return list[i].Skill.Name < list[j].Skill.Name })
}

// Invalidate drops the cached allow-list for one profile, so a grant
// change takes effect before the TTL lapses.
func (e *Enforcer) Invalidate(tenantID, profileName string) {
	e.InvalidateSlug(tenantID, Slug(tenantID, profileName))
}

// InvalidateSlug drops the cached allow-list for the profile at slug
// directly, bypassing NormalizeName. Callers that already hold a stored
// AgentProfile row (e.g. the control-plane API, which knows the row's own
// Slug column rather than whatever spelling a caller's X-Agent-Profile-Name
// header might use) should prefer this over Invalidate: it is correct even
// when the profile's Name has changed since it was created (Slug does
// not follow it -- see docs/profiles.md).
func (e *Enforcer) InvalidateSlug(tenantID, slug string) {
	key := tenantID + "|" + slug
	e.mu.Lock()
	delete(e.cache, key)
	// A bound-profile row may have been renamed or deleted too.
	for k := range e.bound {
		if strings.HasPrefix(k, tenantID+"|") {
			delete(e.bound, k)
		}
	}
	e.mu.Unlock()
}

// InvalidateTenant drops every cached allow-list for one tenant. It is
// the "simplest acceptable" response to a registry-level change whose
// blast radius isn't worth computing precisely -- a skill/command write
// (a new version, enable/disable, delete) may affect any number of
// profiles that attach it, and finding exactly which ones isn't worth a
// query when the cache's own TTL already bounds how long a miss costs.
func (e *Enforcer) InvalidateTenant(tenantID string) {
	prefix := tenantID + "|"
	e.mu.Lock()
	defer e.mu.Unlock()
	for key := range e.cache {
		if strings.HasPrefix(key, prefix) {
			delete(e.cache, key)
		}
	}
	for key := range e.bound {
		if strings.HasPrefix(key, prefix) {
			delete(e.bound, key)
		}
	}
}

// maxSlugLen is the agent_profiles.slug column width (VARCHAR(100)).
const maxSlugLen = 100

// Slug renders the agent_profiles row key for a profile name within a
// tenant: "<tenant id>-<normalized name>", the name part cut to fit
// maxSlugLen and "item" when the name has no letters or digits. The API
// stores exactly this, so a header naming the profile always finds it.
func Slug(tenantID, name string) string {
	n := NormalizeName(name)
	if limit := maxSlugLen - len(tenantID) - 1; limit > 0 && len(n) > limit {
		n = strings.TrimSuffix(n[:limit], "-")
	}
	if n == "" {
		n = "item"
	}
	return tenantID + "-" + n
}

// NormalizeName lowercases a profile name and collapses every run of
// characters outside [a-z0-9] into a single hyphen, trimming hyphens from
// both ends. "SOC Analyst (L1)" and "soc_analyst-l1" both become
// "soc-analyst-l1", so a caller does not have to reproduce the exact
// punctuation the profile was created with.
func NormalizeName(name string) string {
	var b strings.Builder
	b.Grow(len(name))

	lastHyphen := true // suppresses a leading hyphen
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}

	return strings.TrimSuffix(b.String(), "-")
}
