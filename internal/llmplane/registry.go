package llmplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// registryTTL is how long a resolved model name is cached per tenant
// (hits and misses alike). Short enough that a registry edit takes effect
// in seconds even on a replica the API plane did not tell (see
// Registry.Invalidate), long enough that a chatty agent does not query
// Postgres on every call. Same shape as internal/dataplane/profile's cache.
const registryTTL = 30 * time.Second

// CredentialSource resolves a tenant's named credential to its decrypted
// fields. *internal/secrets.Service satisfies it; the plane depends only
// on this method so it imports neither the secret store nor its key ring.
type CredentialSource interface {
	Get(ctx context.Context, tenantID, name string) (map[string]string, error)
}

// Registry resolves the "model" a client asked for against the tenant's
// model registry (pkg/store.ModelStore): the tenant's own row first, then
// the platform default of that name. A name with no row is a MISS, and the
// router forwards the request byte-for-byte as it always has; a hit swaps
// the upstream host, credential, region and model id (see resolve.go).
//
// A nil *Registry is valid and resolves nothing -- the plane runs that way
// when no store is wired (tests, or a deployment that never registered a
// model).
type Registry struct {
	models store.ModelStore
	creds  CredentialSource

	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	cache map[string]registryEntry // tenant|name -> entry
}

// registryEntry caches a lookup result. model nil is a negative entry (the
// name is not registered), cached like a hit so an unregistered vendor
// model id -- the common case for every call today -- costs one query per
// TTL per tenant, not one per call.
type registryEntry struct {
	model     *store.Model
	expiresAt time.Time
}

// RegistryOptions configures a Registry.
type RegistryOptions struct {
	// TTL is how long a resolution is cached. Zero uses registryTTL (30s).
	TTL time.Duration
	// Now returns the current time; tests override it.
	Now func() time.Time
}

// NewRegistry returns a Registry over the model store and credential
// source. creds may be nil when no target will ever name a credential (a
// target that does then fails its build with a clear error).
func NewRegistry(models store.ModelStore, creds CredentialSource, opts RegistryOptions) *Registry {
	if opts.TTL <= 0 {
		opts.TTL = registryTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Registry{
		models: models,
		creds:  creds,
		ttl:    opts.TTL,
		now:    opts.Now,
		cache:  make(map[string]registryEntry),
	}
}

// Resolve returns the registry row name resolves to for tenantID, or
// (nil, nil) when the name is not registered. An error is returned only
// when the store itself failed -- the router turns that into a 502, never
// into a silent passthrough that would send an alias to the wrong vendor.
func (r *Registry) Resolve(ctx context.Context, tenantID, name string) (*store.Model, error) {
	if r == nil || name == "" {
		return nil, nil
	}
	key := tenantID + "|" + name

	now := r.now()
	r.mu.Lock()
	if e, ok := r.cache[key]; ok && now.Before(e.expiresAt) {
		r.mu.Unlock()
		return e.model, nil
	}
	r.mu.Unlock()

	m, err := r.models.GetByName(ctx, tenantID, name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("model registry: look up %q: %w", name, err)
	}
	// ErrNotFound -> m is nil -> negative entry.

	r.mu.Lock()
	r.cache[key] = registryEntry{model: m, expiresAt: now.Add(r.ttl)}
	r.mu.Unlock()
	return m, nil
}

// Invalidate drops every cached resolution for tenantID, so a registry
// write takes effect before the TTL lapses on the replica that served
// the write (internal/api/handlers.Models calls it; other replicas wait
// out the TTL). tenantID "" -- a platform-row change -- drops every
// tenant's entries, since any of them may have resolved to the platform
// row. Nil-safe.
func (r *Registry) Invalidate(tenantID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if tenantID == "" {
		r.cache = make(map[string]registryEntry)
		return
	}
	prefix := tenantID + "|"
	for k := range r.cache {
		if strings.HasPrefix(k, prefix) {
			delete(r.cache, k)
		}
	}
}

// credential fetches the decrypted fields of the tenant's credential name.
// Never cached here: the secrets service is the one place that knows when
// a credential rotated, and an LLM call is expensive enough that one
// decrypt per call is noise. The values are handed to the request builder
// only; nothing here logs or captures them.
func (r *Registry) credential(ctx context.Context, tenantID, name string) (map[string]string, error) {
	if r.creds == nil {
		return nil, fmt.Errorf("model registry: target names credential %q but the plane has no credential source", name)
	}
	fields, err := r.creds.Get(ctx, tenantID, name)
	if err != nil {
		return nil, fmt.Errorf("model registry: credential %q: %w", name, err)
	}
	return fields, nil
}

// apiKeyOf picks the API key out of a decrypted credential payload: the
// "api_key" field, or the sole field of a single-field credential (so a
// credential created as {"key": "..."} or {"token": "..."} works too).
func apiKeyOf(fields map[string]string) (string, error) {
	if v, ok := fields["api_key"]; ok && v != "" {
		return v, nil
	}
	if len(fields) == 1 {
		for _, v := range fields {
			if v != "" {
				return v, nil
			}
		}
	}
	return "", errors.New("credential has no api_key field")
}
