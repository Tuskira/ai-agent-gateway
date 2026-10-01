package auth

import (
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/clientip"
)

// Clock abstracts wall-clock time so tests can drive RateLimiter's window/
// lockout/eviction logic deterministically instead of racing real time.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Default tuning applied by NewRateLimiter when the corresponding
// RateLimiterConfig field is left at its zero value. These mirror
// internal/config's documented defaults for auth.rate_limit; config.Default
// is the source of truth for what ships in a real deployment, these are
// just a safety net for callers (tests, mainly) that construct a
// RateLimiter directly.
const (
	DefaultMaxFailures      = 10
	DefaultWindow           = time.Minute
	DefaultLockout          = 5 * time.Minute
	DefaultMaxEntries       = 10000
	DefaultCredMaxFailures  = 20
	DefaultCredWindow       = 5 * time.Minute
	defaultEvictionInterval = time.Minute
	minBurst, maxBurst      = 5, 50
	burstDivisor            = 20 // requests_per_minute / burstDivisor, clamped to [minBurst, maxBurst]
)

// RateLimiterConfig configures a RateLimiter.
type RateLimiterConfig struct {
	// Plane names the plane this limiter serves ("api", "mcp", "llm"); it
	// only labels log lines. Each plane gets its OWN RateLimiter, so
	// failures on one plane never lock a client out of another.
	Plane string

	// Enabled turns rate limiting on. When false, every RateLimiter method
	// is a no-op (Allowed/Locked always report "allow", Report* do
	// nothing) so callers can wire a RateLimiter unconditionally and let
	// this flag gate behavior.
	Enabled bool

	// MaxFailures is how many auth failures from the same IP within
	// Window trip the lockout. Zero is replaced with DefaultMaxFailures.
	MaxFailures int
	// Window is the sliding-ish (fixed-window) period failures are
	// counted over. Zero is replaced with DefaultWindow.
	Window time.Duration
	// Lockout is how long a tripped IP is locked out for once
	// MaxFailures is reached. Zero is replaced with DefaultLockout.
	Lockout time.Duration

	// RequestsPerMinute is the general per-IP request cap on the control
	// plane. Zero disables the general cap entirely (the failure lockout
	// still applies).
	RequestsPerMinute int

	// CredMaxFailures is how many failed lookups of an UNKNOWN key sharing
	// one credential prefix (across all client IPs) within CredWindow lock
	// that prefix. This is the second limiter dimension: it bounds guessing
	// of one key from a rotating pool of IPs, without touching any client
	// IP's own standing. Zero is replaced with DefaultCredMaxFailures.
	CredMaxFailures int
	// CredWindow is the period CredMaxFailures is counted over. Zero is
	// replaced with DefaultCredWindow. A locked prefix stays locked for
	// Lockout.
	CredWindow time.Duration

	// Resolver derives the client IP (trusted-proxy aware X-Forwarded-For
	// handling). Nil means no proxy is trusted: RemoteAddr only. Share ONE
	// Resolver across every plane.
	Resolver *clientip.Resolver

	// TrustedProxies is a convenience for callers that have raw config
	// strings (IPs or CIDRs): used to build a Resolver when Resolver is nil.
	// Entries that fail to parse are skipped with a warning; config.Load
	// rejects them earlier in a real deployment.
	TrustedProxies []string

	// MaxEntries bounds the number of tracked IPs; once reached, the
	// oldest (least-recently-seen) entries are evicted to make room for a
	// new one. Zero is replaced with DefaultMaxEntries.
	MaxEntries int
	// IdleTTL is how long an IP's state survives with no activity before
	// the background sweep evicts it. Zero is replaced with 10x Lockout
	// (or 10 minutes, whichever is larger).
	IdleTTL time.Duration
	// EvictionInterval is how often the background sweep runs. Zero is
	// replaced with defaultEvictionInterval.
	EvictionInterval time.Duration

	Logger *slog.Logger
	// Clock is used for all time reads (window/lockout/eviction). Nil
	// defaults to the real wall clock.
	Clock Clock
}

