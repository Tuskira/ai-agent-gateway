// Package dptest is in-memory test support for the MCP plane's own
// tests: a store.Store whose repositories keep rows in maps.
//
// It is not a second production backend and is not run against
// pkg/store/storetest. Its only job is to let the plane's unit tests
// exercise routing, profile enforcement and the transport without a
// Postgres, while the end-to-end test (test/e2e) covers the real one.
package dptest

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/skills"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Store is an in-memory store.Store.
type Store struct {
	mu sync.Mutex

	tenants    map[string]*store.Tenant
	connectors map[string]*store.Connector
	catalog    map[string]*store.MCPCatalogEntry
	profiles   map[string]*store.AgentProfile
	profileIdx map[string][]store.ProfileTool // profile id -> tools
	tools      []*store.CachedTool
	models     map[string]*store.Model
	skills     map[string]*store.Skill
	versions   map[string][]store.SkillVersion // skill id -> versions, oldest first
	profileSkl map[string][]store.ProfileSkill // profile id -> attachments

	users    map[string]*store.User
	sessions map[string]*store.UserSession
	audit    []*store.AuthAuditEntry

	nextID int
}

// New returns an empty Store.
func New() *Store {
	return &Store{
		tenants:    make(map[string]*store.Tenant),
		connectors: make(map[string]*store.Connector),
		catalog:    make(map[string]*store.MCPCatalogEntry),
		profiles:   make(map[string]*store.AgentProfile),
		profileIdx: make(map[string][]store.ProfileTool),
		models:     make(map[string]*store.Model),
		skills:     make(map[string]*store.Skill),
		versions:   make(map[string][]store.SkillVersion),
		profileSkl: make(map[string][]store.ProfileSkill),
		users:      make(map[string]*store.User),
		sessions:   make(map[string]*store.UserSession),
	}
}

func (s *Store) id(prefix string) string {
	s.nextID++
	return prefix + "-" + strconv.Itoa(s.nextID)
}

// Tenants implements store.Store.
func (s *Store) Tenants() store.TenantStore { return (*tenantStore)(s) }

// APIKeys implements store.Store. The MCP plane never reads API keys
// (authentication happens upstream of it), so this facet is unused.
func (s *Store) APIKeys() store.APIKeyStore { return nil }

// Credentials implements store.Store; unused by the MCP plane's tests.
func (s *Store) Credentials() store.CredentialStore { return nil }

// Connectors implements store.Store.
func (s *Store) Connectors() store.ConnectorStore { return (*connectorStore)(s) }

// AgentProfiles implements store.Store.
func (s *Store) AgentProfiles() store.AgentProfileStore { return (*agentProfileStore)(s) }

// ToolCache implements store.Store.
func (s *Store) ToolCache() store.ToolCacheStore { return (*toolCacheStore)(s) }

// Models implements store.Store.
func (s *Store) Models() store.ModelStore { return (*modelStore)(s) }

// ModelCatalog implements store.Store; unused by the MCP plane's tests.
func (s *Store) ModelCatalog() store.ModelCatalogStore { return nil }

// Skills implements store.Store.
func (s *Store) Skills() store.SkillStore { return (*skillStore)(s) }

// Users implements store.Store. Together with UserSessions and AuthAudit
// it is a full in-memory implementation (handler tests use it), mirroring
// the semantics pkg/store/storetest asserts.
func (s *Store) Users() store.UserStore { return (*userStore)(s) }

// UserSessions implements store.Store.
func (s *Store) UserSessions() store.UserSessionStore { return (*userSessionStore)(s) }

// AuthAudit implements store.Store.
func (s *Store) AuthAudit() store.AuthAuditStore { return (*authAuditStore)(s) }

// Migrate implements store.Store.
func (s *Store) Migrate(context.Context) error { return nil }

// Ping implements store.Store.
func (s *Store) Ping(context.Context) error { return nil }

// Close implements store.Store.
func (s *Store) Close() error { return nil }

var _ store.Store = (*Store)(nil)

// ---------------------------------------------------------------------------

type tenantStore Store

