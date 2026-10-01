package apikey

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const (
	defaultCacheTTL = 30 * time.Second
	// defaultNegativeTTL is how long a lookup that found NO such key is
	// remembered. Short on purpose: it only has to absorb a burst of
	// repeated bad requests, and a key created moments after a failed
	// attempt with the same secret (practically impossible: secrets are
	// 256-bit random) would otherwise wait it out.
	defaultNegativeTTL = 5 * time.Second
	// defaultNegativeMax bounds the negative cache (LRU).
	defaultNegativeMax = 10000

	defaultTouchInterval = 60 * time.Second

	// touchQueueSize bounds the background touch worker's inbound
	// channel. A full queue means last_used_at updates are arriving
	// faster than Postgres can absorb them; scheduleTouch drops rather
	// than blocking the request path.
	touchQueueSize = 256

	// touchTimeout bounds each background TouchLastUsed call.
	touchTimeout = 5 * time.Second
)

// Options configures an Authenticator. The zero value is valid: every
// field falls back to a documented default.
type Options struct {
	// CacheTTL is how long a successful key lookup is cached in memory
	// before the next Authenticate for that key re-queries the store. It
	// is the BACKSTOP for revocation: a revoke normally evicts the entry
	// at once (locally, and on every other replica through the store's
	// RevocationNotifier), but a replica that missed the notification
	// serves a revoked key for at most this long. Zero uses the default
	// of 30s.
	CacheTTL time.Duration

	// NegativeTTL is how long a lookup that found no such key is cached
	// (so a flood of requests with one bad key costs one DB query, not
	// one each). Zero uses 5s; negative disables the negative cache.
	NegativeTTL time.Duration
	// NegativeMax bounds the negative cache (least-recently-used entries
	// are dropped first). Zero uses 10000.
	NegativeMax int

	// Guard, when set, adds the per-credential failure dimension: every
	// failed lookup of an unknown key is reported against its key prefix,
	// and once a prefix is locked, only keys already in the cache are
	// served. Typically a *internal/auth.RateLimiter; a guard carried by WithGuard
	// takes precedence (per-plane limiters).
	Guard CredentialGuard

	// TouchInterval throttles how often a given key's last_used_at is
	// persisted: at most once per TouchInterval, regardless of request
	// volume. Zero uses the default of 60s.
	TouchInterval time.Duration

	// Now returns the current time. Nil uses time.Now; tests override it
	// for deterministic cache-expiry and throttling behavior.
	Now func() time.Time
}

// CredentialGuard is the per-credential-prefix half of the auth-failure
// limiter (see internal/auth.RateLimiter.CredLocked/ReportCredFailure).
type CredentialGuard interface {
	CredLocked(prefix string) (locked bool, retryAfter time.Duration)
	ReportCredFailure(prefix string)
}

type guardCtxKey struct{}

// WithGuard returns a context carrying g as the credential guard for any
// Authenticate call made with it, overriding Options.Guard. One
// Authenticator (and so one lookup cache and negative cache) serves every
// plane, but each plane keeps its own failure counters: the plane's
// rate-limiting wrapper passes its own limiter through here.
func WithGuard(ctx context.Context, g CredentialGuard) context.Context {
	return context.WithValue(ctx, guardCtxKey{}, g)
}

// cacheEntry is one key-hash's cached lookup result.
type cacheEntry struct {
	key       *store.APIKey
	expiresAt time.Time
}

// touchRequest asks the background worker to persist a key's
// last-used-at timestamp.
type touchRequest struct {
	id string
	at time.Time
}

// Authenticator authenticates requests bearing a gateway-issued API key
// (see ParseBearer, Hash). It implements auth.Authenticator.
//
// The zero value is not usable; construct with New.
type Authenticator struct {
	keys store.APIKeyStore

	cacheTTL      time.Duration
	touchInterval time.Duration
	now           func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry

	neg   *negativeCache
	guard CredentialGuard

	touchMu   sync.Mutex
	lastTouch map[string]time.Time

	touchCh chan touchRequest
}

var _ auth.Authenticator = (*Authenticator)(nil)

