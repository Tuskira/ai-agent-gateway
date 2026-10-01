package handlers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/skills"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// fakeStore is a minimal, map-backed store.Store for handler tests. It
// mirrors the semantics documented on pkg/store's interfaces (tenant
// scoping, uniqueness -> store.ErrConflict, missing row ->
// store.ErrNotFound, soft delete hides a row from Get/GetBySlug/List)
// closely enough to exercise every handler path without a real database.
// It follows the same shape as internal/secrets/store_test.go's
// fakeCredentialStore, extended to the rest of pkg/store.Store.
type fakeStore struct {
	tenants      *fakeTenantStore
	apiKeys      *fakeAPIKeyStore
	credentials  *fakeCredentialStore
	connectors   *fakeConnectorStore
	profiles     *fakeAgentProfileStore
	toolCache    *fakeToolCacheStore
	models       *fakeModelStore
	modelCatalog *fakeModelCatalogStore
	skills       *fakeSkillStore
	// authn backs the users/sessions/auth-audit facets with dptest's full
	// in-memory implementation (shared rather than copied; see
	// internal/dataplane/dptest/users.go).
	authn *dptest.Store
}

func newFakeStore() *fakeStore {
	connectors := &fakeConnectorStore{byID: map[string]*store.Connector{}}
	profiles := &fakeAgentProfileStore{byID: map[string]*store.AgentProfile{}, tools: map[string][]store.ProfileTool{}, skills: map[string][]store.ProfileSkill{}}
	credentials := &fakeCredentialStore{byKey: map[string]*store.Credential{}}
	models := &fakeModelStore{byID: map[string]*store.Model{}}
	return &fakeStore{
		tenants:     &fakeTenantStore{byID: map[string]*store.Tenant{}},
		apiKeys:     &fakeAPIKeyStore{byID: map[string]*store.APIKey{}},
		credentials: credentials,
		connectors:  connectors,
		profiles:    profiles,
		toolCache:   &fakeToolCacheStore{items: map[string]*store.CachedTool{}, connectors: connectors},
		models:      models,
		modelCatalog: &fakeModelCatalogStore{
			providers:    map[string]*store.ModelCatalogProvider{},
			models:       map[string]*store.ModelCatalogModel{},
			credentials:  credentials,
			tenantModels: models,
		},
		skills: &fakeSkillStore{byID: map[string]*store.Skill{}, versions: map[string][]store.SkillVersion{}},
		authn:  dptest.New(),
	}
}

func (f *fakeStore) Tenants() store.TenantStore             { return f.tenants }
func (f *fakeStore) APIKeys() store.APIKeyStore             { return f.apiKeys }
func (f *fakeStore) Credentials() store.CredentialStore     { return f.credentials }
func (f *fakeStore) Connectors() store.ConnectorStore       { return f.connectors }
func (f *fakeStore) MCPCatalog() store.MCPCatalogStore      { return f.authn.MCPCatalog() }
func (f *fakeStore) AgentProfiles() store.AgentProfileStore { return f.profiles }
func (f *fakeStore) ToolCache() store.ToolCacheStore        { return f.toolCache }
func (f *fakeStore) Models() store.ModelStore               { return f.models }
func (f *fakeStore) ModelCatalog() store.ModelCatalogStore  { return f.modelCatalog }
func (f *fakeStore) Skills() store.SkillStore               { return f.skills }
func (f *fakeStore) Users() store.UserStore                 { return f.authn.Users() }
func (f *fakeStore) UserSessions() store.UserSessionStore   { return f.authn.UserSessions() }
func (f *fakeStore) AuthAudit() store.AuthAuditStore        { return f.authn.AuthAudit() }
func (f *fakeStore) Migrate(context.Context) error          { return nil }
func (f *fakeStore) Ping(context.Context) error             { return nil }
func (f *fakeStore) Close() error                           { return nil }

var _ store.Store = (*fakeStore)(nil)

// ---------------------------------------------------------------------------
// tenants
// ---------------------------------------------------------------------------

type fakeTenantStore struct {
	mu    sync.Mutex
	byID  map[string]*store.Tenant
	order []string
	seq   int
}

func (s *fakeTenantStore) Create(_ context.Context, t *store.Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.byID {
		if existing.Slug == t.Slug {
			return store.ErrConflict
		}
	}
	s.seq++
	t.ID = fmt.Sprintf("tenant-%d", s.seq)
	t.CreatedAt = time.Now().UTC()
	cp := *t
	s.byID[t.ID] = &cp
	s.order = append(s.order, t.ID)
	return nil
}