func (t *tenantStore) Create(_ context.Context, tn *store.Tenant) error {
	s := (*Store)(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	if tn.ID == "" {
		tn.ID = s.id("tenant")
	}
	tn.CreatedAt = time.Now()
	s.tenants[tn.ID] = tn
	return nil
}

func (t *tenantStore) GetBySlug(_ context.Context, slug string) (*store.Tenant, error) {
	s := (*Store)(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, tn := range s.tenants {
		if tn.Slug == slug {
			return tn, nil
		}
	}
	return nil, store.ErrNotFound
}

func (t *tenantStore) List(context.Context) ([]*store.Tenant, error) {
	s := (*Store)(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*store.Tenant, 0, len(s.tenants))
	for _, tn := range s.tenants {
		out = append(out, tn)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---------------------------------------------------------------------------

type connectorStore Store

func (c *connectorStore) Create(_ context.Context, conn *store.Connector) error {
	s := (*Store)(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	if conn.ID == "" {
		conn.ID = s.id("conn")
	}
	if conn.Status == "" {
		conn.Status = "unknown"
	}
	conn.CreatedAt, conn.UpdatedAt = time.Now(), time.Now()
	s.connectors[conn.ID] = conn
	return nil
}

func (c *connectorStore) Get(_ context.Context, tenantID, id string) (*store.Connector, error) {
	s := (*Store)(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	conn, ok := s.connectors[id]
	if !ok || conn.TenantID != tenantID || conn.DeletedAt != nil {
		return nil, store.ErrNotFound
	}
	return conn, nil
}

func (c *connectorStore) GetBySlug(_ context.Context, tenantID, slug string) (*store.Connector, error) {
	s := (*Store)(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, conn := range s.connectors {
		if conn.TenantID == tenantID && conn.Slug == slug && conn.DeletedAt == nil {
			return conn, nil
		}
	}
	return nil, store.ErrNotFound
}

func (c *connectorStore) List(_ context.Context, tenantID string) ([]*store.Connector, error) {
	s := (*Store)(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*store.Connector, 0, len(s.connectors))
	for _, conn := range s.connectors {
		if conn.TenantID == tenantID && conn.DeletedAt == nil {
			out = append(out, conn)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (c *connectorStore) Update(_ context.Context, conn *store.Connector) error {
	s := (*Store)(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.connectors[conn.ID]
	if !ok || existing.TenantID != conn.TenantID || existing.DeletedAt != nil {
		return store.ErrNotFound
	}
	conn.UpdatedAt = time.Now()
	s.connectors[conn.ID] = conn
	return nil
}

func (c *connectorStore) SoftDelete(_ context.Context, tenantID, id string) error {
	s := (*Store)(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	conn, ok := s.connectors[id]
	if !ok || conn.TenantID != tenantID || conn.DeletedAt != nil {
		return store.ErrNotFound
	}
	now := time.Now()
	conn.DeletedAt = &now
	return nil
}

// ---------------------------------------------------------------------------

type agentProfileStore Store

func (a *agentProfileStore) Create(_ context.Context, p *store.AgentProfile) error {
	s := (*Store)(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == "" {
		p.ID = s.id("profile")
	}
	p.CreatedAt, p.UpdatedAt = time.Now(), time.Now()
	s.profiles[p.ID] = p
	return nil
}

func (a *agentProfileStore) Get(_ context.Context, tenantID, id string) (*store.AgentProfile, error) {
	s := (*Store)(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[id]
	if !ok || p.TenantID != tenantID || p.DeletedAt != nil {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func (a *agentProfileStore) GetBySlug(_ context.Context, tenantID, slug string) (*store.AgentProfile, error) {
	s := (*Store)(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.profiles {
		if p.TenantID == tenantID && p.Slug == slug && p.DeletedAt == nil {
			return p, nil
		}
	}
	return nil, store.ErrNotFound
}

func (a *agentProfileStore) List(_ context.Context, tenantID string) ([]*store.AgentProfile, error) {
	s := (*Store)(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*store.AgentProfile, 0, len(s.profiles))
	for _, p := range s.profiles {
		if p.TenantID == tenantID && p.DeletedAt == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (a *agentProfileStore) Update(_ context.Context, p *store.AgentProfile) error {
	s := (*Store)(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.profiles[p.ID]; !ok {
		return store.ErrNotFound
	}
	p.UpdatedAt = time.Now()
	s.profiles[p.ID] = p
	return nil
}

func (a *agentProfileStore) SoftDelete(_ context.Context, tenantID, id string) error {
	s := (*Store)(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[id]
	if !ok || p.TenantID != tenantID || p.DeletedAt != nil {
		return store.ErrNotFound
	}
	now := time.Now()
	p.DeletedAt = &now
	return nil
}

func (a *agentProfileStore) SetTools(_ context.Context, tenantID, profileID string, tools []store.ProfileTool) error {
	s := (*Store)(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[profileID]
	if !ok || p.TenantID != tenantID || p.DeletedAt != nil {
		return store.ErrNotFound
	}
	s.profileIdx[profileID] = append([]store.ProfileTool(nil), tools...)
	return nil
}

func (a *agentProfileStore) GetTools(_ context.Context, tenantID, profileID string) ([]store.ProfileTool, error) {
	s := (*Store)(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[profileID]
	if !ok || p.TenantID != tenantID || p.DeletedAt != nil {
		return nil, store.ErrNotFound
	}
	return append([]store.ProfileTool(nil), s.profileIdx[profileID]...), nil
}

func (a *agentProfileStore) SetSkills(_ context.Context, profileID string, items []store.ProfileSkill) error {
	s := (*Store)(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[profileID]
	if !ok || p.DeletedAt != nil {
		return store.ErrNotFound
	}
	s.profileSkl[profileID] = append([]store.ProfileSkill(nil), items...)
	return nil
}

func (a *agentProfileStore) GetSkills(_ context.Context, profileID string) ([]store.ProfileSkill, error) {
	s := (*Store)(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.ProfileSkill(nil), s.profileSkl[profileID]...), nil
}

// ---------------------------------------------------------------------------

type toolCacheStore Store

func (t *toolCacheStore) Upsert(_ context.Context, tools ...*store.CachedTool) error {
	s := (*Store)(t)
	s.mu.Lock()
	defer s.mu.Unlock()
next:
	for _, ct := range tools {
		for i, existing := range s.tools {
			if existing.TenantID == ct.TenantID && existing.ConnectorID == ct.ConnectorID &&
				existing.ToolNamespace == ct.ToolNamespace && existing.ToolName == ct.ToolName {
				s.tools[i] = ct
				continue next
			}
		}
		s.tools = append(s.tools, ct)
	}
	return nil
}

func (t *toolCacheStore) ListByConnector(_ context.Context, tenantID, connectorID string) ([]*store.CachedTool, error) {
	s := (*Store)(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.CachedTool
	for _, ct := range s.tools {
		if ct.TenantID == tenantID && ct.ConnectorID == connectorID && !connectorDeleted(s, ct.ConnectorID) {
			out = append(out, ct)
		}
	}
	return out, nil
}

func (t *toolCacheStore) ListByTenant(_ context.Context, tenantID string) ([]*store.CachedTool, error) {
	s := (*Store)(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.CachedTool
	for _, ct := range s.tools {
		if ct.TenantID == tenantID && !connectorDeleted(s, ct.ConnectorID) {
			out = append(out, ct)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ToolName < out[j].ToolName })
	return out, nil
}

// connectorDeleted reports whether connectorID names a connector that
// exists and has been soft-deleted, mirroring the "hide it" rule
// store.ConnectorStore's Get/List apply and the EXISTS-against-connectors
// join internal/store/postgres's tool_cache queries use for the same
// purpose: a soft-deleted connector's cached tools must not surface from
// ListByConnector/ListByTenant (and, through it, Search).
//
// Unlike Postgres, this in-memory store does not enforce the schema's
// connector_id foreign key, and several of this package's own callers
// seed tool_cache rows directly without ever creating the matching
// connector row. So a connector id this store has never seen is treated
// as alive (not filtered) -- only one this store knows to be
// soft-deleted is hidden. Caller must hold s.mu.
func connectorDeleted(s *Store, connectorID string) bool {
	conn, ok := s.connectors[connectorID]
	return ok && conn.DeletedAt != nil
}

func (t *toolCacheStore) Search(ctx context.Context, tenantID, query string, limit int, includeStale bool) ([]*store.CachedTool, error) {
	rows, err := t.ListByTenant(ctx, tenantID)
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

func (t *toolCacheStore) MarkStale(_ context.Context, connectorID string) error {
	s := (*Store)(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ct := range s.tools {
		if ct.ConnectorID == connectorID {
			ct.Stale = true
		}
	}
	return nil
}

func (t *toolCacheStore) DeleteByConnector(_ context.Context, connectorID string) error {
	s := (*Store)(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.tools[:0]
	for _, ct := range s.tools {
		if ct.ConnectorID != connectorID {
			kept = append(kept, ct)
		}
	}
	s.tools = kept
	return nil
}

func (t *toolCacheStore) DeleteExpired(context.Context) (int64, error) {
	s := (*Store)(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var n int64
	kept := s.tools[:0]
	for _, ct := range s.tools {
		if ct.ExpiresAt.Before(now) {
			n++
			continue
		}
		kept = append(kept, ct)
	}
	s.tools = kept
	return n, nil
}

// ---------------------------------------------------------------------------

type modelStore Store

// visible reports whether m is live and readable by tenantID: its own row,
// or a platform row (TenantID "").
func modelVisible(m *store.Model, tenantID string) bool {
	return m.DeletedAt == nil && (m.TenantID == tenantID || m.TenantID == "")
}

func (ms *modelStore) Create(_ context.Context, m *store.Model) error {
	s := (*Store)(ms)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.models {
		if existing.DeletedAt == nil && existing.TenantID == m.TenantID && existing.Name == m.Name {
			return store.ErrConflict
		}
	}
	if m.ID == "" {
		m.ID = s.id("model")
	}
	m.CreatedAt, m.UpdatedAt = time.Now(), time.Now()
	s.models[m.ID] = m
	return nil
}

func (ms *modelStore) Get(_ context.Context, tenantID, id string) (*store.Model, error) {
	s := (*Store)(ms)
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.models[id]
	if !ok || !modelVisible(m, tenantID) {
		return nil, store.ErrNotFound
	}
	return m, nil
}

func (ms *modelStore) GetByName(_ context.Context, tenantID, name string) (*store.Model, error) {
	s := (*Store)(ms)
	s.mu.Lock()
	defer s.mu.Unlock()
	var platform *store.Model
	for _, m := range s.models {
		if !modelVisible(m, tenantID) || m.Name != name {
			continue
		}
		if m.TenantID == tenantID && tenantID != "" {
			return m, nil // the tenant's own row wins
		}
		platform = m
	}
	if platform == nil {
		return nil, store.ErrNotFound
	}
	return platform, nil
}

func (ms *modelStore) List(_ context.Context, tenantID string, opts store.ListOptions) ([]*store.Model, error) {
	s := (*Store)(ms)
	s.mu.Lock()
	defer s.mu.Unlock()
	byName := make(map[string]*store.Model)
	for _, m := range s.models {
		if !modelVisible(m, tenantID) || (opts.EnabledOnly && !m.Enabled) {
			continue
		}
		if prev, ok := byName[m.Name]; ok && prev.TenantID != "" {
			continue // a tenant row already claimed this name
		}
		byName[m.Name] = m
	}
	out := make([]*store.Model, 0, len(byName))
	for _, m := range byName {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (ms *modelStore) Update(_ context.Context, m *store.Model) error {
	s := (*Store)(ms)
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.models[m.ID]
	if !ok || existing.TenantID != m.TenantID || existing.DeletedAt != nil {
		return store.ErrNotFound
	}
	for _, other := range s.models {
		if other.ID != m.ID && other.DeletedAt == nil && other.TenantID == m.TenantID && other.Name == m.Name {
			return store.ErrConflict
		}
	}
	m.CreatedAt = existing.CreatedAt
	m.UpdatedAt = time.Now()
	s.models[m.ID] = m
	return nil
}

func (ms *modelStore) SoftDelete(_ context.Context, tenantID, id string) error {
	s := (*Store)(ms)
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.models[id]
	if !ok || m.TenantID != tenantID || m.DeletedAt != nil {
		return store.ErrNotFound
	}
	now := time.Now()
	m.DeletedAt = &now
	m.UpdatedAt = now
	return nil
}

// ---------------------------------------------------------------------------

type skillStore Store

// skillVisible reports whether sk is live and readable by tenantID: its
// own row, or a platform row (TenantID "").
func skillVisible(sk *store.Skill, tenantID string) bool {
	return sk.DeletedAt == nil && (sk.TenantID == tenantID || sk.TenantID == "")
}

func (sk *skillStore) Create(_ context.Context, m *store.Skill, files []store.SkillFile, createdBy string) error {
	s := (*Store)(sk)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.skills {
		if existing.DeletedAt == nil && existing.TenantID == m.TenantID && existing.Name == m.Name {
			return store.ErrConflict
		}
	}
	if m.ID == "" {
		m.ID = s.id("skill")
	}
	m.LatestVersion = 1
	m.CreatedAt, m.UpdatedAt = time.Now(), time.Now()
	s.skills[m.ID] = m
	s.versions[m.ID] = []store.SkillVersion{{
		SkillID: m.ID, Version: 1, Files: append([]store.SkillFile(nil), files...),
		CreatedBy: createdBy, CreatedAt: m.CreatedAt,
	}}
	return nil
}

func (sk *skillStore) Get(_ context.Context, tenantID, id string) (*store.Skill, error) {
	s := (*Store)(sk)
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.skills[id]
	if !ok || !skillVisible(m, tenantID) {
		return nil, store.ErrNotFound
	}
	return m, nil
}

func (sk *skillStore) GetByName(_ context.Context, tenantID, name string) (*store.Skill, error) {
	s := (*Store)(sk)
	s.mu.Lock()
	defer s.mu.Unlock()
	var platform *store.Skill
	for _, m := range s.skills {
		if !skillVisible(m, tenantID) || m.Name != name {
			continue
		}
		if m.TenantID == tenantID && tenantID != "" {
			return m, nil
		}
		platform = m
	}
	if platform == nil {
		return nil, store.ErrNotFound
	}
	return platform, nil
}

func (sk *skillStore) List(_ context.Context, tenantID string, opts store.SkillListOptions) ([]store.Skill, int, error) {
	s := (*Store)(sk)
	s.mu.Lock()
	defer s.mu.Unlock()
	byName := make(map[string]*store.Skill)
	for _, m := range s.skills {
		if !skillVisible(m, tenantID) || (opts.Kind != "" && m.Kind != opts.Kind) {
			continue
		}
		if prev, ok := byName[m.Name]; ok && prev.TenantID != "" {
			continue
		}
		byName[m.Name] = m
	}
	out := make([]store.Skill, 0, len(byName))
	for _, m := range byName {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, len(out), nil
}

func (sk *skillStore) Update(_ context.Context, m *store.Skill) error {
	s := (*Store)(sk)
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.skills[m.ID]
	if !ok || existing.TenantID != m.TenantID || existing.DeletedAt != nil {
		return store.ErrNotFound
	}
	existing.Description = m.Description
	existing.Enabled = m.Enabled
	existing.Metadata = m.Metadata
	existing.Arguments = m.Arguments
	existing.UpdatedAt = time.Now()
	*m = *existing
	return nil
}

func (sk *skillStore) SoftDelete(_ context.Context, tenantID, id string) error {
	s := (*Store)(sk)
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.skills[id]
	if !ok || m.TenantID != tenantID || m.DeletedAt != nil {
		return store.ErrNotFound
	}
	now := time.Now()
	m.DeletedAt = &now
	m.UpdatedAt = now
	return nil
}

func (sk *skillStore) AddVersion(_ context.Context, tenantID, id string, files []store.SkillFile, createdBy string) (*store.SkillVersion, error) {
	s := (*Store)(sk)
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.skills[id]
	if !ok || !skillVisible(m, tenantID) {
		return nil, store.ErrNotFound
	}

	skillMD, err := skills.FindSkillMD(files)
	if err != nil {
		return nil, err
	}
	fm, err := skills.ParseFrontmatter(skillMD, m.Name)
	if err != nil {
		return nil, err
	}

	newVersion := m.LatestVersion + 1
	now := time.Now()
	v := store.SkillVersion{SkillID: id, Version: newVersion, Files: append([]store.SkillFile(nil), files...), CreatedBy: createdBy, CreatedAt: now}
	s.versions[id] = append(s.versions[id], v)

	m.LatestVersion = newVersion
	m.Description = skills.DescriptionFrom(fm)
	m.Frontmatter = fm
	m.UpdatedAt = now

	return &v, nil
}

func (sk *skillStore) GetVersion(_ context.Context, skillID string, version int) (*store.SkillVersion, error) {
	s := (*Store)(sk)
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

func (sk *skillStore) ListVersions(_ context.Context, skillID string) ([]store.SkillVersion, error) {
	s := (*Store)(sk)
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.versions[skillID]
	out := make([]store.SkillVersion, len(src))
	for i, v := range src {
		out[len(src)-1-i] = store.SkillVersion{SkillID: v.SkillID, Version: v.Version, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt}
	}
	return out, nil
}