// New returns an Authenticator backed by keys, applying opts (with
// defaults filled in). It starts a single background goroutine that
// persists last_used_at touches for the life of the process; there is no
// Stop, matching this Authenticator's intended lifetime (one per gateway
// process, alongside the store itself).
func New(keys store.APIKeyStore, opts Options) *Authenticator {
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = defaultCacheTTL
	}
	if opts.TouchInterval <= 0 {
		opts.TouchInterval = defaultTouchInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NegativeTTL == 0 {
		opts.NegativeTTL = defaultNegativeTTL
	}
	if opts.NegativeMax <= 0 {
		opts.NegativeMax = defaultNegativeMax
	}

	a := &Authenticator{
		keys:          keys,
		cacheTTL:      opts.CacheTTL,
		touchInterval: opts.TouchInterval,
		now:           opts.Now,
		cache:         make(map[string]cacheEntry),
		guard:         opts.Guard,
		lastTouch:     make(map[string]time.Time),
		touchCh:       make(chan touchRequest, touchQueueSize),
	}

	if opts.NegativeTTL > 0 {
		a.neg = newNegativeCache(opts.NegativeMax, opts.NegativeTTL)
	}

	go a.runTouchWorker()

	return a
}

// Name identifies this Authenticator within an auth.Chain.
func (a *Authenticator) Name() string { return "apikey" }

// Authenticate resolves r's gateway API key (Authorization: Bearer gk_...
// or X-Gateway-Key) to a Principal.
//
// It returns auth.ErrNoCredential when r carries no gateway API key at
// all (so a Chain should try the next Authenticator), and auth.ErrInvalid
// -- wrapped with an internal reason, for logs only; never surfaced to
// the caller -- when the key is unknown, revoked, or expired.
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (*auth.Principal, error) {
	plaintext, ok := ParseBearer(r)
	if !ok {
		return nil, auth.ErrNoCredential
	}

	hash := Hash(plaintext)
	prefix := credentialPrefix(plaintext)

	// A prefix locked for too many failed guesses serves only keys we
	// already hold (the legitimate holder keeps working; a guesser gets
	// 429 without a DB lookup per guess).
	guard := a.guard
	if g, ok := ctx.Value(guardCtxKey{}).(CredentialGuard); ok && g != nil {
		guard = g
	}
	var key *store.APIKey
	if guard != nil {
		if locked, retry := guard.CredLocked(prefix); locked {
			cached, ok := a.cachedAnyAge(hash)
			if !ok {
				return nil, &auth.RateLimitedError{RetryAfter: retry}
			}
			key = cached
		}
	}
	if key == nil {
		var err error
		key, err = a.lookup(ctx, hash)
		if err != nil {
			if guard != nil && errors.Is(err, errUnknownKey) {
				guard.ReportCredFailure(prefix)
			}
			return nil, err
		}
	}

	now := a.now()
	if key.RevokedAt != nil {
		return nil, fmt.Errorf("%w: key revoked", auth.ErrInvalid)
	}
	if key.ExpiresAt != nil && !now.Before(*key.ExpiresAt) {
		return nil, fmt.Errorf("%w: key expired", auth.ErrInvalid)
	}

	a.scheduleTouch(key.ID, now)

	profileID := ""
	if key.ProfileID != nil {
		profileID = *key.ProfileID
	}
	return &auth.Principal{
		ProfileID:     profileID,
		Subject:       key.ID,
		TenantID:      key.TenantID,
		Email:         "",
		Roles:         auth.RolesForKey(key.Role),
		AuthMethod:    "apikey",
		KeyID:         key.ID,
		RawCredential: plaintext,
	}, nil
}

// Invalidate evicts hash from the lookup caches, so the next Authenticate
// for that key re-fetches it from the store instead of serving a stale
// cached row. Called from the key revoke/rotate path (this process) and by
// the store's revocation listener (every other process).
func (a *Authenticator) Invalidate(hash string) {
	a.mu.Lock()
	delete(a.cache, hash)
	a.mu.Unlock()
	a.neg.remove(hash)
}

// Flush drops every cached lookup. The revocation listener calls it when
// its connection (re)establishes, because notifications sent while it was
// down are lost.
func (a *Authenticator) Flush() {
	a.mu.Lock()
	a.cache = make(map[string]cacheEntry)
	a.mu.Unlock()
	a.neg.clear()
}

// cachedAnyAge returns hash's cached key regardless of TTL.
func (a *Authenticator) cachedAnyAge(hash string) (*store.APIKey, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.cache[hash]
	return e.key, ok
}

// InvalidateKey adapts Invalidate to handlers.KeyInvalidator's method
// name (internal/api/handlers.KeyInvalidator), so cmd/gateway/main.go can
// pass this Authenticator straight through to api.Deps.KeyInvalidator
// with no wrapper type.
func (a *Authenticator) InvalidateKey(hash string) {
	a.Invalidate(hash)
}

