package auth

import (
	"sync"
	"time"
)

// keyBucket is one API key's token bucket. Every field is guarded by
// KeyRateLimiter.mu.
type keyBucket struct {
	tokens     float64
	lastRefill time.Time
}

// KeyRateLimiter is a simple in-memory, per-API-key requests-per-minute
// limiter with a small burst, used only by POST /api/v1/ingest
// (internal/api/handlers.Ingest): that route is deliberately exempt from
// the control plane's per-IP RateLimiter (see internal/api/router.go's
// isIngestPath) because many interceptor instances behind one office NAT
// share an IP, so per-IP limiting would throttle them together. Limiting
// per API key instead means each interceptor install gets its own budget
// regardless of how many others share its network.
//
// Unlike RateLimiter, this has no idle-eviction sweep: a key only ever
// reaches Allow after authenticating AND passing the "ingest.write"
// permission check (RequirePermission runs first -- see router.go), so the
// tracked-key set is bounded by how many interceptor-role API keys a
// tenant has actually minted, not by arbitrary caller input. That set is
// expected to stay small (one interceptor install per contributor
// laptop), so no bound/sweep is needed to keep memory in check.
//
// The zero value is not usable; construct with NewKeyRateLimiter.
type KeyRateLimiter struct {
	rate  float64 // tokens/sec refill rate
	burst int

	mu   sync.Mutex
	keys map[string]*keyBucket

	clock Clock
}

// NewKeyRateLimiter returns a KeyRateLimiter allowing requestsPerMinute
// requests per key per minute, with a small burst
// (min(requestsPerMinute/burstDivisor, maxBurst), floored at minBurst --
// the same shape RateLimiter's general cap uses). requestsPerMinute <= 0
// disables the limiter entirely: Allow always reports allowed. A nil
// clock uses the real wall clock.
func NewKeyRateLimiter(requestsPerMinute int, clock Clock) *KeyRateLimiter {
	if clock == nil {
		clock = realClock{}
	}
	kl := &KeyRateLimiter{
		keys:  make(map[string]*keyBucket),
		clock: clock,
	}
	if requestsPerMinute > 0 {
		kl.rate = float64(requestsPerMinute) / 60.0
		kl.burst = min(max(requestsPerMinute/burstDivisor, minBurst), maxBurst)
	}
	return kl
}

// Allow consumes one token from keyID's bucket, reporting whether the
// request is allowed and, if not, how long until a token is available
// (for the Retry-After header). A disabled limiter (requestsPerMinute <= 0
// at construction, or a nil receiver) always allows.
func (kl *KeyRateLimiter) Allow(keyID string) (allowed bool, retryAfter time.Duration) {
	if kl == nil || kl.rate <= 0 {
		return true, 0
	}
	kl.mu.Lock()
	defer kl.mu.Unlock()

	now := kl.clock.Now()
	b, ok := kl.keys[keyID]
	if !ok {
		b = &keyBucket{tokens: float64(kl.burst), lastRefill: now}
		kl.keys[keyID] = b
	}

	if elapsed := now.Sub(b.lastRefill).Seconds(); elapsed > 0 {
		b.tokens = min(b.tokens+elapsed*kl.rate, float64(kl.burst))
		b.lastRefill = now
	}

	if b.tokens < 1 {
		wait := (1 - b.tokens) / kl.rate
		return false, time.Duration(wait * float64(time.Second))
	}
	b.tokens--
	return true, 0
}
