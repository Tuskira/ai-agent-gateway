package llmplane

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

// concurrency caps the in-flight requests of one tenant
// (llm_proxy.limits.max_concurrent_per_tenant). acquire either takes a
// slot -- ok is true and release frees it, exactly once, when the request
// is done -- or refuses (ok false, release nil). err reports that the
// store itself failed; neither ok nor release is meaningful then.
type concurrency interface {
	acquire(ctx context.Context, tenant string) (release func(), ok bool, err error)
}

// newConcurrency builds the limiter the router consults. limit <= 0 is no
// limit (nil). A nil Redis client keeps the count in this process, so the
// cap is per pod; a client makes it cluster-wide, with the in-process
// count as the fallback while Redis is unreachable.
func newConcurrency(limit int, rdb goredis.UniversalClient, lease time.Duration) concurrency {
	if limit <= 0 {
		return nil
	}
	mem := newMemoryConcurrency(limit)
	if rdb == nil {
		return mem
	}
	f := &fallbackConcurrency{primary: newRedisConcurrency(rdb, limit, lease), local: mem}
	f.primary.warn = f.warn // one rate limit for every Redis warning
	return f
}

// ---------------------------------------------------------------------------
// In process
// ---------------------------------------------------------------------------

// memoryConcurrency counts in this process only: one buffered channel per
// tenant (an unset tenant shares one bucket). With N replicas the real cap
// is N times the limit.
type memoryConcurrency struct {
	mu    sync.Mutex
	sem   map[string]chan struct{}
	limit int
}

func newMemoryConcurrency(limit int) *memoryConcurrency {
	return &memoryConcurrency{sem: make(map[string]chan struct{}), limit: limit}
}

func (m *memoryConcurrency) chanFor(tenant string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := m.sem[tenant]
	if ch == nil {
		ch = make(chan struct{}, m.limit)
		m.sem[tenant] = ch
	}
	return ch
}

func (m *memoryConcurrency) acquire(_ context.Context, tenant string) (func(), bool, error) {
	ch := m.chanFor(tenant)
	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-ch }) }, true, nil
	default:
		return nil, false, nil
	}
}

// ---------------------------------------------------------------------------
// Redis (cluster-wide)
// ---------------------------------------------------------------------------

const (
	// concurrencyKeyPrefix namespaces the per-tenant lease sets; the
	// session driver's keys live under gw:sess:.
	concurrencyKeyPrefix = "gw:llm:conc:"
	// DefaultConcurrencyLeaseTTL is how long a slot outlives the last
	// renewal of a request that stopped renewing (its pod died).
	DefaultConcurrencyLeaseTTL = 30 * time.Second
	// concurrencyOpTimeout bounds each Redis round trip, so an unreachable
	// Redis delays a request by at most this much before the fallback.
	concurrencyOpTimeout = 500 * time.Millisecond
	// concurrencyReleaseTimeout bounds the release, which runs on a fresh
	// context: the request's own may already be cancelled.
	concurrencyReleaseTimeout = 2 * time.Second
)

// acquireScript takes a slot for ARGV[2] when the tenant's lease set
// (KEYS[1], a sorted set of lease ids scored by expiry in ms) holds fewer
// than ARGV[1] live leases. Time is Redis's own (TIME), so the pods'
// clocks never matter. Expired leases -- those of a request whose pod died
// -- are dropped first. The key's own expiry follows the newest lease, so
// an idle tenant leaves nothing behind. Returns 1 when taken, 0 when full.
var acquireScript = goredis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local ttl = tonumber(ARGV[3])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[1]) then
  return 0
end
redis.call('ZADD', KEYS[1], now + ttl, ARGV[2])
redis.call('PEXPIRE', KEYS[1], ttl)
return 1
`)

// renewScript pushes lease ARGV[1]'s expiry out by ARGV[2] ms. A lease
// that was swept meanwhile (Redis was unreachable longer than the lease)
// is added back: the request is still running and must be counted.
var renewScript = goredis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local ttl = tonumber(ARGV[2])
redis.call('ZADD', KEYS[1], now + ttl, ARGV[1])
redis.call('PEXPIRE', KEYS[1], ttl)
return 1
`)

