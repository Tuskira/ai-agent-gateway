// Package invalidate keeps a gateway process's caches in step with writes
// made by ANY gateway process: it subscribes to the store's optional
// store.InvalidationNotifier and evicts what each announced write may have
// changed.
package invalidate

import (
	"context"
	"log/slog"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const (
	backoffMin = 500 * time.Millisecond
	backoffMax = 30 * time.Second
	// stableAfter is how long a subscription must have stayed up for its
	// next failure to restart the backoff from the minimum.
	stableAfter = 30 * time.Second
)

// Target is what a process evicts from.
type Target interface {
	// Apply evicts what ev may have changed.
	Apply(ev store.Invalidation)
	// Flush evicts everything: events published while the subscription was
	// down are lost, so it is called each time it (re)establishes.
	Flush()
}

// Listen subscribes through n and applies every event to t until ctx is done,
// reconnecting with exponential backoff. If the subscription cannot be kept
// up, caches still expire on their own TTLs; the listener only makes a change
// prompt.
func Listen(ctx context.Context, n store.InvalidationNotifier, t Target, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	backoff := backoffMin
	for ctx.Err() == nil {
		started := time.Now()
		err := n.ListenInvalidations(ctx, func() {
			t.Flush()
			logger.Info("cache invalidation listener ready")
		}, t.Apply)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= stableAfter {
			backoff = backoffMin
		}
		logger.Warn("cache invalidation listener lost; retrying (caches fall back to their TTLs meanwhile)",
			"error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// Targets fans one event out to several targets.
type Targets []Target

// Apply implements Target.
func (ts Targets) Apply(ev store.Invalidation) {
	for _, t := range ts {
		t.Apply(ev)
	}
}

// Flush implements Target.
func (ts Targets) Flush() {
	for _, t := range ts {
		t.Flush()
	}
}

// Funcs adapts a pair of functions to a Target. Either may be nil.
type Funcs struct {
	ApplyFunc func(store.Invalidation)
	FlushFunc func()
}

// Apply implements Target.
func (f Funcs) Apply(ev store.Invalidation) {
	if f.ApplyFunc != nil {
		f.ApplyFunc(ev)
	}
}

// Flush implements Target.
func (f Funcs) Flush() {
	if f.FlushFunc != nil {
		f.FlushFunc()
	}
}

// Caches is the Target for a whole gateway process. Every field is
// optional: a process without that cache leaves it nil.
type Caches struct {
	// Profiles is the data plane (dataplane.Plane): resolved profile
	// allow-lists, bound profiles and skill bundles.
	Profiles Target
	// Secrets is the secret store service's decrypted-field cache
	// (secrets.Service).
	Secrets TenantCache
	// External is the external credential service provider's value cache
	// (secrets.ExternalServiceProvider).
	External TenantCache
	// Keys is the API-key lookup cache (apikey.Authenticator).
	Keys KeyCache
}

// TenantCache is a cache that can be emptied per tenant or entirely.
type TenantCache interface {
	InvalidateTenant(tenantID string)
	InvalidateAll()
}

// KeyCache is the API-key lookup cache.
type KeyCache interface {
	Invalidate(keyHash string)
	Flush()
}

var _ Target = Caches{}

// Apply implements Target.
//
//   - profile: the data plane's profile caches.
//   - credential, source: the secret store's decrypted fields and the
//     external service's values, for the tenant. A source names the
//     credential that authenticates to the service, and a credential may
//     back several sources, so both are dropped tenant-wide rather than
//     tracing which entries depend on which row.
//   - key: that key's lookup entry.
func (c Caches) Apply(ev store.Invalidation) {
	switch ev.Kind {
	case store.InvalidateProfile:
		if c.Profiles != nil {
			c.Profiles.Apply(ev)
		}
	case store.InvalidateCredential, store.InvalidateSource:
		if ev.Tenant == "" {
			c.Flush()
			return
		}
		if c.Secrets != nil {
			c.Secrets.InvalidateTenant(ev.Tenant)
		}
		if c.External != nil {
			c.External.InvalidateTenant(ev.Tenant)
		}
	case store.InvalidateKey:
		if c.Keys != nil && ev.ID != "" {
			c.Keys.Invalidate(ev.ID)
		}
	}
}

// Flush implements Target.
func (c Caches) Flush() {
	if c.Profiles != nil {
		c.Profiles.Flush()
	}
	if c.Secrets != nil {
		c.Secrets.InvalidateAll()
	}
	if c.External != nil {
		c.External.InvalidateAll()
	}
	if c.Keys != nil {
		c.Keys.Flush()
	}
}
