package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// TestPermissions_FullStack exercises the actual route wiring (buildRoutes'
// Permission field + internalauth.RequirePermission + the real
// pkgauth.RoleAuthorizer), not just an individual handler in isolation:
// an "agent" principal can read connectors but is refused write access,
// and is refused api-keys entirely (see APIKeys' doc comment on why that's
// gated by "admin.manage" rather than an "apikey.*" permission).
//
// This complements internal/api/handlers' per-handler tests (which use a
// full fake store.Store to exercise validation/happy-path/tenant-isolation
// logic); this file only needs enough of a store to prove the *routing*
// layer gates correctly, so nopStore below is intentionally trivial --
// none of these requests are expected to reach deep store logic (writes
// are blocked by permission middleware before the handler runs at all).
func TestPermissions_FullStack(t *testing.T) {
	principals := map[string]*pkgauth.Principal{
		"admin-a":    {Subject: "admin-a", TenantID: "tenant-a", Roles: []string{"admin"}, AuthMethod: "test"},
		"platform-a": {Subject: "platform-a", TenantID: "tenant-a", Roles: pkgauth.RolesForKey(pkgauth.RolePlatformAdmin), AuthMethod: "test"},
		"agent-a":    {Subject: "agent-a", TenantID: "tenant-a", Roles: []string{"agent"}, AuthMethod: "test"},
	}

	h := NewRouter(Deps{
		ServiceVersion: "test",
		Authenticator:  &scriptedAuthenticator{principals: principals},
		Authorizer:     pkgauth.NewRoleAuthorizer(),
		Store:          nopStore{},
	})

	do := func(method, path, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := do(http.MethodGet, "/api/v1/connectors", "agent-a"); w.Code != http.StatusOK {
		t.Errorf("agent GET /connectors = %d, want 200", w.Code)
	}
	if w := do(http.MethodPost, "/api/v1/connectors", "agent-a"); w.Code != http.StatusForbidden {
		t.Errorf("agent POST /connectors = %d, want 403", w.Code)
	}
	// The MCP catalog mirrors connectors: an agent can read it, not add.
	if w := do(http.MethodGet, "/api/v1/mcp-catalog", "agent-a"); w.Code != http.StatusOK {
		t.Errorf("agent GET /mcp-catalog = %d, want 200", w.Code)
	}
	if w := do(http.MethodPost, "/api/v1/mcp-catalog/drawio/add", "agent-a"); w.Code != http.StatusForbidden {
		t.Errorf("agent POST /mcp-catalog/drawio/add = %d, want 403", w.Code)
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		path := "/api/v1/mcp-catalog"
		if m != http.MethodPost {
			path += "/drawio"
		}
		if w := do(m, path, "agent-a"); w.Code != http.StatusForbidden {
			t.Errorf("agent %s %s = %d, want 403", m, path, w.Code)
		}
	}
	if w := do(http.MethodGet, "/api/v1/api-keys", "agent-a"); w.Code != http.StatusForbidden {
		t.Errorf("agent GET /api-keys = %d, want 403", w.Code)
	}
	if w := do(http.MethodGet, "/api/v1/credentials", "agent-a"); w.Code != http.StatusForbidden {
		t.Errorf("agent GET /credentials = %d, want 403", w.Code)
	}

	// Connector health/discover and cache routes: reads are "*.read"
	// (granted to agent), writes need connector.update/cache.manage
	// (not granted). ConnectorOps/CacheOps are nil in this test's Deps,
	// so an allowed request reaches the handler and gets 503 -- the
	// assertion here is only that permission routing itself does not
	// block it with 403.
	if w := do(http.MethodGet, "/api/v1/connectors/x/health", "agent-a"); w.Code == http.StatusForbidden {
		t.Errorf("agent GET .../health = 403, want allowed (connector.read)")
	}
	if w := do(http.MethodPost, "/api/v1/connectors/x/discover", "agent-a"); w.Code != http.StatusForbidden {
		t.Errorf("agent POST .../discover = %d, want 403", w.Code)
	}
	if w := do(http.MethodGet, "/api/v1/cache/stats", "agent-a"); w.Code == http.StatusForbidden {
		t.Errorf("agent GET /cache/stats = 403, want allowed (cache.read)")
	}
	if w := do(http.MethodGet, "/api/v1/cache/search", "agent-a"); w.Code == http.StatusForbidden {
		t.Errorf("agent GET /cache/search = 403, want allowed (cache.read)")
	}
	if w := do(http.MethodPost, "/api/v1/cache/refresh", "agent-a"); w.Code != http.StatusForbidden {
		t.Errorf("agent POST /cache/refresh = %d, want 403", w.Code)
	}
	if w := do(http.MethodPost, "/api/v1/cache/refresh/connectors/x", "agent-a"); w.Code != http.StatusForbidden {
		t.Errorf("agent POST /cache/refresh/connectors/x = %d, want 403", w.Code)
	}
	if w := do(http.MethodDelete, "/api/v1/cache", "agent-a"); w.Code != http.StatusForbidden {
		t.Errorf("agent DELETE /cache = %d, want 403", w.Code)
	}
	if w := do(http.MethodDelete, "/api/v1/cache/connectors/x", "agent-a"); w.Code != http.StatusForbidden {
		t.Errorf("agent DELETE /cache/connectors/x = %d, want 403", w.Code)
	}

	// admin is not blocked by permission middleware: POST /connectors
	// with an empty body fails validation (400), but must not be 403.
	if w := do(http.MethodPost, "/api/v1/connectors", "admin-a"); w.Code == http.StatusForbidden {
		t.Errorf("admin POST /connectors = 403, want anything but 403 (permission should pass)")
	}
	if w := do(http.MethodGet, "/api/v1/api-keys", "admin-a"); w.Code != http.StatusOK {
		t.Errorf("admin GET /api-keys = %d, want 200", w.Code)
	}

	// Tenant enumeration (platform.admin): an agent's "*.read" wildcard
	// must NOT satisfy it -- both routes 403 for agent-a. only platform-admin holds
	// platform.admin (NewRoleAuthorizer), so for it GET succeeds and
	// POST reaches validation (400 on an empty body), never 403.
	if w := do(http.MethodGet, "/api/v1/tenants", "agent-a"); w.Code != http.StatusForbidden {
		t.Errorf("agent GET /tenants = %d, want 403", w.Code)
	}
	if w := do(http.MethodPost, "/api/v1/tenants", "agent-a"); w.Code != http.StatusForbidden {
		t.Errorf("agent POST /tenants = %d, want 403", w.Code)
	}
	// A tenant admin ("*") is NOT a platform admin: both routes 403.
	if w := do(http.MethodGet, "/api/v1/tenants", "admin-a"); w.Code != http.StatusForbidden {
		t.Errorf("tenant admin GET /tenants = %d, want 403", w.Code)
	}
	if w := do(http.MethodPost, "/api/v1/tenants", "admin-a"); w.Code != http.StatusForbidden {
		t.Errorf("tenant admin POST /tenants = %d, want 403", w.Code)
	}
	if w := do(http.MethodGet, "/api/v1/tenants", "platform-a"); w.Code != http.StatusOK {
		t.Errorf("platform-admin GET /tenants = %d, want 200", w.Code)
	}
	if w := do(http.MethodPost, "/api/v1/tenants", "platform-a"); w.Code == http.StatusForbidden {
		t.Errorf("platform-admin POST /tenants = 403, want anything but 403 (permission should pass)")
	}
}

// TestPermissions_PlatformAdmin_BareStarRoleDenied is the full-stack
// counterpart to pkg/auth's
// TestRoleAuthorizer_CustomRole_BareStarDoesNotGrantPlatformAdmin: a
// custom role holding only the blanket "*" pattern must still be refused
// GET /tenants, proving the platform namespace carve-out actually reaches
// the mounted route, not just RoleAuthorizer.Allow in isolation.
func TestPermissions_PlatformAdmin_BareStarRoleDenied(t *testing.T) {
	authorizer := &pkgauth.RoleAuthorizer{Rules: map[string][]string{
		"superstar": {"*"},
	}}
	principals := map[string]*pkgauth.Principal{
		"star-a": {Subject: "star-a", TenantID: "tenant-a", Roles: []string{"superstar"}, AuthMethod: "test"},
	}
	h := NewRouter(Deps{
		ServiceVersion: "test",
		Authenticator:  &scriptedAuthenticator{principals: principals},
		Authorizer:     authorizer,
		Store:          nopStore{},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tenants", nil)
	req.Header.Set("Authorization", "Bearer star-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("bare-\"*\" role GET /tenants = %d, want 403", w.Code)
	}
}

// scriptedAuthenticator resolves "Authorization: Bearer <token>" directly
// onto a fixed Principal keyed by token, bypassing real credential
// parsing/hashing entirely -- these tests are about permission routing,
// not authentication.
type scriptedAuthenticator struct {
	principals map[string]*pkgauth.Principal
}

func (s *scriptedAuthenticator) Name() string { return "scripted" }

func (s *scriptedAuthenticator) Authenticate(_ context.Context, r *http.Request) (*pkgauth.Principal, error) {
	authz := r.Header.Get("Authorization")
	if authz == "" {
		return nil, pkgauth.ErrNoCredential
	}
	token := strings.TrimPrefix(authz, "Bearer ")
	p, ok := s.principals[token]
	if !ok {
		return nil, pkgauth.ErrNoCredential
	}
	return p, nil
}

// nopStore is a trivial store.Store: every read returns an empty/zero
// result and every write succeeds as a no-op. It exists only so
// TestPermissions_FullStack can build a real *chi.Mux via NewRouter
// without wiring a full fake store -- see that test's doc comment.
type nopStore struct{}

func (nopStore) Tenants() store.TenantStore             { return nopTenants{} }
func (nopStore) APIKeys() store.APIKeyStore             { return nopAPIKeys{} }
func (nopStore) Credentials() store.CredentialStore     { return nopCredentials{} }
func (nopStore) Connectors() store.ConnectorStore       { return nopConnectors{} }
func (nopStore) MCPCatalog() store.MCPCatalogStore      { return nopAuthn.MCPCatalog() }
func (nopStore) AgentProfiles() store.AgentProfileStore { return nopProfiles{} }
func (nopStore) ToolCache() store.ToolCacheStore        { return nopToolCache{} }
func (nopStore) Models() store.ModelStore               { return nopModels{} }
func (nopStore) ModelCatalog() store.ModelCatalogStore  { return nopModelCatalog{} }
func (nopStore) Skills() store.SkillStore               { return nopSkills{} }

// nopAuthn backs nopStore's users/sessions/audit facets (in-memory, empty).
var nopAuthn = dptest.New()

func (nopStore) Users() store.UserStore               { return nopAuthn.Users() }
func (nopStore) UserSessions() store.UserSessionStore { return nopAuthn.UserSessions() }
func (nopStore) AuthAudit() store.AuthAuditStore      { return nopAuthn.AuthAudit() }
func (nopStore) Migrate(context.Context) error        { return nil }
func (nopStore) Ping(context.Context) error           { return nil }
func (nopStore) Close() error                         { return nil }

type nopTenants struct{}

func (nopTenants) Create(context.Context, *store.Tenant) error { return nil }
func (nopTenants) GetBySlug(context.Context, string) (*store.Tenant, error) {
	return nil, store.ErrNotFound
}
func (nopTenants) List(context.Context) ([]*store.Tenant, error) { return nil, nil }

type nopAPIKeys struct{}

func (nopAPIKeys) Create(context.Context, *store.APIKey) error { return nil }
func (nopAPIKeys) GetByHash(context.Context, string) (*store.APIKey, error) {
	return nil, store.ErrNotFound
}
func (nopAPIKeys) GetByID(context.Context, string, string) (*store.APIKey, error) {
	return nil, store.ErrNotFound
}
func (nopAPIKeys) List(context.Context, string) ([]*store.APIKey, error)  { return nil, nil }
func (nopAPIKeys) Revoke(context.Context, string, string) error           { return store.ErrNotFound }
func (nopAPIKeys) TouchLastUsed(context.Context, string, time.Time) error { return nil }
func (nopAPIKeys) SetLimits(context.Context, string, string, *store.Limits) error {
	return store.ErrNotFound
}
func (nopAPIKeys) SetProfile(context.Context, string, string, *string) error {
	return store.ErrNotFound
}

type nopCredentials struct{}

func (nopCredentials) Create(context.Context, *store.Credential) error { return nil }
func (nopCredentials) Get(context.Context, string, string) (*store.Credential, error) {
	return nil, store.ErrNotFound
}
func (nopCredentials) List(context.Context, string) ([]*store.Credential, error) { return nil, nil }
func (nopCredentials) Rotate(context.Context, string, string, []byte, []byte, string, []string) error {
	return store.ErrNotFound
}
func (nopCredentials) Delete(context.Context, string, string) error { return store.ErrNotFound }

type nopConnectors struct{}

func (nopConnectors) Create(context.Context, *store.Connector) error { return nil }
func (nopConnectors) Get(context.Context, string, string) (*store.Connector, error) {
	return nil, store.ErrNotFound
}
func (nopConnectors) GetBySlug(context.Context, string, string) (*store.Connector, error) {
	return nil, store.ErrNotFound
}
func (nopConnectors) List(context.Context, string) ([]*store.Connector, error) { return nil, nil }
func (nopConnectors) Update(context.Context, *store.Connector) error           { return store.ErrNotFound }
func (nopConnectors) SoftDelete(context.Context, string, string) error         { return store.ErrNotFound }

type nopProfiles struct{}

func (nopProfiles) Create(context.Context, *store.AgentProfile) error { return nil }
func (nopProfiles) Get(context.Context, string, string) (*store.AgentProfile, error) {
	return nil, store.ErrNotFound
}
func (nopProfiles) GetBySlug(context.Context, string, string) (*store.AgentProfile, error) {
	return nil, store.ErrNotFound
}
func (nopProfiles) List(context.Context, string) ([]*store.AgentProfile, error) { return nil, nil }
func (nopProfiles) Update(context.Context, *store.AgentProfile) error           { return store.ErrNotFound }
func (nopProfiles) SoftDelete(context.Context, string, string) error            { return store.ErrNotFound }
func (nopProfiles) SetTools(context.Context, string, string, []store.ProfileTool) error {
	return store.ErrNotFound
}
func (nopProfiles) GetTools(context.Context, string, string) ([]store.ProfileTool, error) {
	return nil, nil
}
func (nopProfiles) SetSkills(context.Context, string, []store.ProfileSkill) error {
	return store.ErrNotFound
}
func (nopProfiles) GetSkills(context.Context, string) ([]store.ProfileSkill, error) {
	return nil, nil
}

type nopToolCache struct{}

func (nopToolCache) Upsert(context.Context, ...*store.CachedTool) error { return nil }
func (nopToolCache) ListByConnector(context.Context, string, string) ([]*store.CachedTool, error) {
	return nil, nil
}
func (nopToolCache) ListByTenant(context.Context, string) ([]*store.CachedTool, error) {
	return nil, nil
}
func (nopToolCache) Search(context.Context, string, string, int, bool) ([]*store.CachedTool, error) {
	return nil, nil
}
func (nopToolCache) MarkStale(context.Context, string) error         { return nil }
func (nopToolCache) DeleteByConnector(context.Context, string) error { return nil }
func (nopToolCache) DeleteExpired(context.Context) (int64, error)    { return 0, nil }

type nopModels struct{}

func (nopModels) Create(context.Context, *store.Model) error { return nil }
func (nopModels) Get(context.Context, string, string) (*store.Model, error) {
	return nil, store.ErrNotFound
}
func (nopModels) GetByName(context.Context, string, string) (*store.Model, error) {
	return nil, store.ErrNotFound
}
func (nopModels) List(context.Context, string, store.ListOptions) ([]*store.Model, error) {
	return nil, nil
}
func (nopModels) Update(context.Context, *store.Model) error       { return nil }
func (nopModels) SoftDelete(context.Context, string, string) error { return nil }

type nopModelCatalog struct{}

func (nopModelCatalog) ListProviders(context.Context) ([]*store.ModelCatalogProvider, error) {
	return nil, nil
}
func (nopModelCatalog) GetProvider(context.Context, string) (*store.ModelCatalogProvider, error) {
	return nil, store.ErrNotFound
}
func (nopModelCatalog) CreateProvider(context.Context, *store.ModelCatalogProvider) error { return nil }
func (nopModelCatalog) UpdateProvider(context.Context, *store.ModelCatalogProvider) error { return nil }
func (nopModelCatalog) DeleteProvider(context.Context, string) error                      { return nil }
func (nopModelCatalog) GetModel(context.Context, string) (*store.ModelCatalogModel, error) {
	return nil, store.ErrNotFound
}
func (nopModelCatalog) CreateModel(context.Context, *store.ModelCatalogModel) error { return nil }
func (nopModelCatalog) UpdateModel(context.Context, *store.ModelCatalogModel) error { return nil }
func (nopModelCatalog) DeleteModel(context.Context, string) error                   { return nil }
func (nopModelCatalog) CountTenantUsage(context.Context, string) (int, error)       { return 0, nil }
func (nopModelCatalog) Connect(context.Context, string, store.CatalogConnectCredential, []store.CatalogConnectModel) (*store.CatalogConnectResult, error) {
	return nil, store.ErrNotFound
}
func (nopModelCatalog) ApplyPrices(context.Context, []store.PriceUpdate, bool) (*store.ApplyPricesResult, error) {
	return nil, store.ErrNotFound
}

type nopSkills struct{}

func (nopSkills) Create(context.Context, *store.Skill, []store.SkillFile, string) error { return nil }
func (nopSkills) Get(context.Context, string, string) (*store.Skill, error) {
	return nil, store.ErrNotFound
}
func (nopSkills) GetByName(context.Context, string, string) (*store.Skill, error) {
	return nil, store.ErrNotFound
}
func (nopSkills) List(context.Context, string, store.SkillListOptions) ([]store.Skill, int, error) {
	return nil, 0, nil
}
func (nopSkills) Update(context.Context, *store.Skill) error       { return nil }
func (nopSkills) SoftDelete(context.Context, string, string) error { return nil }
func (nopSkills) AddVersion(context.Context, string, string, []store.SkillFile, string) (*store.SkillVersion, error) {
	return nil, store.ErrNotFound
}
func (nopSkills) GetVersion(context.Context, string, int) (*store.SkillVersion, error) {
	return nil, store.ErrNotFound
}
func (nopSkills) ListVersions(context.Context, string) ([]store.SkillVersion, error) {
	return nil, nil
}