func (s *fakeTenantStore) GetBySlug(_ context.Context, slug string) (*store.Tenant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.byID {
		if t.Slug == slug {
			cp := *t
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *fakeTenantStore) List(_ context.Context) ([]*store.Tenant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*store.Tenant, 0, len(s.order))
	for _, id := range s.order {
		cp := *s.byID[id]
		out = append(out, &cp)
	}
	return out, nil
}

var _ store.TenantStore = (*fakeTenantStore)(nil)

// ---------------------------------------------------------------------------
// api keys
// ---------------------------------------------------------------------------

type fakeAPIKeyStore struct {
	mu    sync.Mutex
	byID  map[string]*store.APIKey
	order []string
	seq   int
}

func (s *fakeAPIKeyStore) Create(_ context.Context, k *store.APIKey) error {
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
	s.order = append(s.order, k.ID)
	return nil
}

func (s *fakeAPIKeyStore) GetByHash(_ context.Context, keyHash string) (*store.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.byID {
		if k.KeyHash == keyHash {
			cp := *k
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *fakeAPIKeyStore) GetByID(_ context.Context, tenantID, id string) (*store.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok || k.TenantID != tenantID {
		return nil, store.ErrNotFound
	}
	cp := *k
	return &cp, nil
}

func (s *fakeAPIKeyStore) List(_ context.Context, tenantID string) ([]*store.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.APIKey
	for _, id := range s.order {
		k := s.byID[id]
		if k.TenantID != tenantID {
			continue
		}
		cp := *k
		out = append(out, &cp)
	}
	return out, nil
}

func (s *fakeAPIKeyStore) Revoke(_ context.Context, tenantID, id string) error {
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

func (s *fakeAPIKeyStore) TouchLastUsed(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok {
		return store.ErrNotFound
	}
	k.LastUsedAt = &at
	return nil
}

func (s *fakeAPIKeyStore) SetLimits(_ context.Context, tenantID, id string, l *store.Limits) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok || k.TenantID != tenantID {
		return store.ErrNotFound
	}
	if l.Empty() {
		l = nil
	}
	k.Limits = l
	return nil
}

func (s *fakeAPIKeyStore) SetProfile(_ context.Context, tenantID, id string, profileID *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok || k.TenantID != tenantID {
		return store.ErrNotFound
	}
	k.ProfileID = profileID
	return nil
}

var _ store.APIKeyStore = (*fakeAPIKeyStore)(nil)

// ---------------------------------------------------------------------------
// credentials
// ---------------------------------------------------------------------------

type fakeCredentialStore struct {
	mu    sync.Mutex
	byKey map[string]*store.Credential
	order []string
}

func credKey(tenantID, name string) string { return tenantID + "|" + name }

func (s *fakeCredentialStore) Create(_ context.Context, c *store.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := credKey(c.TenantID, c.Name)
	if _, exists := s.byKey[key]; exists {
		return store.ErrConflict
	}
	c.ID = fmt.Sprintf("cred-%d", len(s.order)+1)
	c.CreatedAt = time.Now().UTC()
	cp := *c
	s.byKey[key] = &cp
	s.order = append(s.order, key)
	return nil
}

func (s *fakeCredentialStore) Get(_ context.Context, tenantID, name string) (*store.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byKey[credKey(tenantID, name)]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (s *fakeCredentialStore) List(_ context.Context, tenantID string) ([]*store.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.Credential
	for _, key := range s.order {
		c := s.byKey[key]
		if c.TenantID != tenantID {
			continue
		}
		meta := *c
		meta.Ciphertext = nil
		meta.Nonce = nil
		out = append(out, &meta)
	}
	return out, nil
}

func (s *fakeCredentialStore) Rotate(_ context.Context, tenantID, name string, ciphertext, nonce []byte, keyID string, fieldNames []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byKey[credKey(tenantID, name)]
	if !ok {
		return store.ErrNotFound
	}
	c.Ciphertext = ciphertext
	c.Nonce = nonce
	c.KeyID = keyID
	c.FieldNames = fieldNames
	now := time.Now().UTC()
	c.RotatedAt = &now
	return nil
}

func (s *fakeCredentialStore) Delete(_ context.Context, tenantID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := credKey(tenantID, name)
	if _, ok := s.byKey[key]; !ok {
		return store.ErrNotFound
	}
	delete(s.byKey, key)
	return nil
}

var _ store.CredentialStore = (*fakeCredentialStore)(nil)

// ---------------------------------------------------------------------------
// connectors
// ---------------------------------------------------------------------------

type fakeConnectorStore struct {
	mu    sync.Mutex
	byID  map[string]*store.Connector
	order []string
	seq   int
}

func (s *fakeConnectorStore) Create(_ context.Context, c *store.Connector) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.byID {
		if existing.TenantID == c.TenantID && existing.Slug == c.Slug && existing.DeletedAt == nil {
			return store.ErrConflict
		}
	}
	s.seq++
	c.ID = fmt.Sprintf("connector-%d", s.seq)
	now := time.Now().UTC()
	c.CreatedAt, c.UpdatedAt = now, now
	if c.Status == "" {
		c.Status = "unknown"
	}
	cp := *c
	s.byID[c.ID] = &cp
	s.order = append(s.order, c.ID)
	return nil
}

func (s *fakeConnectorStore) Get(_ context.Context, tenantID, id string) (*store.Connector, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[id]
	if !ok || c.TenantID != tenantID || c.DeletedAt != nil {
		return nil, store.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (s *fakeConnectorStore) GetBySlug(_ context.Context, tenantID, slug string) (*store.Connector, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.byID {
		if c.TenantID == tenantID && c.Slug == slug && c.DeletedAt == nil {
			cp := *c
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *fakeConnectorStore) List(_ context.Context, tenantID string) ([]*store.Connector, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.Connector
	for _, id := range s.order {
		c := s.byID[id]
		if c.TenantID != tenantID || c.DeletedAt != nil {
			continue
		}
		cp := *c
		out = append(out, &cp)
	}
	return out, nil
}

func (s *fakeConnectorStore) Update(_ context.Context, c *store.Connector) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[c.ID]
	if !ok || existing.TenantID != c.TenantID || existing.DeletedAt != nil {
		return store.ErrNotFound
	}
	if c.Slug != existing.Slug {
		for _, other := range s.byID {
			if other.ID != c.ID && other.TenantID == c.TenantID && other.Slug == c.Slug && other.DeletedAt == nil {
				return store.ErrConflict
			}
		}
	}
	c.UpdatedAt = time.Now().UTC()
	c.CreatedAt = existing.CreatedAt
	cp := *c
	s.byID[c.ID] = &cp
	return nil
}

func (s *fakeConnectorStore) SoftDelete(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[id]
	if !ok || c.TenantID != tenantID || c.DeletedAt != nil {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	c.DeletedAt = &now
	c.UpdatedAt = now
	return nil
}

// alive reports whether id names a connector that exists and has not
// been soft-deleted. fakeToolCacheStore uses it to mirror the
// EXISTS-against-connectors filter internal/store/postgres's tool_cache
// queries apply, so a soft-deleted connector's cached tools disappear
// from ListByConnector/ListByTenant here too.
func (s *fakeConnectorStore) alive(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[id]
	return ok && c.DeletedAt == nil
}

var _ store.ConnectorStore = (*fakeConnectorStore)(nil)

// ---------------------------------------------------------------------------
// agent profiles
// ---------------------------------------------------------------------------

type fakeAgentProfileStore struct {
	mu     sync.Mutex
	byID   map[string]*store.AgentProfile
	order  []string
	seq    int
	tools  map[string][]store.ProfileTool  // profileID -> tools
	skills map[string][]store.ProfileSkill // profileID -> skill attachments
}

func (s *fakeAgentProfileStore) Create(_ context.Context, p *store.AgentProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.byID {
		if existing.TenantID == p.TenantID && existing.Slug == p.Slug && existing.DeletedAt == nil {
			return store.ErrConflict
		}
	}
	s.seq++
	p.ID = fmt.Sprintf("profile-%d", s.seq)
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	cp := *p
	s.byID[p.ID] = &cp
	s.order = append(s.order, p.ID)
	return nil
}

func (s *fakeAgentProfileStore) Get(_ context.Context, tenantID, id string) (*store.AgentProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok || p.TenantID != tenantID || p.DeletedAt != nil {
		return nil, store.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (s *fakeAgentProfileStore) GetBySlug(_ context.Context, tenantID, slug string) (*store.AgentProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.byID {
		if p.TenantID == tenantID && p.Slug == slug && p.DeletedAt == nil {
			cp := *p
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *fakeAgentProfileStore) List(_ context.Context, tenantID string) ([]*store.AgentProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.AgentProfile
	for _, id := range s.order {
		p := s.byID[id]
		if p.TenantID != tenantID || p.DeletedAt != nil {
			continue
		}
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

func (s *fakeAgentProfileStore) Update(_ context.Context, p *store.AgentProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[p.ID]
	if !ok || existing.TenantID != p.TenantID || existing.DeletedAt != nil {
		return store.ErrNotFound
	}
	p.UpdatedAt = time.Now().UTC()
	p.CreatedAt = existing.CreatedAt
	p.Slug = existing.Slug
	cp := *p
	s.byID[p.ID] = &cp
	return nil
}

func (s *fakeAgentProfileStore) SoftDelete(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok || p.TenantID != tenantID || p.DeletedAt != nil {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	p.DeletedAt = &now
	p.UpdatedAt = now
	return nil
}

func (s *fakeAgentProfileStore) SetTools(_ context.Context, tenantID, profileID string, tools []store.ProfileTool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[profileID]
	if !ok || p.TenantID != tenantID || p.DeletedAt != nil {
		return store.ErrNotFound
	}
	cp := make([]store.ProfileTool, len(tools))
	copy(cp, tools)
	s.tools[profileID] = cp
	return nil
}

func (s *fakeAgentProfileStore) GetTools(_ context.Context, tenantID, profileID string) ([]store.ProfileTool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.byID[profileID]; !ok || p.TenantID != tenantID {
		return nil, nil
	}
	out := make([]store.ProfileTool, len(s.tools[profileID]))
	copy(out, s.tools[profileID])
	return out, nil
}

func (s *fakeAgentProfileStore) SetSkills(_ context.Context, profileID string, items []store.ProfileSkill) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[profileID]
	if !ok || p.DeletedAt != nil {
		return store.ErrNotFound
	}
	cp := make([]store.ProfileSkill, len(items))
	copy(cp, items)
	s.skills[profileID] = cp
	return nil
}

func (s *fakeAgentProfileStore) GetSkills(_ context.Context, profileID string) ([]store.ProfileSkill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.ProfileSkill, len(s.skills[profileID]))
	copy(out, s.skills[profileID])
	return out, nil
}

var _ store.AgentProfileStore = (*fakeAgentProfileStore)(nil)

// ---------------------------------------------------------------------------
// tool cache
// ---------------------------------------------------------------------------

type fakeToolCacheStore struct {
	mu    sync.Mutex
	items map[string]*store.CachedTool // key: tenant|connector|namespace|name
	// connectors backs the soft-delete filter on ListByConnector/
	// ListByTenant (see fakeConnectorStore.alive). Nil is a valid no-op
	// (no filtering) for any test that builds a bare fakeToolCacheStore.
	connectors *fakeConnectorStore
}

func toolKey(t *store.CachedTool) string {
	return t.TenantID + "|" + t.ConnectorID + "|" + t.ToolNamespace + "|" + t.ToolName
}

func (s *fakeToolCacheStore) Upsert(_ context.Context, tools ...*store.CachedTool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range tools {
		cp := *t
		s.items[toolKey(t)] = &cp
	}
	return nil
}

func (s *fakeToolCacheStore) ListByConnector(_ context.Context, tenantID, connectorID string) ([]*store.CachedTool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connectors != nil && !s.connectors.alive(connectorID) {
		return nil, nil
	}
	var out []*store.CachedTool
	for _, t := range s.items {
		if t.TenantID == tenantID && t.ConnectorID == connectorID {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *fakeToolCacheStore) ListByTenant(_ context.Context, tenantID string) ([]*store.CachedTool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.CachedTool
	for _, t := range s.items {
		if t.TenantID == tenantID && (s.connectors == nil || s.connectors.alive(t.ConnectorID)) {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *fakeToolCacheStore) Search(_ context.Context, tenantID, _ string, _ int, includeStale bool) ([]*store.CachedTool, error) {
	rows, err := s.ListByTenant(context.Background(), tenantID)
	if err != nil || includeStale {
		return rows, err
	}
	out := make([]*store.CachedTool, 0, len(rows))
	for _, ct := range rows {
		if !ct.Stale {
			out = append(out, ct)
		}
	}
	return out, nil
}

func (s *fakeToolCacheStore) MarkStale(_ context.Context, connectorID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.items {
		if t.ConnectorID == connectorID {
			t.Stale = true
		}
	}
	return nil
}

func (s *fakeToolCacheStore) DeleteByConnector(_ context.Context, connectorID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.items {
		if t.ConnectorID == connectorID {
			delete(s.items, k)
		}
	}
	return nil
}

func (s *fakeToolCacheStore) DeleteExpired(context.Context) (int64, error) { return 0, nil }

var _ store.ToolCacheStore = (*fakeToolCacheStore)(nil)

// ---------------------------------------------------------------------------
// models
// ---------------------------------------------------------------------------

// fakeModelStore mirrors internal/store/postgres/models.go: a row with
// TenantID "" is a platform default every tenant can read; a tenant's own
// row of the same name wins; Update/SoftDelete address a row only within
// its own TenantID.
type fakeModelStore struct {
	mu    sync.Mutex
	byID  map[string]*store.Model
	order []string
	seq   int
}

func fakeModelVisible(m *store.Model, tenantID string) bool {
	return m.DeletedAt == nil && (m.TenantID == tenantID || m.TenantID == "")
}

func (s *fakeModelStore) Create(_ context.Context, m *store.Model) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.byID {
		if existing.TenantID == m.TenantID && existing.Name == m.Name && existing.DeletedAt == nil {
			return store.ErrConflict
		}
	}
	s.seq++
	m.ID = fmt.Sprintf("model-%d", s.seq)
	now := time.Now().UTC()
	m.CreatedAt, m.UpdatedAt = now, now
	cp := *m
	s.byID[m.ID] = &cp
	s.order = append(s.order, m.ID)
	return nil
}

func (s *fakeModelStore) Get(_ context.Context, tenantID, id string) (*store.Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok || !fakeModelVisible(m, tenantID) {
		return nil, store.ErrNotFound
	}
	cp := *m
	return &cp, nil
}

func (s *fakeModelStore) GetByName(_ context.Context, tenantID, name string) (*store.Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var platform *store.Model
	for _, id := range s.order {
		m := s.byID[id]
		if !fakeModelVisible(m, tenantID) || m.Name != name {
			continue
		}
		if m.TenantID == tenantID && tenantID != "" {
			cp := *m
			return &cp, nil
		}
		platform = m
	}
	if platform == nil {
		return nil, store.ErrNotFound
	}
	cp := *platform
	return &cp, nil
}

func (s *fakeModelStore) List(_ context.Context, tenantID string, opts store.ListOptions) ([]*store.Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byName := map[string]*store.Model{}
	for _, id := range s.order {
		m := s.byID[id]
		if !fakeModelVisible(m, tenantID) || (opts.EnabledOnly && !m.Enabled) {
			continue
		}
		if prev, ok := byName[m.Name]; ok && prev.TenantID != "" {
			continue
		}
		byName[m.Name] = m
	}
	out := make([]*store.Model, 0, len(byName))
	for _, m := range byName {
		cp := *m
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *fakeModelStore) Update(_ context.Context, m *store.Model) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[m.ID]
	if !ok || existing.TenantID != m.TenantID || existing.DeletedAt != nil {
		return store.ErrNotFound
	}
	for _, other := range s.byID {
		if other.ID != m.ID && other.TenantID == m.TenantID && other.Name == m.Name && other.DeletedAt == nil {
			return store.ErrConflict
		}
	}
	m.CreatedAt = existing.CreatedAt
	m.UpdatedAt = time.Now().UTC()
	cp := *m
	s.byID[m.ID] = &cp
	return nil
}

func (s *fakeModelStore) SoftDelete(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok || m.TenantID != tenantID || m.DeletedAt != nil {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	m.DeletedAt = &now
	m.UpdatedAt = now
	return nil
}

var _ store.ModelStore = (*fakeModelStore)(nil)

// ---------------------------------------------------------------------------
// skills
// ---------------------------------------------------------------------------

type fakeSkillStore struct {
	mu       sync.Mutex
	byID     map[string]*store.Skill
	order    []string
	seq      int
	versions map[string][]store.SkillVersion // skill id -> versions, oldest first
}

func fakeSkillVisible(sk *store.Skill, tenantID string) bool {
	return sk.DeletedAt == nil && (sk.TenantID == tenantID || sk.TenantID == "")
}

func (s *fakeSkillStore) Create(_ context.Context, sk *store.Skill, files []store.SkillFile, createdBy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.byID {
		if existing.TenantID == sk.TenantID && existing.Name == sk.Name && existing.DeletedAt == nil {
			return store.ErrConflict
		}
	}
	s.seq++
	sk.ID = fmt.Sprintf("skill-%d", s.seq)
	sk.LatestVersion = 1
	now := time.Now().UTC()
	sk.CreatedAt, sk.UpdatedAt = now, now
	cp := *sk
	s.byID[sk.ID] = &cp
	s.order = append(s.order, sk.ID)
	s.versions[sk.ID] = []store.SkillVersion{{
		SkillID: sk.ID, Version: 1, Files: append([]store.SkillFile(nil), files...),
		CreatedBy: createdBy, CreatedAt: now,
	}}
	return nil
}

func (s *fakeSkillStore) Get(_ context.Context, tenantID, id string) (*store.Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sk, ok := s.byID[id]
	if !ok || !fakeSkillVisible(sk, tenantID) {
		return nil, store.ErrNotFound
	}
	cp := *sk
	return &cp, nil
}

func (s *fakeSkillStore) GetByName(_ context.Context, tenantID, name string) (*store.Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var platform *store.Skill
	for _, id := range s.order {
		sk := s.byID[id]
		if !fakeSkillVisible(sk, tenantID) || sk.Name != name {
			continue
		}
		if sk.TenantID == tenantID && tenantID != "" {
			cp := *sk
			return &cp, nil
		}
		platform = sk
	}
	if platform == nil {
		return nil, store.ErrNotFound
	}
	cp := *platform
	return &cp, nil
}

func (s *fakeSkillStore) List(_ context.Context, tenantID string, opts store.SkillListOptions) ([]store.Skill, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byName := map[string]*store.Skill{}
	for _, id := range s.order {
		sk := s.byID[id]
		if !fakeSkillVisible(sk, tenantID) || (opts.Kind != "" && sk.Kind != opts.Kind) {
			continue
		}
		if prev, ok := byName[sk.Name]; ok && prev.TenantID != "" {
			continue
		}
		byName[sk.Name] = sk
	}
	out := make([]store.Skill, 0, len(byName))
	for _, sk := range byName {
		out = append(out, *sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, len(out), nil
}

func (s *fakeSkillStore) Update(_ context.Context, sk *store.Skill) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[sk.ID]
	if !ok || existing.TenantID != sk.TenantID || existing.DeletedAt != nil {
		return store.ErrNotFound
	}
	existing.Description = sk.Description
	existing.Enabled = sk.Enabled
	existing.Metadata = sk.Metadata
	existing.Arguments = sk.Arguments
	existing.UpdatedAt = time.Now().UTC()
	cp := *existing
	*sk = cp
	return nil
}

func (s *fakeSkillStore) SoftDelete(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sk, ok := s.byID[id]
	if !ok || sk.TenantID != tenantID || sk.DeletedAt != nil {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	sk.DeletedAt = &now
	sk.UpdatedAt = now
	return nil
}

func (s *fakeSkillStore) AddVersion(_ context.Context, tenantID, id string, files []store.SkillFile, createdBy string) (*store.SkillVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sk, ok := s.byID[id]
	if !ok || !fakeSkillVisible(sk, tenantID) {
		return nil, store.ErrNotFound
	}

	skillMD, err := skills.FindSkillMD(files)
	if err != nil {
		return nil, err
	}
	fm, err := skills.ParseFrontmatter(skillMD, sk.Name)
	if err != nil {
		return nil, err
	}

	newVersion := sk.LatestVersion + 1
	now := time.Now().UTC()
	v := store.SkillVersion{SkillID: id, Version: newVersion, Files: append([]store.SkillFile(nil), files...), CreatedBy: createdBy, CreatedAt: now}
	s.versions[id] = append(s.versions[id], v)

	sk.LatestVersion = newVersion
	sk.Description = skills.DescriptionFrom(fm)
	sk.Frontmatter = fm
	sk.UpdatedAt = now

	return &v, nil
}

func (s *fakeSkillStore) GetVersion(_ context.Context, skillID string, version int) (*store.SkillVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.versions[skillID] {
		if v.Version == version {
			cp := v
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *fakeSkillStore) ListVersions(_ context.Context, skillID string) ([]store.SkillVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.versions[skillID]
	out := make([]store.SkillVersion, len(src))
	for i, v := range src {
		out[len(src)-1-i] = store.SkillVersion{SkillID: v.SkillID, Version: v.Version, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt}
	}
	return out, nil
}

var _ store.SkillStore = (*fakeSkillStore)(nil)

// ---------------------------------------------------------------------------
// model catalog
// ---------------------------------------------------------------------------

// fakeModelCatalogStore is a minimal, map-backed store.ModelCatalogStore.
// Connect reuses credentials' and tenantModels' own Create methods (rather
// than reimplementing their id/timestamp bookkeeping), so it exercises the
// same conflict rules those fakes already enforce; the one thing it does
// itself is the pre-check that turns a matching catalog_model_id into
// "exists" instead of a conflict.
type fakeModelCatalogStore struct {
	mu        sync.Mutex
	providers map[string]*store.ModelCatalogProvider
	pOrder    []string
	models    map[string]*store.ModelCatalogModel
	mOrder    []string
	seq       int

	credentials  *fakeCredentialStore
	tenantModels *fakeModelStore
}

func (s *fakeModelCatalogStore) modelsForProvider(providerID string) []store.ModelCatalogModel {
	var out []store.ModelCatalogModel
	for _, id := range s.mOrder {
		m := s.models[id]
		if m.ProviderID == providerID {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SuggestedName < out[j].SuggestedName })
	return out
}

func (s *fakeModelCatalogStore) ListProviders(context.Context) ([]*store.ModelCatalogProvider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*store.ModelCatalogProvider, 0, len(s.pOrder))
	for _, id := range s.pOrder {
		p := *s.providers[id]
		p.Models = s.modelsForProvider(p.ID)
		out = append(out, &p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (s *fakeModelCatalogStore) GetProvider(_ context.Context, id string) (*store.ModelCatalogProvider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.providers[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *p
	cp.Models = s.modelsForProvider(id)
	return &cp, nil
}

func (s *fakeModelCatalogStore) CreateProvider(_ context.Context, p *store.ModelCatalogProvider) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.providers {
		if existing.Slug == p.Slug {
			return store.ErrConflict
		}
	}
	s.seq++
	p.ID = fmt.Sprintf("catalog-provider-%d", s.seq)
	if p.Vendor == "" {
		p.Vendor = "openai_compat"
	}
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	cp := *p
	s.providers[p.ID] = &cp
	s.pOrder = append(s.pOrder, p.ID)
	return nil
}

func (s *fakeModelCatalogStore) UpdateProvider(_ context.Context, p *store.ModelCatalogProvider) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.providers[p.ID]
	if !ok {
		return store.ErrNotFound
	}
	for _, other := range s.providers {
		if other.ID != p.ID && other.Slug == p.Slug {
			return store.ErrConflict
		}
	}
	p.CreatedAt = existing.CreatedAt
	p.UpdatedAt = time.Now().UTC()
	cp := *p
	cp.Models = nil
	s.providers[p.ID] = &cp
	return nil
}

func (s *fakeModelCatalogStore) DeleteProvider(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.providers[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.providers, id)
	kept := s.pOrder[:0]
	for _, pid := range s.pOrder {
		if pid != id {
			kept = append(kept, pid)
		}
	}
	s.pOrder = kept
	// ON DELETE CASCADE (its models) and ON DELETE SET NULL (any tenant
	// model that referenced one of them), mirroring the Postgres schema.
	for mid, m := range s.models {
		if m.ProviderID != id {
			continue
		}
		delete(s.models, mid)
		s.clearCatalogModelID(mid)
	}
	kept2 := s.mOrder[:0]
	for _, mid := range s.mOrder {
		if _, ok := s.models[mid]; ok {
			kept2 = append(kept2, mid)
		}
	}
	s.mOrder = kept2
	return nil
}

func (s *fakeModelCatalogStore) GetModel(_ context.Context, id string) (*store.ModelCatalogModel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.models[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *m
	return &cp, nil
}

func (s *fakeModelCatalogStore) CreateModel(_ context.Context, m *store.ModelCatalogModel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.models {
		if existing.SuggestedName == m.SuggestedName || (existing.ProviderID == m.ProviderID && existing.ModelID == m.ModelID) {
			return store.ErrConflict
		}
	}
	s.seq++
	m.ID = fmt.Sprintf("catalog-model-%d", s.seq)
	now := time.Now().UTC()
	m.CreatedAt, m.UpdatedAt = now, now
	cp := *m
	s.models[m.ID] = &cp
	s.mOrder = append(s.mOrder, m.ID)
	return nil
}

func (s *fakeModelCatalogStore) UpdateModel(_ context.Context, m *store.ModelCatalogModel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.models[m.ID]
	if !ok {
		return store.ErrNotFound
	}
	for _, other := range s.models {
		if other.ID != m.ID && (other.SuggestedName == m.SuggestedName || (other.ProviderID == existing.ProviderID && other.ModelID == m.ModelID)) {
			return store.ErrConflict
		}
	}
	m.ProviderID = existing.ProviderID
	m.CreatedAt = existing.CreatedAt
	m.UpdatedAt = time.Now().UTC()
	cp := *m
	s.models[m.ID] = &cp
	return nil
}

func (s *fakeModelCatalogStore) DeleteModel(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.models[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.models, id)
	kept := s.mOrder[:0]
	for _, mid := range s.mOrder {
		if mid != id {
			kept = append(kept, mid)
		}
	}
	s.mOrder = kept
	s.clearCatalogModelID(id)
	return nil
}

// clearCatalogModelID mirrors ON DELETE SET NULL: every live tenant model
// row whose CatalogModelID is catalogModelID loses that link. Caller must
// hold s.mu.
func (s *fakeModelCatalogStore) clearCatalogModelID(catalogModelID string) {
	s.tenantModels.mu.Lock()
	defer s.tenantModels.mu.Unlock()
	for _, m := range s.tenantModels.byID {
		if m.CatalogModelID == catalogModelID {
			m.CatalogModelID = ""
		}
	}
}

func (s *fakeModelCatalogStore) CountTenantUsage(_ context.Context, catalogModelID string) (int, error) {
	s.tenantModels.mu.Lock()
	defer s.tenantModels.mu.Unlock()
	var n int
	for _, m := range s.tenantModels.byID {
		if m.CatalogModelID == catalogModelID && m.DeletedAt == nil {
			n++
		}
	}
	return n, nil
}

func (s *fakeModelCatalogStore) Connect(ctx context.Context, tenantID string, cred store.CatalogConnectCredential, models []store.CatalogConnectModel) (*store.CatalogConnectResult, error) {
	result := &store.CatalogConnectResult{}

	switch {
	case cred.Existing != "":
		if _, err := s.credentials.Get(ctx, tenantID, cred.Existing); err != nil {
			return nil, err
		}
		result.CredentialName = cred.Existing
		result.CredentialCreated = false
	case cred.New != nil:
		c := &store.Credential{
			TenantID: tenantID, Name: cred.New.Name, Type: cred.New.Type,
			Ciphertext: cred.New.Ciphertext, Nonce: cred.New.Nonce, KeyID: cred.New.KeyID, FieldNames: cred.New.FieldNames,
		}
		if err := s.credentials.Create(ctx, c); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return nil, &store.CredentialNameConflictError{Name: cred.New.Name}
			}
			return nil, err
		}
		result.CredentialName = cred.New.Name
		result.CredentialCreated = true
	default:
		return nil, fmt.Errorf("fakeModelCatalogStore: connect: credential spec has neither Existing nor New set")
	}

	for _, cm := range models {
		mr, err := s.connectOne(ctx, tenantID, result.CredentialName, cm)
		if err != nil {
			return nil, err
		}
		result.Models = append(result.Models, *mr)
	}
	return result, nil
}

func (s *fakeModelCatalogStore) connectOne(ctx context.Context, tenantID, credentialName string, cm store.CatalogConnectModel) (*store.CatalogConnectModelResult, error) {
	s.tenantModels.mu.Lock()
	for _, m := range s.tenantModels.byID {
		if m.TenantID != tenantID || m.Name != cm.Name || m.DeletedAt != nil {
			continue
		}
		if m.CatalogModelID == cm.CatalogModelID {
			s.tenantModels.mu.Unlock()
			return &store.CatalogConnectModelResult{CatalogModelID: cm.CatalogModelID, ModelID: m.ID, Name: cm.Name, Status: "exists"}, nil
		}
		s.tenantModels.mu.Unlock()
		return nil, &store.ModelNameConflictError{Name: cm.Name}
	}
	s.tenantModels.mu.Unlock()

	m := &store.Model{
		TenantID: tenantID, Name: cm.Name, Description: cm.Description, Enabled: true,
		Targets: []store.ModelTarget{{Vendor: "openai_compat", Model: cm.ModelID, BaseURL: cm.BaseURL, Credential: credentialName, Label: cm.Label}},
		Price:   cm.Price, Metadata: map[string]any{}, CatalogModelID: cm.CatalogModelID, Capabilities: cm.Capabilities,
	}
	if err := s.tenantModels.Create(ctx, m); err != nil {
		return nil, err
	}
	return &store.CatalogConnectModelResult{CatalogModelID: cm.CatalogModelID, ModelID: m.ID, Name: cm.Name, Status: "created"}, nil
}

// ApplyPrices mirrors the Postgres backend's semantics: every update is
// applied to its catalog model (ErrNotFound aborts before anything is
// written), and -- when updateTenantModels is true -- every live tenant
// model whose CatalogModelID matches and whose Price still deep-equals the
// catalog model's price as it stood before this call is rewritten too.
func (s *fakeModelCatalogStore) ApplyPrices(_ context.Context, updates []store.PriceUpdate, updateTenantModels bool) (*store.ApplyPricesResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, u := range updates {
		if _, ok := s.models[u.CatalogModelID]; !ok {
			return nil, store.ErrNotFound
		}
	}

	result := &store.ApplyPricesResult{}
	tenantIDs := make(map[string]struct{})
	for _, u := range updates {
		m := s.models[u.CatalogModelID]
		oldPrice := m.Price
		m.Price = u.Price
		m.UpdatedAt = time.Now().UTC()
		result.CatalogUpdated++

		if updateTenantModels {
			s.tenantModels.mu.Lock()
			for _, tm := range s.tenantModels.byID {
				if tm.CatalogModelID != u.CatalogModelID || tm.DeletedAt != nil {
					continue
				}
				if !pricesEqual(tm.Price, oldPrice) {
					continue
				}
				tm.Price = u.Price
				tm.UpdatedAt = time.Now().UTC()
				result.TenantUpdated++
				tenantIDs[tm.TenantID] = struct{}{}
			}
			s.tenantModels.mu.Unlock()
		}
	}
	for tid := range tenantIDs {
		result.TenantIDs = append(result.TenantIDs, tid)
	}
	return result, nil
}

// pricesEqual is defined in modelcatalog.go (same package) -- reused here
// so this fake's ApplyPrices matches the real handler's / Postgres
// backend's nil-safe "still equals the old catalog price" rule exactly.

var _ store.ModelCatalogStore = (*fakeModelCatalogStore)(nil)