// redisConcurrency is the cluster-wide count: every llm pod takes and
// frees leases in one shared sorted set per tenant. A request renews its
// lease every lease/3 for as long as it runs (a 30-minute stream keeps its
// slot), and a crashed pod's leases simply expire.
type redisConcurrency struct {
	rdb   goredis.UniversalClient
	limit int
	lease time.Duration
	// warn reports a failed renewal or release; set by the fallback so
	// all Redis warnings share one rate limit.
	warn func(msg, tenant string, err error)
}

func newRedisConcurrency(rdb goredis.UniversalClient, limit int, lease time.Duration) *redisConcurrency {
	if lease <= 0 {
		lease = DefaultConcurrencyLeaseTTL
	}
	return &redisConcurrency{rdb: rdb, limit: limit, lease: lease, warn: func(msg, tenant string, err error) {
		slog.Warn(msg, "tenant", tenant, "error", err)
	}}
}

func (rc *redisConcurrency) acquire(ctx context.Context, tenant string) (func(), bool, error) {
	key := concurrencyKeyPrefix + tenant
	id := uuid.NewString()
	opCtx, cancel := context.WithTimeout(ctx, concurrencyOpTimeout)
	defer cancel()
	n, err := acquireScript.Run(opCtx, rc.rdb, []string{key}, rc.limit, id, rc.lease.Milliseconds()).Int()
	if err != nil {
		return nil, false, err
	}
	if n != 1 {
		return nil, false, nil
	}

	// Renew until released. release waits for the renewer to stop before
	// removing the lease, so a late renewal can never re-add a freed slot.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(rc.lease / 3)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				rctx, rcancel := context.WithTimeout(context.Background(), concurrencyOpTimeout)
				err := renewScript.Run(rctx, rc.rdb, []string{key}, id, rc.lease.Milliseconds()).Err()
				rcancel()
				if err != nil {
					// The request keeps running; at worst the slot is
					// uncounted until the next renewal succeeds.
					rc.warn("llmplane: tenant concurrency lease renewal failed", tenant, err)
				}
			}
		}
	}()

	var once sync.Once
	release := func() {
		once.Do(func() {
			close(stop)
			<-done
			rctx, rcancel := context.WithTimeout(context.Background(), concurrencyReleaseTimeout)
			defer rcancel()
			if err := rc.rdb.ZRem(rctx, key, id).Err(); err != nil {
				// The lease expires on its own after the lease time.
				rc.warn("llmplane: tenant concurrency release failed", tenant, err)
			}
		})
	}
	return release, true, nil
}

// ---------------------------------------------------------------------------
// Fallback
// ---------------------------------------------------------------------------

const (
	// concurrencyWarnEvery rate-limits the "Redis unreachable" warning.
	concurrencyWarnEvery = 30 * time.Second
	// concurrencyRetryAfter is how long after a Redis error the limiter
	// goes straight to the in-process count, so an outage costs each
	// request nothing rather than an op timeout.
	concurrencyRetryAfter = 5 * time.Second
)

// fallbackConcurrency uses Redis and, while Redis fails, the in-process
// count with the same limit: the cap is per pod again for the outage, but
// it is never lifted and Redis never blocks LLM traffic.
type fallbackConcurrency struct {
	primary  *redisConcurrency
	local    *memoryConcurrency
	lastWarn atomic.Int64 // unix nanos of the last warning
	skipTill atomic.Int64 // unix nanos until which Redis is not tried
}

// warn logs at most once per concurrencyWarnEvery.
func (f *fallbackConcurrency) warn(msg, tenant string, err error) {
	now := time.Now().UnixNano()
	last := f.lastWarn.Load()
	if now-last < int64(concurrencyWarnEvery) || !f.lastWarn.CompareAndSwap(last, now) {
		return
	}
	slog.Warn(msg, "tenant", tenant, "error", err)
}

func (f *fallbackConcurrency) acquire(ctx context.Context, tenant string) (func(), bool, error) {
	if time.Now().UnixNano() >= f.skipTill.Load() {
		release, ok, err := f.primary.acquire(ctx, tenant)
		if err == nil {
			return release, ok, nil
		}
		// A request the client already abandoned says nothing about Redis.
		if ctx.Err() == nil {
			f.skipTill.Store(time.Now().Add(concurrencyRetryAfter).UnixNano())
			f.warn("llmplane: tenant concurrency store (redis) unreachable; using the per-pod limit", tenant, err)
		}
	}
	return f.local.acquire(ctx, tenant)
}
