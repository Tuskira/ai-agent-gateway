package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	internalauth "github.com/Tuskira/tusk-ai-secured-gateway/internal/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/apikey"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// TestPermissions_CustomRole_EndToEnd exercises config-driven roles
// (internal/config's Auth.Roles) all the way through: the authorizer is
// built the same way cmd/gateway/main.go builds it (NewRoleAuthorizer,
// then overlay a custom-roles map), POST /api-keys' role validation
// (Deps.AllowedRoles, sourced from that exact authorizer via
// allowedRoles) accepts the custom role and rejects an unknown one, and a
// real API key minted with the custom role is enforced by the real
// apikey.Authenticator + RoleAuthorizer for both a read (allowed) and a
// write (denied) route.
func TestPermissions_CustomRole_EndToEnd(t *testing.T) {
	authorizer := pkgauth.NewRoleAuthorizer()
	// Mirrors cmd/gateway/main.go's merge of config.Auth.Roles over the
	// built-ins.
	for name, patterns := range map[string][]string{"reader": {"*.read"}} {
		authorizer.Rules[name] = patterns
	}

	rstore := &roleTestStore{apiKeys: newMemAPIKeyStore()}
	apiKeyAuth := apikey.New(rstore.apiKeys, apikey.Options{})

	principals := map[string]*pkgauth.Principal{
		"admin-a": {Subject: "admin-a", TenantID: "tenant-a", Roles: []string{"admin"}, AuthMethod: "test"},
	}
	// scriptedAuthenticator only recognizes tokens in `principals`; any
	// other bearer (e.g. a real gk_... key) falls through to apiKeyAuth,
	// same ordering cmd/gateway/main.go's buildAuthenticator uses.
	authenticator := internalauth.Chain(&scriptedAuthenticator{principals: principals}, apiKeyAuth)

	h := NewRouter(Deps{
		ServiceVersion: "test",
		Authenticator:  authenticator,
		Authorizer:     authorizer,
		Store:          rstore,
		KeyInvalidator: apiKeyAuth,
	})

	do := func(method, path, bearer, body string) *httptest.ResponseRecorder {
		var req *http.Request
		if body != "" {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// An unknown role is rejected with 400, listing the known roles.
	w := do(http.MethodPost, "/api/v1/api-keys", "admin-a", `{"name":"x","role":"nope"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create with role=nope status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	var badRoleErr struct {
		Error struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &badRoleErr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(badRoleErr.Error.Message, "reader") {
		t.Errorf("error message = %q, want it to list the known roles including reader", badRoleErr.Error.Message)
	}

	// The custom role "reader" is accepted.
	w = do(http.MethodPost, "/api/v1/api-keys", "admin-a", `{"name":"reader-bot","role":"reader"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create with role=reader status = %d, want 201, body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Key == "" {
		t.Fatal("create response carried no plaintext key")
	}

	// The reader key is enforced by the real authorizer: reads allowed,
	// writes refused.
	if w := do(http.MethodGet, "/api/v1/connectors", created.Key, ""); w.Code != http.StatusOK {
		t.Errorf("reader key GET /connectors = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if w := do(http.MethodPost, "/api/v1/connectors", created.Key, `{}`); w.Code != http.StatusForbidden {
		t.Errorf("reader key POST /connectors = %d, want 403, body=%s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// roleTestStore: nopStore, but with a real (in-memory, tenant-scoped)
// APIKeyStore -- needed here because, unlike TestPermissions_FullStack's
// scripted principals, this test mints and authenticates a real API key.
// ---------------------------------------------------------------------------

type roleTestStore struct {
	nopStore
	apiKeys *memAPIKeyStore
}

func (s *roleTestStore) APIKeys() store.APIKeyStore { return s.apiKeys }

var _ store.Store = (*roleTestStore)(nil)

type memAPIKeyStore struct {
	mu   sync.Mutex
	byID map[string]*store.APIKey
	seq  int
}

func newMemAPIKeyStore() *memAPIKeyStore {
	return &memAPIKeyStore{byID: map[string]*store.APIKey{}}
}

func (s *memAPIKeyStore) Create(_ context.Context, k *store.APIKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.byID {
		if existing.KeyHash == k.KeyHash {
			return store.ErrConflict
		}
	}
	s.seq++
	k.ID = fmt.Sprintf("key-%d", s.seq)
	k.CreatedAt = time.Now().UTC()
	cp := *k
	s.byID[k.ID] = &cp
	return nil
}

func (s *memAPIKeyStore) GetByHash(_ context.Context, hash string) (*store.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.byID {
		if k.KeyHash == hash {
			cp := *k
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *memAPIKeyStore) GetByID(_ context.Context, tenantID, id string) (*store.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok || k.TenantID != tenantID {
		return nil, store.ErrNotFound
	}
	cp := *k
	return &cp, nil
}

func (s *memAPIKeyStore) List(_ context.Context, tenantID string) ([]*store.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.APIKey
	for _, k := range s.byID {
		if k.TenantID == tenantID {
			cp := *k
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *memAPIKeyStore) Revoke(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok || k.TenantID != tenantID {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	k.RevokedAt = &now
	return nil
}

func (s *memAPIKeyStore) TouchLastUsed(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok {
		return store.ErrNotFound
	}
	k.LastUsedAt = &at
	return nil
}

func (s *memAPIKeyStore) SetLimits(_ context.Context, tenantID, id string, l *store.Limits) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok || k.TenantID != tenantID {
		return store.ErrNotFound
	}
	k.Limits = l
	return nil
}

func (s *memAPIKeyStore) SetProfile(_ context.Context, tenantID, id string, profileID *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok || k.TenantID != tenantID {
		return store.ErrNotFound
	}
	k.ProfileID = profileID
	return nil
}

var _ store.APIKeyStore = (*memAPIKeyStore)(nil)
