package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// defaultFetchCacheTTL is how long Service.Fetcher caches a resolved field
// value before re-fetching (and re-decrypting) it.
const defaultFetchCacheTTL = 5 * time.Minute

// ErrFieldNotFound is returned by GetField (and, wrapped, by the
// CredentialFetcher returned by Fetcher) when the credential's decrypted
// payload doesn't contain the requested field.
var ErrFieldNotFound = errors.New("secrets: field not found")

// Service is the gateway's encrypted secret store: it layers
// encrypt/decrypt, field-level access, rotation, and a short-lived read
// cache on top of a store.CredentialStore (which only ever sees ciphertext)
// and a KeyRing (which never touches the store).
type Service struct {
	store store.CredentialStore
	ring  *KeyRing

	cacheTTL time.Duration
	cacheMu  sync.Mutex
	cache    map[string]cacheEntry
}

type cacheEntry struct {
	value     string
	expiresAt time.Time
}

// NewService builds a Service over cs (a tenant-scoped credential store)
// and ring (the master key ring used to encrypt/decrypt every row).
func NewService(cs store.CredentialStore, ring *KeyRing) *Service {
	return &Service{
		store:    cs,
		ring:     ring,
		cacheTTL: defaultFetchCacheTTL,
		cache:    make(map[string]cacheEntry),
	}
}

// Create JSON-encodes payload, encrypts it under the ring's active key, and
// persists it as a new Credential named name for tenantID. FieldNames is
// set to payload's keys, sorted, so List can show field names without ever
// decrypting. A (tenantID, name) that already exists returns an error
// wrapping store.ErrConflict.
func (s *Service) Create(ctx context.Context, tenantID, name, typ string, payload map[string]string, createdBy string) (*store.Credential, error) {
	ciphertext, nonce, err := s.seal(payload)
	if err != nil {
		return nil, err
	}

	cred := &store.Credential{
		TenantID:   tenantID,
		Name:       name,
		Type:       typ,
		Ciphertext: ciphertext,
		Nonce:      nonce,
		KeyID:      s.ring.ActiveKeyID,
		FieldNames: fieldNames(payload),
		CreatedBy:  createdBy,
	}
	if err := s.store.Create(ctx, cred); err != nil {
		return nil, fmt.Errorf("secrets: create credential %q: %w", name, err)
	}
	return cred, nil
}

// Get returns the decrypted payload of the credential named name for
// tenantID, decrypting with whichever key id the row was last written
// under (cred.KeyID) -- which need not be the ring's current active key.
func (s *Service) Get(ctx context.Context, tenantID, name string) (map[string]string, error) {
	cred, err := s.store.Get(ctx, tenantID, name)
	if err != nil {
		return nil, fmt.Errorf("secrets: get credential %q: %w", name, err)
	}

	plaintext, err := s.ring.Decrypt(cred.KeyID, cred.Ciphertext, cred.Nonce)
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt credential %q: %w", name, err)
	}

	var payload map[string]string
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return nil, fmt.Errorf("secrets: unmarshal credential %q: %w", name, err)
	}
	return payload, nil
}

// GetField returns one field of the credential named name for tenantID. It
// returns an error wrapping ErrFieldNotFound if the decrypted payload has
// no such field.
func (s *Service) GetField(ctx context.Context, tenantID, name, field string) (string, error) {
	payload, err := s.Get(ctx, tenantID, name)
	if err != nil {
		return "", err
	}
	v, ok := payload[field]
	if !ok {
		return "", fmt.Errorf("secrets: credential %q has no field %q: %w", name, field, ErrFieldNotFound)
	}
	return v, nil
}

// Seal JSON-encodes and encrypts payload under the ring's active key --
// the same cipher path Create/Rotate use -- WITHOUT persisting anything.
// It exists for a caller (model catalog Connect) that must hand the
// gateway a ciphertext/nonce/key_id/field_names tuple to insert itself,
// inside its own transaction spanning more than a credential row, rather
// than through Create's own separate INSERT. Every ordinary caller should
// still prefer Create.
func (s *Service) Seal(payload map[string]string) (ciphertext, nonce []byte, keyID string, fieldNamesOut []string, err error) {
	ciphertext, nonce, err = s.seal(payload)
	if err != nil {
		return nil, nil, "", nil, err
	}
	return ciphertext, nonce, s.ring.ActiveKeyID, fieldNames(payload), nil
}

// Rotate re-encrypts name's payload (which may differ from what was
// originally stored) under the ring's active key and persists it,
// replacing the row's ciphertext/nonce/key_id/field_names and setting
// RotatedAt.
func (s *Service) Rotate(ctx context.Context, tenantID, name string, payload map[string]string) error {
	ciphertext, nonce, err := s.seal(payload)
	if err != nil {
		return err
	}
	if err := s.store.Rotate(ctx, tenantID, name, ciphertext, nonce, s.ring.ActiveKeyID, fieldNames(payload)); err != nil {
		return fmt.Errorf("secrets: rotate credential %q: %w", name, err)
	}
	s.Invalidate(tenantID, name)
	return nil
}

