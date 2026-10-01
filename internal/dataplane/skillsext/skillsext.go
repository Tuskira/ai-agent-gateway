// Package skillsext resolves the skills attached to an agent profile for
// the MCP Skills Extension (SEP-2640).
//
// It is deliberately independent of internal/dataplane/profile, the
// gateway-native profile enforcer that Phase 2 of the skills-and-commands
// work (SKILLS-CONTRACT.md) is extending, in parallel and on a separate
// branch, to carry the same profile_skills attachments for the
// gateway-native gateway__skill tool. Building a second, small resolver
// here -- rather than depending on Phase 2's still-unmerged changes to
// that package -- lets this extension land on its own and converge with
// Phase 2's resolution in a later merge (see SKILLS-CONTRACT.md's Phase
// 2/3 split). The only thing borrowed from it is profile.Slug/Header,
// the tenant-scoped slug convention and header name every profile lookup
// in this codebase already shares.
//
// Wire shapes this package's callers (internal/dataplane/orchestrator)
// implement, per https://modelcontextprotocol.io/extensions/skills/overview
// and the MCP caching utility it depends on
// (https://modelcontextprotocol.io/specification/draft/server/utilities/caching),
// protocol revision 2026-07-28 or later:
//
//   - Capability, declared in `initialize` only when Resolve returns at
//     least one skill:
//     {"capabilities": {"resources": {}, "extensions": {"io.modelcontextprotocol/skills": {}}}}
//     The extension object is empty because resources/directory/read is
//     not implemented; the spec defines an empty object as "support
//     without directory reading".
//   - skills/list and skills/get share one entry shape:
//     {"uri": "skill://<name>/SKILL.md", "frontmatter": {...},
//     "resources": [{"uri": "...", "digest": "sha256:<hex>", "size": N}, ...]}
//     A manifest always includes SKILL.md itself, per the spec's "A
//     manifest MUST include SKILL.md and every supporting file."
//   - Both list/get results, and a resources/read of a skill file, carry
//     "resultType":"complete", "ttlMs":30000 and "cacheScope":"private".
//     "private" (not "public") because every one of these responses
//     depends on the caller's agent profile -- its authorization context
//     -- which is exactly the caching utility's test for that value:
//     "private ... MAY be reused for the same authorization context ...
//     MUST NOT be shared across authorization contexts".
//   - Errors: a skill:// URI naming a skill the resolved profile does not
//     grant (including one that does not exist at all, or a request with
//     no profile header) is a profile denial, -32003, with data
//     {"skill": "<name>", "profile": "<name as sent>"} -- the same code
//     and shape tools/prompts/resources already use for "the target
//     exists but the profile does not grant it". An unknown file within a
//     known, granted skill, or a malformed skill:// URI, is -32602 per
//     the extension's error table ("Unknown skill/file ... -32602").
package skillsext

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Scheme is the URI scheme skill files are served under, per the
// extension's "Servers SHOULD use the skill:// scheme."
const Scheme = "skill://"

// SkillMDPath is the file every skill carries at its root, and the path
// component of the URI that names the skill itself (as opposed to one of
// its supporting files).
const SkillMDPath = "SKILL.md"

// kindSkill is the only pkg/store.Skill Kind this package exposes. A
// "command" row is Phase 2's native-prompt surface (prompts/list,
// prompts/get) and is deliberately not listed here: exposing the same
// registry row through two different MCP surfaces would let a client
// load a command's template as if it were workflow instructions.
const kindSkill = "skill"

// defaultTTL matches the 30s cache internal/dataplane/profile.Enforcer
// already uses for a resolved profile's tool allow-list.
const defaultTTL = 30 * time.Second

// defaultPageSize bounds one skills/list page.
const defaultPageSize = 50

// Skill is one resolved, versioned attachment: the registry row plus the
// exact file set of the version the profile is pinned to (or the row's
// current LatestVersion, when the attachment did not pin one).
type Skill struct {
	Row     store.Skill
	Version store.SkillVersion
}

// Resolved is the skills one agent profile grants, keyed by skill name.
type Resolved struct {
	// Found reports whether the named profile row exists at all. False
	// resolves to zero skills either way -- a missing profile grants
	// nothing, the same rule internal/dataplane/profile.AllowList
	// applies to tools -- but is kept for logging, like AllowList.Found.
	Found bool
	// Skills is never nil, even when empty.
	Skills map[string]Skill
}

// Get returns the attached skill named name, or ok=false when the
// profile does not grant it.
func (r Resolved) Get(name string) (Skill, bool) {
	sk, ok := r.Skills[name]
	return sk, ok
}