// ipState is one tracked IP's failure-lockout and general-cap bookkeeping.
// Every field is guarded by RateLimiter.mu -- there is no per-entry lock,
// which keeps this simple at the control plane's expected scale.
type ipState struct {
	// Failure lockout.
	failures      int
	windowStart   time.Time
	lockedUntil   time.Time
	lockoutLogged bool // true once the current lockout has been WARN-logged

	// General per-IP token bucket.
	tokens     float64
	lastRefill time.Time

	lastSeen time.Time
}

// RateLimiter is an in-memory, per-client-IP limiter for ONE plane (api,
// mcp or llm: main.go builds one per plane, so counters and lockouts are
// independent across planes).
// It enforces two independent policies (see RateLimiterConfig):
//
//   - a failure-triggered lockout, to slow down credential guessing: once
//     an IP racks up MaxFailures auth failures inside Window, every request
//     from it (on a non-exempt route) gets 429 for Lockout;
//   - a general requests-per-minute cap with a small burst, to slow down
//     any other abusive client, independent of whether its requests
//     authenticate.
//
// Memory is bounded two ways: idle entries are swept on a ticker, and the
// tracked-IP count is capped (oldest entries evicted first) so a very wide
// scan can't grow the map without bound between sweeps.
//
// The zero value is not usable; construct with NewRateLimiter.
type RateLimiter struct {
	cfg    RateLimiterConfig
	clock  Clock
	logger *slog.Logger

	rate  float64 // tokens/sec refill rate for the general cap; 0 if disabled
	burst int     // general cap bucket capacity

	resolver *clientip.Resolver

	mu    sync.Mutex
	ips   map[string]*ipState
	creds map[string]*ipState // failure lockout per credential prefix

	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewRateLimiter builds a RateLimiter from cfg, applying defaults for any
// zero-valued tuning field, and -- when cfg.Enabled -- starts the
// background idle-eviction sweep. Callers should defer Close() to stop
// that goroutine on shutdown.
func NewRateLimiter(cfg RateLimiterConfig) *RateLimiter {
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = DefaultMaxFailures
	}
	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}
	if cfg.Lockout <= 0 {
		cfg.Lockout = DefaultLockout
	}
	if cfg.CredMaxFailures <= 0 {
		cfg.CredMaxFailures = DefaultCredMaxFailures
	}
	if cfg.CredWindow <= 0 {
		cfg.CredWindow = DefaultCredWindow
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = DefaultMaxEntries
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = max(cfg.Lockout*10, 10*time.Minute)
	}
	if cfg.EvictionInterval <= 0 {
		cfg.EvictionInterval = defaultEvictionInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	clock := cfg.Clock
	if clock == nil {
		clock = realClock{}
	}

	resolver := cfg.Resolver
	if resolver == nil && len(cfg.TrustedProxies) > 0 {
		var err error
		if resolver, err = clientip.New(cfg.TrustedProxies); err != nil {
			cfg.Logger.Warn("rate limiter: ignoring invalid trusted_proxies", "error", err)
			resolver = nil
		}
	}

	rl := &RateLimiter{
		cfg:      cfg,
		clock:    clock,
		logger:   cfg.Logger,
		resolver: resolver,
		ips:      make(map[string]*ipState),
		creds:    make(map[string]*ipState),
		stopCh:   make(chan struct{}),
	}

	if cfg.RequestsPerMinute > 0 {
		rl.rate = float64(cfg.RequestsPerMinute) / 60.0
		rl.burst = min(max(cfg.RequestsPerMinute/burstDivisor, minBurst), maxBurst)
	}

	if cfg.Enabled {
		go rl.evictionLoop()
	}

	return rl
}

// Close stops the background eviction sweep. Safe to call more than once,
// and safe to call on a RateLimiter built with Enabled: false (the loop was
// never started).
func (rl *RateLimiter) Close() {
	if rl == nil {
		return
	}
	rl.stopOnce.Do(func() { close(rl.stopCh) })
}

// Enabled reports whether rl actively enforces its policies.
func (rl *RateLimiter) Enabled() bool {
	return rl != nil && rl.cfg.Enabled
}