// Delete removes the credential named name for tenantID.
func (s *Service) Delete(ctx context.Context, tenantID, name string) error {
	if err := s.store.Delete(ctx, tenantID, name); err != nil {
		return fmt.Errorf("secrets: delete credential %q: %w", name, err)
	}
	s.Invalidate(tenantID, name)
	return nil
}

// List returns metadata for every credential belonging to tenantID.
// Ciphertext/Nonce are never populated (store.CredentialStore.List already
// omits them; this method never calls Get, so it never decrypts anything).
func (s *Service) List(ctx context.Context, tenantID string) ([]*store.Credential, error) {
	creds, err := s.store.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("secrets: list credentials: %w", err)
	}
	return creds, nil
}

// ReEncryptAll rewrites every credential belonging to tenantID whose KeyID
// is not the ring's current ActiveKeyID, decrypting under the row's own
// key and re-encrypting under the active one. It returns the number of
// rows rewritten. Used for master-key rotation, exposed via
// `gateway secrets rekey`.
func (s *Service) ReEncryptAll(ctx context.Context, tenantID string) (int, error) {
	creds, err := s.store.List(ctx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("secrets: list credentials for rekey: %w", err)
	}

	var rewritten int
	for _, c := range creds {
		if c.KeyID == s.ring.ActiveKeyID {
			continue
		}

		full, err := s.store.Get(ctx, tenantID, c.Name)
		if err != nil {
			return rewritten, fmt.Errorf("secrets: get credential %q for rekey: %w", c.Name, err)
		}
		plaintext, err := s.ring.Decrypt(full.KeyID, full.Ciphertext, full.Nonce)
		if err != nil {
			return rewritten, fmt.Errorf("secrets: decrypt credential %q for rekey: %w", c.Name, err)
		}
		ciphertext, nonce, err := s.ring.Encrypt(s.ring.ActiveKeyID, plaintext)
		if err != nil {
			return rewritten, fmt.Errorf("secrets: re-encrypt credential %q: %w", c.Name, err)
		}
		if err := s.store.Rotate(ctx, tenantID, c.Name, ciphertext, nonce, s.ring.ActiveKeyID, full.FieldNames); err != nil {
			return rewritten, fmt.Errorf("secrets: persist re-encrypted credential %q: %w", c.Name, err)
		}
		s.Invalidate(tenantID, c.Name)
		rewritten++
	}
	return rewritten, nil
}

// Fetcher returns a headers.CredentialFetcher backed by GetField, with an
// in-memory read cache (TTL 5 minutes, keyed by tenant|name|field). Used by
// the "secret_store" header provider so resolving the same header on every
// proxied request doesn't decrypt on every call.
func (s *Service) Fetcher() headers.CredentialFetcher {
	return func(ctx context.Context, tenantID, name, field string) (string, error) {
		key := cacheKey(tenantID, name, field)
		if v, ok := s.cacheGet(key); ok {
			return v, nil
		}

		v, err := s.GetField(ctx, tenantID, name, field)
		if err != nil {
			return "", err
		}
		s.cacheSet(key, v)
		return v, nil
	}
}

// Invalidate evicts every cached field of credential name for tenantID
// (across all fields previously fetched via Fetcher). Rotate and Delete
// call this automatically; callers that mutate a credential by some other
// path (there are none yet) should call it too.
func (s *Service) Invalidate(tenantID, name string) {
	s.evictCachePrefix(tenantID + "|" + name + "|")
}

// InvalidateTenant evicts every cached field for every credential of
// tenantID. It exists for callers (e.g. the "secret_store" provider's
// Invalidate, driven by a connector-level auth failure) that know a
// caller's tenant but not which specific credential backs the connector
// that failed -- a coarser, best-effort flush.
func (s *Service) InvalidateTenant(tenantID string) {
	s.evictCachePrefix(tenantID + "|")
}

func (s *Service) evictCachePrefix(prefix string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for k := range s.cache {
		if strings.HasPrefix(k, prefix) {
			delete(s.cache, k)
		}
	}
}

func (s *Service) cacheGet(key string) (string, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	e, ok := s.cache[key]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expiresAt) {
		delete(s.cache, key)
		return "", false
	}
	return e.value, true
}

func (s *Service) cacheSet(key, value string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.cache[key] = cacheEntry{value: value, expiresAt: time.Now().Add(s.cacheTTL)}
}

func cacheKey(tenantID, name, field string) string {
	return tenantID + "|" + name + "|" + field
}

// seal JSON-encodes payload and encrypts it under the ring's active key.
func (s *Service) seal(payload map[string]string) (ciphertext, nonce []byte, err error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("secrets: marshal payload: %w", err)
	}
	ciphertext, nonce, err = s.ring.Encrypt(s.ring.ActiveKeyID, plaintext)
	if err != nil {
		return nil, nil, fmt.Errorf("secrets: encrypt payload: %w", err)
	}
	return ciphertext, nonce, nil
}

func fieldNames(payload map[string]string) []string {
	names := make([]string, 0, len(payload))
	for k := range payload {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