// Names returns the attached skills' names, sorted -- the order
// skills/list paginates over.
func (r Resolved) Names() []string {
	names := make([]string, 0, len(r.Skills))
	for n := range r.Skills {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Resolver loads and caches one tenant's profiles' attached skills.
type Resolver struct {
	profiles store.AgentProfileStore
	skills   store.SkillStore

	ttl      time.Duration
	pageSize int
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	resolved  Resolved
	expiresAt time.Time
}

// Options configures a Resolver.
type Options struct {
	// TTL is how long a resolved bundle is cached; zero uses 30s.
	TTL time.Duration
	// PageSize bounds one skills/list page; zero uses 50.
	PageSize int
	// Now returns the current time; tests override it.
	Now func() time.Time
}

// New returns a Resolver over the profile and skill store facets.
func New(profiles store.AgentProfileStore, skills store.SkillStore, opts Options) *Resolver {
	if opts.TTL <= 0 {
		opts.TTL = defaultTTL
	}
	if opts.PageSize <= 0 {
		opts.PageSize = defaultPageSize
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Resolver{
		profiles: profiles,
		skills:   skills,
		ttl:      opts.TTL,
		pageSize: opts.PageSize,
		now:      opts.Now,
		cache:    make(map[string]cacheEntry),
	}
}

// PageSize returns the configured skills/list page size.
func (r *Resolver) PageSize() int { return r.pageSize }

// Resolve returns tenantID's profileName's attached, enabled, kind
// "skill" rows, with a 30s cache.
//
// An empty profileName resolves to zero skills without touching the
// store: "no profile header -> no skills" is a decision the skills and
// commands feature makes independently of tools' RequireProfile
// fallback (see SKILLS-CONTRACT.md, "Decisions already made") -- a
// missing header narrows to nothing here even when the tenant would
// otherwise see every tool.
func (r *Resolver) Resolve(ctx context.Context, tenantID, profileName string) (Resolved, error) {
	if profileName == "" {
		return Resolved{Skills: map[string]Skill{}}, nil
	}

	slug := profile.Slug(tenantID, profileName)
	key := tenantID + "|" + slug

	now := r.now()
	r.mu.Lock()
	if entry, ok := r.cache[key]; ok && now.Before(entry.expiresAt) {
		r.mu.Unlock()
		return entry.resolved, nil
	}
	r.mu.Unlock()

	resolved, err := r.load(ctx, tenantID, slug)
	if err != nil {
		return Resolved{Skills: map[string]Skill{}}, err
	}

	r.mu.Lock()
	r.cache[key] = cacheEntry{resolved: resolved, expiresAt: now.Add(r.ttl)}
	r.mu.Unlock()

	return resolved, nil
}

func (r *Resolver) load(ctx context.Context, tenantID, slug string) (Resolved, error) {
	out := Resolved{Skills: map[string]Skill{}}

	prof, err := r.profiles.GetBySlug(ctx, tenantID, slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return out, nil // Found stays false: grants nothing.
		}
		return out, fmt.Errorf("skillsext: look up profile %q: %w", slug, err)
	}
	out.Found = true

	items, err := r.profiles.GetSkills(ctx, prof.ID)
	if err != nil {
		return out, fmt.Errorf("skillsext: list skill attachments of %q: %w", slug, err)
	}

	for _, item := range items {
		row, err := r.skills.Get(ctx, tenantID, item.SkillID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // the attached row was deleted out from under the profile
			}
			return out, fmt.Errorf("skillsext: look up skill %s: %w", item.SkillID, err)
		}
		if row.Kind != kindSkill || !row.Enabled {
			continue
		}

		version := row.LatestVersion
		if item.Version != nil {
			version = *item.Version
		}
		ver, err := r.skills.GetVersion(ctx, row.ID, version)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // a pinned version that no longer exists
			}
			return out, fmt.Errorf("skillsext: load %s@%d: %w", row.Name, version, err)
		}

		out.Skills[row.Name] = Skill{Row: *row, Version: *ver}
	}

	return out, nil
}

// Invalidate drops the cached bundle for one profile, so an attachment
// change takes effect before the TTL lapses. Nothing on this branch calls
// it yet -- Phase 2 owns the PUT /profiles/{id}/skills handler that will
// -- but it mirrors internal/dataplane/profile.Enforcer.Invalidate and
// costs nothing to have ready for that wiring.
func (r *Resolver) Invalidate(tenantID, profileName string) {
	key := tenantID + "|" + profile.Slug(tenantID, profileName)
	r.mu.Lock()
	delete(r.cache, key)
	r.mu.Unlock()
}

// URI builds the skill:// URI for a file at path within the named skill.
func URI(name, path string) string { return Scheme + name + "/" + path }

// ParseURI splits a skill:// URI into its skill name and file path.
// ok is false for anything not shaped "skill://<name>/<path>" -- no
// scheme, an empty name, or an empty (trailing-slash) path.
func ParseURI(uri string) (name, path string, ok bool) {
	rest, found := strings.CutPrefix(uri, Scheme)
	if !found {
		return "", "", false
	}
	i := strings.IndexByte(rest, '/')
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// MimeType returns the MIME type resources/read reports for a skill
// file's path: "text/markdown" for SKILL.md and any other ".md" file,
// "text/plain" for the registry's other allowed extensions (.txt, .json,
// .yaml, .yml, .csv, .xml, .toml) -- every file in the registry is text.
func MimeType(path string) string {
	if strings.HasSuffix(path, ".md") {
		return "text/markdown"
	}
	return "text/plain"
}