// ClientIP returns the IP the limiter should key r on, via the shared
// clientip.Resolver (see that package for the trusted-proxy rules).
func (rl *RateLimiter) ClientIP(r *http.Request) string {
	if rl == nil {
		return (*clientip.Resolver)(nil).IP(r)
	}
	return rl.resolver.IP(r)
}

// Locked reports whether ip is currently under a failure lockout, and if
// so, how long until it expires (for the Retry-After header). A disabled
// limiter never locks anyone out.
func (rl *RateLimiter) Locked(ip string) (locked bool, retryAfter time.Duration) {
	if !rl.Enabled() {
		return false, 0
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	st := rl.getOrCreateLocked(ip)
	now := rl.clock.Now()
	if st.lockedUntil.After(now) {
		return true, st.lockedUntil.Sub(now)
	}
	return false, 0
}

// AllowGeneral applies the general per-IP requests-per-minute cap (a token
// bucket with a small burst) to ip, consuming one token on success. It
// always reports allowed when the limiter is disabled or the cap itself is
// disabled (RequestsPerMinute <= 0).
func (rl *RateLimiter) AllowGeneral(ip string) (allowed bool, retryAfter time.Duration) {
	if !rl.Enabled() || rl.rate <= 0 {
		return true, 0
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	st := rl.getOrCreateLocked(ip)
	now := rl.clock.Now()

	if elapsed := now.Sub(st.lastRefill).Seconds(); elapsed > 0 {
		st.tokens = min(st.tokens+elapsed*rl.rate, float64(rl.burst))
		st.lastRefill = now
	}

	if st.tokens < 1 {
		wait := (1 - st.tokens) / rl.rate
		return false, time.Duration(wait * float64(time.Second))
	}
	st.tokens--
	return true, 0
}

// ReportFailure records an auth failure from ip. Once MaxFailures failures
// have accumulated within Window, ip is locked out for Lockout and a WARN
// is logged once for that lockout (not on every subsequent request).
func (rl *RateLimiter) ReportFailure(ip string) {
	if !rl.Enabled() {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	st := rl.getOrCreateLocked(ip)
	now := rl.clock.Now()

	if st.windowStart.IsZero() || now.Sub(st.windowStart) >= rl.cfg.Window {
		st.windowStart = now
		st.failures = 0
	}
	st.failures++

	if st.failures >= rl.cfg.MaxFailures {
		st.lockedUntil = now.Add(rl.cfg.Lockout)
		if !st.lockoutLogged {
			rl.logger.Warn("auth failure rate limit: lockout started",
				"plane", rl.cfg.Plane, "ip", ip, "failures", st.failures, "window", rl.cfg.Window, "lockout", rl.cfg.Lockout)
			st.lockoutLogged = true
		}
	}
}

// ReportSuccess resets ip's failure counter. It does not clear an
// already-tripped lockout: in practice a request can't reach the
// authenticator (and therefore can't succeed) while its IP is locked out,
// since RateLimitMiddleware rejects it first -- see middleware.go.
func (rl *RateLimiter) ReportSuccess(ip string) {
	if !rl.Enabled() {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	st, ok := rl.ips[ip]
	if !ok {
		return
	}
	st.failures = 0
	st.windowStart = time.Time{}
	st.lockoutLogged = false
	st.lastSeen = rl.clock.Now()
}

// CredLocked reports whether credential prefix key is locked because too
// many distinct guesses at it failed (from any IP). See CredMaxFailures.
func (rl *RateLimiter) CredLocked(key string) (locked bool, retryAfter time.Duration) {
	if !rl.Enabled() || key == "" {
		return false, 0
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	st, ok := rl.creds[key]
	if !ok {
		return false, 0
	}
	now := rl.clock.Now()
	if st.lockedUntil.After(now) {
		return true, st.lockedUntil.Sub(now)
	}
	return false, 0
}

// ReportCredFailure records a failed guess at credential prefix key.
func (rl *RateLimiter) ReportCredFailure(key string) {
	if !rl.Enabled() || key == "" {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.clock.Now()
	st, ok := rl.creds[key]
	if !ok {
		if len(rl.creds) >= rl.cfg.MaxEntries {
			rl.evictOldestCredsLocked(max(rl.cfg.MaxEntries/20, 1))
		}
		st = &ipState{}
		rl.creds[key] = st
	}
	st.lastSeen = now

	if st.windowStart.IsZero() || now.Sub(st.windowStart) >= rl.cfg.CredWindow {
		st.windowStart = now
		st.failures = 0
	}
	st.failures++

	if st.failures >= rl.cfg.CredMaxFailures {
		st.lockedUntil = now.Add(rl.cfg.Lockout)
		if !st.lockoutLogged {
			rl.logger.Warn("auth failure rate limit: credential lockout started",
				"plane", rl.cfg.Plane, "credential_prefix", key, "failures", st.failures, "window", rl.cfg.CredWindow, "lockout", rl.cfg.Lockout)
			st.lockoutLogged = true
		}
	}
}

// LockedIPCount returns how many IPs are currently under a failure lockout.
// Used to fill GET /api/v1/health's "rate_limit":{"locked_ips":n}.
func (rl *RateLimiter) LockedIPCount() int {
	if !rl.Enabled() {
		return 0
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.clock.Now()
	n := 0
	for _, st := range rl.ips {
		if st.lockedUntil.After(now) {
			n++
		}
	}
	return n
}

// getOrCreateLocked returns ip's state, creating it (and, if the tracked-IP
// count is at cfg.MaxEntries, evicting the oldest entries first) if
// necessary. Callers must hold rl.mu.
func (rl *RateLimiter) getOrCreateLocked(ip string) *ipState {
	now := rl.clock.Now()

	if st, ok := rl.ips[ip]; ok {
		st.lastSeen = now
		return st
	}

	if len(rl.ips) >= rl.cfg.MaxEntries {
		rl.evictOldestLocked(max(rl.cfg.MaxEntries/20, 1))
	}

	st := &ipState{
		lastSeen:   now,
		lastRefill: now,
		tokens:     float64(rl.burst),
	}
	rl.ips[ip] = st
	return st
}

// evictIdle removes every tracked IP that has been inactive for longer
// than cfg.IdleTTL and is not currently locked out (an active lockout is
// activity worth remembering until it expires, so LockedIPCount and a
// concurrent request against that IP stay correct).
func (rl *RateLimiter) evictIdle() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.clock.Now()
	for ip, st := range rl.ips {
		if now.Sub(st.lastSeen) > rl.cfg.IdleTTL && !st.lockedUntil.After(now) {
			delete(rl.ips, ip)
		}
	}
	for k, st := range rl.creds {
		if now.Sub(st.lastSeen) > rl.cfg.IdleTTL && !st.lockedUntil.After(now) {
			delete(rl.creds, k)
		}
	}
}

// evictOldestCredsLocked removes the n least-recently-seen credential
// entries. Callers must hold rl.mu.
func (rl *RateLimiter) evictOldestCredsLocked(n int) {
	type entry struct {
		key      string
		lastSeen time.Time
	}
	entries := make([]entry, 0, len(rl.creds))
	for k, st := range rl.creds {
		entries = append(entries, entry{k, st.lastSeen})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].lastSeen.Before(entries[j].lastSeen) })
	for i := 0; i < n && i < len(entries); i++ {
		delete(rl.creds, entries[i].key)
	}
}

// evictOldestLocked removes the n least-recently-seen entries. Callers
// must hold rl.mu. Only invoked when the map is at cfg.MaxEntries, so the
// O(n log n) sort is a rare, bounded (MaxEntries-sized) cost, not a
// per-request one.
func (rl *RateLimiter) evictOldestLocked(n int) {
	type entry struct {
		ip       string
		lastSeen time.Time
	}
	entries := make([]entry, 0, len(rl.ips))
	for ip, st := range rl.ips {
		entries = append(entries, entry{ip, st.lastSeen})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].lastSeen.Before(entries[j].lastSeen) })

	for i := 0; i < n && i < len(entries); i++ {
		delete(rl.ips, entries[i].ip)
	}
}

// evictionLoop periodically sweeps idle entries until Close is called.
func (rl *RateLimiter) evictionLoop() {
	ticker := time.NewTicker(rl.cfg.EvictionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case <-ticker.C:
			rl.evictIdle()
		}
	}
}
