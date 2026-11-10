package llmplane

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	pkgsink "github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// ProfileHeader is the request header a caller uses to say which agent
// profile it is acting as. The MCP plane reads the same header, so an agent
// that sends it to both planes is one agent in the analytics.
const ProfileHeader = "X-Agent-Profile-Name"

// ProfileNamer resolves an API key's bound profile id to the profile's
// name, "" when it cannot (no such profile, or a lookup error). It is
// called once per captured call, so implementations must be cached.
type ProfileNamer interface {
	ProfileName(ctx context.Context, tenantID, profileID string) string
}

// profileNameTTL is how long NewProfileNames remembers a profile's name. A
// renamed profile shows under its old name in new rows until it lapses.
const profileNameTTL = 30 * time.Second

type profileNames struct {
	profiles store.AgentProfileStore
	ttl      time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]profileNameEntry
}

type profileNameEntry struct {
	name    string
	expires time.Time
}

// NewProfileNames returns a ProfileNamer over the tenant's agent profiles,
// caching each answer (including "no such profile") for ttl; <= 0 uses 30
// seconds.
func NewProfileNames(profiles store.AgentProfileStore, ttl time.Duration) ProfileNamer {
	if ttl <= 0 {
		ttl = profileNameTTL
	}
	return &profileNames{profiles: profiles, ttl: ttl, now: time.Now, cache: map[string]profileNameEntry{}}
}

func (p *profileNames) ProfileName(ctx context.Context, tenantID, profileID string) string {
	key := tenantID + "|" + profileID
	now := p.now()
	p.mu.Lock()
	if e, ok := p.cache[key]; ok && now.Before(e.expires) {
		p.mu.Unlock()
		return e.name
	}
	p.mu.Unlock()

	prof, err := p.profiles.Get(ctx, tenantID, profileID)
	if err != nil || prof == nil {
		// A lookup error is not cached: the next call retries it. A
		// profile that does not exist is, so a dangling binding costs one
		// query per ttl rather than one per call.
		if err == nil || errors.Is(err, store.ErrNotFound) {
			p.put(key, "", now)
		}
		return ""
	}
	p.put(key, prof.Name, now)
	return prof.Name
}

func (p *profileNames) put(key, name string, now time.Time) {
	p.mu.Lock()
	p.cache[key] = profileNameEntry{name: name, expires: now.Add(p.ttl)}
	p.mu.Unlock()
}

// callProfile is the agent profile a captured call is attributed to: the
// profile the caller's API key is bound to when it has one (the caller
// cannot change that), otherwise its X-Agent-Profile-Name header, cleaned
// and capped. A bound key whose profile cannot be resolved is attributed to
// no profile rather than to whatever the header claims.
func (rt *router) callProfile(r *http.Request) string {
	if p, ok := pkgauth.PrincipalFrom(r.Context()); ok && p.ProfileID != "" {
		if rt.profiles == nil {
			return ""
		}
		return rt.profiles.ProfileName(r.Context(), p.TenantID, p.ProfileID)
	}
	return pkgsink.CapProfile(r.Header.Get(ProfileHeader))
}