// errUnknownKey is the (internal-reason) failure for a key hash the store
// does not know.
var errUnknownKey = fmt.Errorf("%w: unknown key", auth.ErrInvalid)

// credentialPrefix is the part of a presented key used as the second
// limiter dimension: the same slice the store keeps as APIKey.KeyPrefix.
func credentialPrefix(plaintext string) string {
	if len(plaintext) > PrefixDisplayLen {
		return plaintext[:PrefixDisplayLen]
	}
	return plaintext
}

// negativeCache remembers key hashes the store does not know, bounded (LRU)
// and short-lived. A nil *negativeCache is a valid, always-miss cache.
type negativeCache struct {
	mu  sync.Mutex
	max int
	ttl time.Duration
	ll  *list.List // front = most recently used; values are *negEntry
	idx map[string]*list.Element
}

type negEntry struct {
	hash    string
	expires time.Time
}

func newNegativeCache(max int, ttl time.Duration) *negativeCache {
	return &negativeCache{max: max, ttl: ttl, ll: list.New(), idx: make(map[string]*list.Element)}
}

func (n *negativeCache) has(hash string, now time.Time) bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	el, ok := n.idx[hash]
	if !ok {
		return false
	}
	if !now.Before(el.Value.(*negEntry).expires) {
		n.ll.Remove(el)
		delete(n.idx, hash)
		return false
	}
	n.ll.MoveToFront(el)
	return true
}

func (n *negativeCache) add(hash string, now time.Time) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if el, ok := n.idx[hash]; ok {
		el.Value.(*negEntry).expires = now.Add(n.ttl)
		n.ll.MoveToFront(el)
		return
	}
	n.idx[hash] = n.ll.PushFront(&negEntry{hash: hash, expires: now.Add(n.ttl)})
	for n.ll.Len() > n.max {
		last := n.ll.Back()
		n.ll.Remove(last)
		delete(n.idx, last.Value.(*negEntry).hash)
	}
}

func (n *negativeCache) remove(hash string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if el, ok := n.idx[hash]; ok {
		n.ll.Remove(el)
		delete(n.idx, hash)
	}
}

func (n *negativeCache) clear() {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ll.Init()
	n.idx = make(map[string]*list.Element)
}

// lookup resolves hash to an APIKey via the in-memory cache, falling back
// to the store on a miss or an expired cache entry.
func (a *Authenticator) lookup(ctx context.Context, hash string) (*store.APIKey, error) {
	now := a.now()

	a.mu.Lock()
	if entry, found := a.cache[hash]; found && now.Before(entry.expiresAt) {
		a.mu.Unlock()
		return entry.key, nil
	}
	a.mu.Unlock()

	if a.neg.has(hash, now) {
		return nil, errUnknownKey
	}

	key, err := a.keys.GetByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.neg.add(hash, now)
			return nil, errUnknownKey
		}
		return nil, fmt.Errorf("apikey: lookup key: %w", err)
	}

	a.mu.Lock()
	a.cache[hash] = cacheEntry{key: key, expiresAt: now.Add(a.cacheTTL)}
	a.mu.Unlock()

	return key, nil
}

// scheduleTouch asynchronously persists a last-used-at touch for keyID,
// throttled to at most once per TouchInterval. It never blocks the
// request path: the throttle check is a quick in-memory map lookup, and
// the actual store write happens on the background worker, fed
// non-blockingly (dropped if the queue is full).
func (a *Authenticator) scheduleTouch(keyID string, now time.Time) {
	a.touchMu.Lock()
	if last, ok := a.lastTouch[keyID]; ok && now.Sub(last) < a.touchInterval {
		a.touchMu.Unlock()
		return
	}
	a.lastTouch[keyID] = now
	a.touchMu.Unlock()

	select {
	case a.touchCh <- touchRequest{id: keyID, at: now}:
	default:
		// Queue full: last_used_at is best-effort telemetry, not
		// correctness-critical, so drop rather than block.
		slog.Default().Warn("apikey: touch queue full, dropping last_used_at update", "key_id", keyID)
	}
}

// runTouchWorker is the single background worker that persists touch
// requests, serialized so it never issues more than one concurrent write
// per Authenticator.
func (a *Authenticator) runTouchWorker() {
	for req := range a.touchCh {
		ctx, cancel := context.WithTimeout(context.Background(), touchTimeout)
		if err := a.keys.TouchLastUsed(ctx, req.id, req.at); err != nil {
			slog.Default().Warn("apikey: touch last_used_at failed", "key_id", req.id, "error", err)
		}
		cancel()
	}
}
