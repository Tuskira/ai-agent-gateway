// Package storetest is the conformance suite every store.Store backend
// must pass.
//
// A new backend is added by implementing store.Store (see pkg/store),
// registering its driver via store.Register in an init(), and then wiring
// this suite up as its test:
//
//	func TestConformance(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) store.Store {
//			// Build and return a Store backed by a fresh, isolated
//			// dataset (e.g. its own schema, database, or in-memory
//			// instance) — subtests below must not see each other's
//			// rows.
//		})
//	}
//
// Run exercises every method on every sub-interface, tenant isolation
// (a row created under tenant A must never be visible through a tenant-
// scoped read for tenant B), unique-constraint conflicts (-> ErrConflict),
// missing rows (-> ErrNotFound), soft-delete visibility, and text search.
// A backend that passes Run is a conforming store.Store implementation.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Run drives the full conformance suite against a Store built by newStore.
// newStore is called once per top-level subtest (and must return a Store
// over a clean dataset each time).
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()

	t.Run("Tenants", func(t *testing.T) { testTenants(t, newStore(t)) })
	t.Run("APIKeys", func(t *testing.T) { testAPIKeys(t, newStore(t)) })
	t.Run("APIKeyLimits", func(t *testing.T) { testAPIKeyLimits(t, newStore(t)) })
	t.Run("Credentials", func(t *testing.T) { testCredentials(t, newStore(t)) })
	t.Run("Connectors", func(t *testing.T) { testConnectors(t, newStore(t)) })
	t.Run("MCPCatalog", func(t *testing.T) { testMCPCatalog(t, newStore(t)) })
	t.Run("AgentProfiles", func(t *testing.T) { testAgentProfiles(t, newStore(t)) })
	t.Run("ToolCache", func(t *testing.T) { testToolCache(t, newStore(t)) })
	t.Run("Models", func(t *testing.T) { testModels(t, newStore(t)) })
	t.Run("ModelLimits", func(t *testing.T) { testModelLimits(t, newStore(t)) })
	t.Run("ModelCatalog", func(t *testing.T) { testModelCatalog(t, newStore(t)) })
	t.Run("Skills", func(t *testing.T) { testSkills(t, newStore(t)) })
	t.Run("ProfileSkills", func(t *testing.T) { testProfileSkills(t, newStore(t)) })
	t.Run("Users", func(t *testing.T) { testUsers(t, newStore(t)) })
	t.Run("UserSessions", func(t *testing.T) { testUserSessions(t, newStore(t)) })
	t.Run("AuthAudit", func(t *testing.T) { testAuthAudit(t, newStore(t)) })
	t.Run("PingAndMigrateAreIdempotent", func(t *testing.T) { testPingMigrate(t, newStore(t)) })
}

// unique returns a short, collision-resistant token for building unique
// slugs/names/hashes within a single subtest run.
func unique(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

func mustCreateTenant(t *testing.T, ctx context.Context, s store.Store, slugPrefix string) *store.Tenant {
	t.Helper()
	tn := &store.Tenant{
		Slug:     unique(slugPrefix),
		Name:     "Tenant " + slugPrefix,
		Settings: map[string]any{"seeded": true},
	}
	if err := s.Tenants().Create(ctx, tn); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if tn.ID == "" {
		t.Fatal("create tenant: ID not populated")
	}
	return tn
}

// ---------------------------------------------------------------------------
// Tenants
// ---------------------------------------------------------------------------

func testTenants(t *testing.T, s store.Store) {
	ctx := context.Background()

	a := mustCreateTenant(t, ctx, s, "tenant-a")
	b := mustCreateTenant(t, ctx, s, "tenant-b")

	got, err := s.Tenants().GetBySlug(ctx, a.Slug)
	if err != nil {
		t.Fatalf("GetBySlug(%q): %v", a.Slug, err)
	}
	if got.ID != a.ID || got.Name != a.Name {
		t.Errorf("GetBySlug(%q) = %+v, want ID=%q Name=%q", a.Slug, got, a.ID, a.Name)
	}
	if got.CreatedAt.IsZero() {
		t.Error("GetBySlug: CreatedAt not populated")
	}

	if _, err := s.Tenants().GetBySlug(ctx, unique("no-such-tenant")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetBySlug(missing) error = %v, want ErrNotFound", err)
	}

	list, err := s.Tenants().List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !containsTenant(list, a.ID) || !containsTenant(list, b.ID) {
		t.Errorf("List() = %d tenants, missing one of %q/%q", len(list), a.ID, b.ID)
	}

	// Duplicate slug -> ErrConflict.
	dup := &store.Tenant{Slug: a.Slug, Name: "duplicate"}
	if err := s.Tenants().Create(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(duplicate slug) error = %v, want ErrConflict", err)
	}
}

func containsTenant(list []*store.Tenant, id string) bool {
	for _, tn := range list {
		if tn.ID == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// APIKeys
// ---------------------------------------------------------------------------

func testAPIKeys(t *testing.T, s store.Store) {
	ctx := context.Background()

	tenantA := mustCreateTenant(t, ctx, s, "apikey-a")
	tenantB := mustCreateTenant(t, ctx, s, "apikey-b")

	keyA := &store.APIKey{
		TenantID:  tenantA.ID,
		Name:      "ci key",
		Role:      "admin",
		KeyHash:   unique("hash"),
		KeyPrefix: "gk_abcd1234",
		CreatedBy: "avinash@tuskira.ai",
	}
	if err := s.APIKeys().Create(ctx, keyA); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	if keyA.ID == "" {
		t.Fatal("create api key: ID not populated")
	}

	keyB := &store.APIKey{
		TenantID:  tenantB.ID,
		Name:      "other tenant key",
		Role:      "agent",
		KeyHash:   unique("hash"),
		KeyPrefix: "gk_efgh5678",
	}
	if err := s.APIKeys().Create(ctx, keyB); err != nil {
		t.Fatalf("create api key (tenant B): %v", err)
	}

	got, err := s.APIKeys().GetByHash(ctx, keyA.KeyHash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if got.ID != keyA.ID || got.TenantID != tenantA.ID {
		t.Errorf("GetByHash = %+v, want ID=%q TenantID=%q", got, keyA.ID, tenantA.ID)
	}

	if _, err := s.APIKeys().GetByHash(ctx, unique("no-such-hash")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByHash(missing) error = %v, want ErrNotFound", err)
	}

	gotByID, err := s.APIKeys().GetByID(ctx, tenantA.ID, keyA.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if gotByID.ID != keyA.ID || gotByID.KeyHash != keyA.KeyHash {
		t.Errorf("GetByID = %+v, want ID=%q KeyHash=%q", gotByID, keyA.ID, keyA.KeyHash)
	}

	// A well-formed but nonexistent id (id is a Postgres uuid column;
	// unlike KeyHash/Slug/Name it can't be a fabricated unique() string
	// without a syntax error against a real backend).
	if _, err := s.APIKeys().GetByID(ctx, tenantA.ID, uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByID(missing id) error = %v, want ErrNotFound", err)
	}
	// Tenant isolation: keyB belongs to tenantB, so fetching it scoped to
	// tenantA must miss even though the id is real.
	if _, err := s.APIKeys().GetByID(ctx, tenantA.ID, keyB.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByID(wrong tenant) error = %v, want ErrNotFound", err)
	}

	// Tenant isolation: listing tenant A's keys must never include tenant B's.
	listA, err := s.APIKeys().List(ctx, tenantA.ID)
	if err != nil {
		t.Fatalf("List(tenantA): %v", err)
	}
	if !containsAPIKey(listA, keyA.ID) {
		t.Errorf("List(tenantA) missing key %q", keyA.ID)
	}
	if containsAPIKey(listA, keyB.ID) {
		t.Error("List(tenantA) leaked tenant B's key")
	}

	touchAt := time.Now().UTC().Truncate(time.Second)
	if err := s.APIKeys().TouchLastUsed(ctx, keyA.ID, touchAt); err != nil {
		t.Fatalf("TouchLastUsed: %v", err)
	}
	listA, err = s.APIKeys().List(ctx, tenantA.ID)
	if err != nil {
		t.Fatalf("List(tenantA) after touch: %v", err)
	}
	if k := findAPIKey(listA, keyA.ID); k == nil || k.LastUsedAt == nil {
		t.Error("TouchLastUsed did not persist LastUsedAt")
	}

	if err := s.APIKeys().Revoke(ctx, tenantA.ID, keyA.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	listA, err = s.APIKeys().List(ctx, tenantA.ID)
	if err != nil {
		t.Fatalf("List(tenantA) after revoke: %v", err)
	}
	if k := findAPIKey(listA, keyA.ID); k == nil || k.RevokedAt == nil {
		t.Error("Revoke did not persist RevokedAt")
	}

	// Duplicate key hash -> ErrConflict.
	dup := &store.APIKey{TenantID: tenantA.ID, Name: "dup", Role: "admin", KeyHash: keyB.KeyHash, KeyPrefix: "gk_dupdupdu"}
	if err := s.APIKeys().Create(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(duplicate key_hash) error = %v, want ErrConflict", err)
	}
}

// testAPIKeyLimits covers APIKey.Limits: round-tripped on Create, replaced
// and cleared by SetLimits, visible through every read, tenant-scoped.
func testAPIKeyLimits(t *testing.T, s store.Store) {
	ctx := context.Background()
	tenantA := mustCreateTenant(t, ctx, s, "limits-a")
	tenantB := mustCreateTenant(t, ctx, s, "limits-b")

	daily, rpm := 1.25, 60
	k := &store.APIKey{
		TenantID: tenantA.ID, Name: "budgeted", Role: "agent",
		KeyHash: unique("hash"), KeyPrefix: "gk_lim00001",
		Limits: &store.Limits{DailyUSD: &daily, RPM: &rpm},
	}
	if err := s.APIKeys().Create(ctx, k); err != nil {
		t.Fatalf("create api key with limits: %v", err)
	}
	plain := &store.APIKey{TenantID: tenantA.ID, Name: "plain", Role: "agent", KeyHash: unique("hash"), KeyPrefix: "gk_lim00002"}
	if err := s.APIKeys().Create(ctx, plain); err != nil {
		t.Fatalf("create api key without limits: %v", err)
	}

	got, err := s.APIKeys().GetByHash(ctx, k.KeyHash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if l := got.Limits; l == nil || l.DailyUSD == nil || *l.DailyUSD != daily || l.RPM == nil || *l.RPM != rpm ||
		l.MonthlyUSD != nil || l.MaxTokens != nil {
		t.Fatalf("GetByHash limits = %+v, want daily_usd=%v rpm=%d only", got.Limits, daily, rpm)
	}
	if got, err := s.APIKeys().GetByID(ctx, tenantA.ID, plain.ID); err != nil || got.Limits != nil {
		t.Fatalf("GetByID(no limits) = %+v, %v; want nil Limits", got, err)
	}

	monthly, maxTok := 30.0, 4096
	if err := s.APIKeys().SetLimits(ctx, tenantA.ID, k.ID, &store.Limits{MonthlyUSD: &monthly, MaxTokens: &maxTok}); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}
	list, err := s.APIKeys().List(ctx, tenantA.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if l := findAPIKey(list, k.ID).Limits; l == nil || l.DailyUSD != nil || l.RPM != nil ||
		l.MonthlyUSD == nil || *l.MonthlyUSD != monthly || l.MaxTokens == nil || *l.MaxTokens != maxTok {
		t.Fatalf("List limits after SetLimits = %+v, want a full replacement (monthly_usd, max_tokens only)", l)
	}

	// A zero budget is a real limit (block all spend), not "unset".
	zero := 0.0
	if err := s.APIKeys().SetLimits(ctx, tenantA.ID, k.ID, &store.Limits{DailyUSD: &zero}); err != nil {
		t.Fatalf("SetLimits(zero): %v", err)
	}
	if got, err := s.APIKeys().GetByID(ctx, tenantA.ID, k.ID); err != nil || got.Limits == nil || got.Limits.DailyUSD == nil || *got.Limits.DailyUSD != 0 {
		t.Fatalf("GetByID after zero budget = %+v, %v; want daily_usd=0", got, err)
	}

	// Clearing: nil and an empty Limits both mean "no limits".
	if err := s.APIKeys().SetLimits(ctx, tenantA.ID, k.ID, &store.Limits{}); err != nil {
		t.Fatalf("SetLimits(empty): %v", err)
	}
	if got, err := s.APIKeys().GetByID(ctx, tenantA.ID, k.ID); err != nil || got.Limits != nil {
		t.Fatalf("GetByID after clear = %+v, %v; want nil Limits", got, err)
	}

	// Tenant isolation and missing rows -> ErrNotFound.
	if err := s.APIKeys().SetLimits(ctx, tenantB.ID, k.ID, &store.Limits{RPM: &rpm}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetLimits(wrong tenant) error = %v, want ErrNotFound", err)
	}
	if err := s.APIKeys().SetLimits(ctx, tenantA.ID, uuid.NewString(), nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetLimits(missing id) error = %v, want ErrNotFound", err)
	}
}

func containsAPIKey(list []*store.APIKey, id string) bool { return findAPIKey(list, id) != nil }

func findAPIKey(list []*store.APIKey, id string) *store.APIKey {
	for _, k := range list {
		if k.ID == id {
			return k
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

func testCredentials(t *testing.T, s store.Store) {
	ctx := context.Background()

	tenantA := mustCreateTenant(t, ctx, s, "cred-a")
	tenantB := mustCreateTenant(t, ctx, s, "cred-b")

	name := unique("okta-prod")
	cred := &store.Credential{
		TenantID:   tenantA.ID,
		Name:       name,
		Type:       "secret_store",
		Ciphertext: []byte("cipher-bytes"),
		Nonce:      []byte("nonce-bytes"),
		KeyID:      "k1",
		FieldNames: []string{"client_id", "client_secret"},
		CreatedBy:  "avinash@tuskira.ai",
	}
	if err := s.Credentials().Create(ctx, cred); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if cred.ID == "" {
		t.Fatal("create credential: ID not populated")
	}

	got, err := s.Credentials().Get(ctx, tenantA.ID, name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Ciphertext) != "cipher-bytes" || string(got.Nonce) != "nonce-bytes" {
		t.Errorf("Get did not round-trip ciphertext/nonce: %+v", got)
	}
	if len(got.FieldNames) != 2 {
		t.Errorf("Get FieldNames = %v, want 2 entries", got.FieldNames)
	}

	// Tenant isolation: tenant B must not be able to Get tenant A's credential.
	if _, err := s.Credentials().Get(ctx, tenantB.ID, name); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(tenantB, tenantA's name) error = %v, want ErrNotFound", err)
	}

	// List is metadata-only: never leaks ciphertext/nonce.
	list, err := s.Credentials().List(ctx, tenantA.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, c := range list {
		if c.ID != cred.ID {
			continue
		}
		found = true
		if len(c.Ciphertext) != 0 || len(c.Nonce) != 0 {
			t.Errorf("List leaked ciphertext/nonce: %+v", c)
		}
		if c.Type != "secret_store" || len(c.FieldNames) != 2 {
			t.Errorf("List metadata incomplete: %+v", c)
		}
	}
	if !found {
		t.Error("List(tenantA) missing the created credential")
	}

	// Duplicate (tenant, name) -> ErrConflict.
	dup := &store.Credential{TenantID: tenantA.ID, Name: name, Type: "env", Ciphertext: []byte("x"), Nonce: []byte("y"), KeyID: "k1"}
	if err := s.Credentials().Create(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(duplicate tenant+name) error = %v, want ErrConflict", err)
	}

	// Rotate updates ciphertext/nonce/key_id and sets RotatedAt.
	if err := s.Credentials().Rotate(ctx, tenantA.ID, name, []byte("new-cipher"), []byte("new-nonce"), "k2", []string{"client_id", "client_secret"}); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	got, err = s.Credentials().Get(ctx, tenantA.ID, name)
	if err != nil {
		t.Fatalf("Get after rotate: %v", err)
	}
	if string(got.Ciphertext) != "new-cipher" || got.KeyID != "k2" || got.RotatedAt == nil || len(got.FieldNames) != 2 || got.FieldNames[1] != "client_secret" {
		t.Errorf("Rotate did not persist: %+v", got)
	}

	// Delete removes it.
	if err := s.Credentials().Delete(ctx, tenantA.ID, name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Credentials().Get(ctx, tenantA.ID, name); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get after delete error = %v, want ErrNotFound", err)
	}
	if err := s.Credentials().Delete(ctx, tenantA.ID, name); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Delete(already deleted) error = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Connectors
// ---------------------------------------------------------------------------

func testConnectors(t *testing.T, s store.Store) {
	ctx := context.Background()

	tenantA := mustCreateTenant(t, ctx, s, "conn-a")
	tenantB := mustCreateTenant(t, ctx, s, "conn-b")

	slug := unique("everything")
	c := &store.Connector{
		TenantID:     tenantA.ID,
		Name:         "Everything server",
		Slug:         slug,
		Endpoint:     "https://mcp.example.com/everything",
		TimeoutMS:    30000,
		Status:       "unknown",
		Capabilities: map[string]any{"tools": true},
		Metadata:     map[string]any{"headers": map[string]any{}},
	}
	if err := s.Connectors().Create(ctx, c); err != nil {
		t.Fatalf("create connector: %v", err)
	}
	if c.ID == "" {
		t.Fatal("create connector: ID not populated")
	}

	byID, err := s.Connectors().Get(ctx, tenantA.ID, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if byID.Slug != slug {
		t.Errorf("Get.Slug = %q, want %q", byID.Slug, slug)
	}

	bySlug, err := s.Connectors().GetBySlug(ctx, tenantA.ID, slug)
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if bySlug.ID != c.ID {
		t.Errorf("GetBySlug.ID = %q, want %q", bySlug.ID, c.ID)
	}

	// Tenant isolation.
	if _, err := s.Connectors().Get(ctx, tenantB.ID, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(tenantB, tenantA's id) error = %v, want ErrNotFound", err)
	}
	if _, err := s.Connectors().GetBySlug(ctx, tenantB.ID, slug); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetBySlug(tenantB, tenantA's slug) error = %v, want ErrNotFound", err)
	}

	list, err := s.Connectors().List(ctx, tenantA.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !containsConnector(list, c.ID) {
		t.Error("List(tenantA) missing created connector")
	}

	// Update.
	c.Endpoint = "https://mcp.example.com/everything-v2"
	c.Status = "healthy"
	if err := s.Connectors().Update(ctx, c); err != nil {
		t.Fatalf("Update: %v", err)
	}
	byID, err = s.Connectors().Get(ctx, tenantA.ID, c.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if byID.Endpoint != c.Endpoint || byID.Status != "healthy" {
		t.Errorf("Update did not persist: %+v", byID)
	}
	if !byID.UpdatedAt.After(byID.CreatedAt) && !byID.UpdatedAt.Equal(byID.CreatedAt) {
		t.Errorf("UpdatedAt %v should be >= CreatedAt %v", byID.UpdatedAt, byID.CreatedAt)
	}

	// Duplicate slug -> ErrConflict.
	dup := &store.Connector{TenantID: tenantA.ID, Name: "dup", Slug: slug, Endpoint: "https://x", TimeoutMS: 1000}
	if err := s.Connectors().Create(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(duplicate slug) error = %v, want ErrConflict", err)
	}

	// Same slug in a different tenant is fine (unique is per-tenant).
	other := &store.Connector{TenantID: tenantB.ID, Name: "same slug, other tenant", Slug: slug, Endpoint: "https://y", TimeoutMS: 1000}
	if err := s.Connectors().Create(ctx, other); err != nil {
		t.Errorf("Create(same slug, different tenant) error = %v, want nil", err)
	}

	// Update can change the slug (an explicit override, not the default
	// immutable path); colliding with another connector's slug in the
	// same tenant is a conflict, same as Create.
	c2 := &store.Connector{TenantID: tenantA.ID, Name: "second", Slug: unique("second"), Endpoint: "https://second-conn", TimeoutMS: 1000}
	if err := s.Connectors().Create(ctx, c2); err != nil {
		t.Fatalf("create second connector: %v", err)
	}
	newSlug := unique("everything-v2")
	c.Slug = newSlug
	if err := s.Connectors().Update(ctx, c); err != nil {
		t.Fatalf("Update (slug change): %v", err)
	}
	byID, err = s.Connectors().Get(ctx, tenantA.ID, c.ID)
	if err != nil {
		t.Fatalf("Get after slug update: %v", err)
	}
	if byID.Slug != newSlug {
		t.Errorf("Update did not persist slug change: got %q, want %q", byID.Slug, newSlug)
	}
	if _, err := s.Connectors().GetBySlug(ctx, tenantA.ID, slug); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetBySlug(old slug) after slug update error = %v, want ErrNotFound", err)
	}

	c.Slug = c2.Slug
	if err := s.Connectors().Update(ctx, c); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Update(colliding slug) error = %v, want ErrConflict", err)
	}
	c.Slug = newSlug // restore: the conflicting Update above must not have persisted

	// SoftDelete hides it from Get/GetBySlug/List.
	if err := s.Connectors().SoftDelete(ctx, tenantA.ID, c.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, err := s.Connectors().Get(ctx, tenantA.ID, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get after soft delete error = %v, want ErrNotFound", err)
	}
	if _, err := s.Connectors().GetBySlug(ctx, tenantA.ID, c.Slug); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetBySlug after soft delete error = %v, want ErrNotFound", err)
	}
	list, err = s.Connectors().List(ctx, tenantA.ID)
	if err != nil {
		t.Fatalf("List after soft delete: %v", err)
	}
	if containsConnector(list, c.ID) {
		t.Error("List after soft delete still contains the connector")
	}

	// A deleted connector's slug is free again.
	again := &store.Connector{TenantID: tenantA.ID, Name: "Again", Slug: c.Slug, Endpoint: "https://mcp.example.com/again", TimeoutMS: 1000}
	if err := s.Connectors().Create(ctx, again); err != nil {
		t.Errorf("Create with a soft-deleted connector's slug: %v", err)
	}
}

func containsConnector(list []*store.Connector, id string) bool {
	for _, c := range list {
		if c.ID == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// AgentProfiles
// ---------------------------------------------------------------------------

func testAgentProfiles(t *testing.T, s store.Store) {
	ctx := context.Background()

	tenantA := mustCreateTenant(t, ctx, s, "prof-a")
	tenantB := mustCreateTenant(t, ctx, s, "prof-b")

	conn := &store.Connector{TenantID: tenantA.ID, Name: "conn", Slug: unique("conn"), Endpoint: "https://x", TimeoutMS: 1000}
	if err := s.Connectors().Create(ctx, conn); err != nil {
		t.Fatalf("create connector: %v", err)
	}
	conn2 := &store.Connector{TenantID: tenantA.ID, Name: "conn2", Slug: unique("conn2"), Endpoint: "https://y", TimeoutMS: 1000}
	if err := s.Connectors().Create(ctx, conn2); err != nil {
		t.Fatalf("create connector2: %v", err)
	}

	slug := unique("reader")
	p := &store.AgentProfile{
		TenantID:    tenantA.ID,
		Name:        "Reader",
		Slug:        slug,
		Description: "read-only agent",
		Metadata:    map[string]any{"owner": "platform"},
	}
	if err := s.AgentProfiles().Create(ctx, p); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if p.ID == "" {
		t.Fatal("create profile: ID not populated")
	}

	byID, err := s.AgentProfiles().Get(ctx, tenantA.ID, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if byID.Slug != slug {
		t.Errorf("Get.Slug = %q, want %q", byID.Slug, slug)
	}

	bySlug, err := s.AgentProfiles().GetBySlug(ctx, tenantA.ID, slug)
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if bySlug.ID != p.ID {
		t.Errorf("GetBySlug.ID = %q, want %q", bySlug.ID, p.ID)
	}

	// Tenant isolation.
	if _, err := s.AgentProfiles().Get(ctx, tenantB.ID, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(tenantB, tenantA's id) error = %v, want ErrNotFound", err)
	}

	list, err := s.AgentProfiles().List(ctx, tenantA.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !containsProfile(list, p.ID) {
		t.Error("List(tenantA) missing created profile")
	}

	// Update.
	p.Description = "read-only agent, updated"
	if err := s.AgentProfiles().Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}
	byID, err = s.AgentProfiles().Get(ctx, tenantA.ID, p.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if byID.Description != p.Description {
		t.Errorf("Update did not persist description: %+v", byID)
	}

	// Duplicate slug -> ErrConflict.
	dup := &store.AgentProfile{TenantID: tenantA.ID, Name: "dup", Slug: slug}
	if err := s.AgentProfiles().Create(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(duplicate slug) error = %v, want ErrConflict", err)
	}

	// SetTools / GetTools, including full-replace semantics.
	initial := []store.ProfileTool{
		{ConnectorID: conn.ID, ToolNamespace: "", ToolName: "echo"},
		{ConnectorID: conn.ID, ToolNamespace: "", ToolName: "query"},
	}
	if err := s.AgentProfiles().SetTools(ctx, tenantA.ID, p.ID, initial); err != nil {
		t.Fatalf("SetTools: %v", err)
	}
	tools, err := s.AgentProfiles().GetTools(ctx, tenantA.ID, p.ID)
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("GetTools() = %d tools, want 2: %+v", len(tools), tools)
	}

	replacement := []store.ProfileTool{
		{ConnectorID: conn2.ID, ToolNamespace: "ns", ToolName: "fetch"},
	}
	if err := s.AgentProfiles().SetTools(ctx, tenantA.ID, p.ID, replacement); err != nil {
		t.Fatalf("SetTools (replace): %v", err)
	}
	tools, err = s.AgentProfiles().GetTools(ctx, tenantA.ID, p.ID)
	if err != nil {
		t.Fatalf("GetTools after replace: %v", err)
	}
	if len(tools) != 1 || tools[0].ToolName != "fetch" || tools[0].ConnectorID != conn2.ID {
		t.Errorf("GetTools after replace = %+v, want exactly the replacement set", tools)
	}

	// SoftDelete hides it from Get/GetBySlug/List.
	if err := s.AgentProfiles().SoftDelete(ctx, tenantA.ID, p.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, err := s.AgentProfiles().Get(ctx, tenantA.ID, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get after soft delete error = %v, want ErrNotFound", err)
	}
	list, err = s.AgentProfiles().List(ctx, tenantA.ID)
	if err != nil {
		t.Fatalf("List after soft delete: %v", err)
	}
	if containsProfile(list, p.ID) {
		t.Error("List after soft delete still contains the profile")
	}

	// A deleted profile's slug is free again.
	again := &store.AgentProfile{TenantID: tenantA.ID, Name: "Again", Slug: p.Slug}
	if err := s.AgentProfiles().Create(ctx, again); err != nil {
		t.Errorf("Create with a soft-deleted profile's slug: %v", err)
	}
}

func containsProfile(list []*store.AgentProfile, id string) bool {
	for _, p := range list {
		if p.ID == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// ToolCache
// ---------------------------------------------------------------------------

func testToolCache(t *testing.T, s store.Store) {
	ctx := context.Background()

	tenantA := mustCreateTenant(t, ctx, s, "tc-a")
	tenantB := mustCreateTenant(t, ctx, s, "tc-b")

	connA := &store.Connector{TenantID: tenantA.ID, Name: "conn", Slug: unique("conn"), Endpoint: "https://x", TimeoutMS: 1000}
	if err := s.Connectors().Create(ctx, connA); err != nil {
		t.Fatalf("create connector: %v", err)
	}
	connA2 := &store.Connector{TenantID: tenantA.ID, Name: "conn2", Slug: unique("conn2"), Endpoint: "https://y", TimeoutMS: 1000}
	if err := s.Connectors().Create(ctx, connA2); err != nil {
		t.Fatalf("create connector2: %v", err)
	}
	connB := &store.Connector{TenantID: tenantB.ID, Name: "connB", Slug: unique("connb"), Endpoint: "https://z", TimeoutMS: 1000}
	if err := s.Connectors().Create(ctx, connB); err != nil {
		t.Fatalf("create connector (tenant B): %v", err)
	}

	now := time.Now().UTC()
	tools := []*store.CachedTool{
		{
			TenantID: tenantA.ID, ConnectorID: connA.ID,
			ToolNamespace: "", ToolName: "photosynthesis-lookup",
			Description: "Looks up photosynthesis rates for a given plant species",
			InputSchema: map[string]any{"type": "object"},
			CachedAt:    now, ExpiresAt: now.Add(30 * time.Minute),
		},
		{
			TenantID: tenantA.ID, ConnectorID: connA2.ID,
			ToolNamespace: "ns", ToolName: "weather",
			Description: "Current weather for a location",
			InputSchema: map[string]any{"type": "object"},
			CachedAt:    now, ExpiresAt: now.Add(-time.Minute), // already expired
		},
		{
			TenantID: tenantB.ID, ConnectorID: connB.ID,
			ToolNamespace: "", ToolName: "other-tenant-tool",
			Description: "belongs to tenant B",
			CachedAt:    now, ExpiresAt: now.Add(30 * time.Minute),
		},
	}
	if err := s.ToolCache().Upsert(ctx, tools...); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	byConn, err := s.ToolCache().ListByConnector(ctx, tenantA.ID, connA.ID)
	if err != nil {
		t.Fatalf("ListByConnector: %v", err)
	}
	if len(byConn) != 1 || byConn[0].ToolName != "photosynthesis-lookup" {
		t.Errorf("ListByConnector = %+v, want exactly the photosynthesis-lookup tool", byConn)
	}

	byTenant, err := s.ToolCache().ListByTenant(ctx, tenantA.ID)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(byTenant) != 2 {
		t.Errorf("ListByTenant(tenantA) = %d tools, want 2", len(byTenant))
	}
	for _, ct := range byTenant {
		if ct.TenantID == tenantB.ID {
			t.Error("ListByTenant(tenantA) leaked tenant B's tool")
		}
	}

	// Upsert again with the same identity (tenant, connector, namespace,
	// name) must update in place, not duplicate.
	updated := &store.CachedTool{
		TenantID: tenantA.ID, ConnectorID: connA.ID,
		ToolNamespace: "", ToolName: "photosynthesis-lookup",
		Description: "Looks up photosynthesis rates for a given plant species (v2)",
		CachedAt:    now, ExpiresAt: now.Add(30 * time.Minute),
	}
	if err := s.ToolCache().Upsert(ctx, updated); err != nil {
		t.Fatalf("Upsert (update): %v", err)
	}
	byConn, err = s.ToolCache().ListByConnector(ctx, tenantA.ID, connA.ID)
	if err != nil {
		t.Fatalf("ListByConnector after re-upsert: %v", err)
	}
	if len(byConn) != 1 || byConn[0].Description != updated.Description {
		t.Errorf("Upsert should update in place, got %+v", byConn)
	}

	// Search: a mid-word substring ("tosyn", inside "photosynthesis") has
	// no full-text stem of its own, so a hit here proves the ILIKE
	// fallback (not just plainto_tsquery) is wired up.
	hits, err := s.ToolCache().Search(ctx, tenantA.ID, "tosyn", 10, false)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !containsCachedTool(hits, connA.ID, "photosynthesis-lookup") {
		t.Errorf("Search(%q) = %+v, want a substring match on photosynthesis-lookup", "tosyn", hits)
	}

	// Search must stay tenant-scoped.
	hits, err = s.ToolCache().Search(ctx, tenantA.ID, "other-tenant-tool", 10, false)
	if err != nil {
		t.Fatalf("Search (cross-tenant): %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("Search(tenantA) leaked tenant B's tool: %+v", hits)
	}

	// MarkStale.
	if err := s.ToolCache().MarkStale(ctx, connA.ID); err != nil {
		t.Fatalf("MarkStale: %v", err)
	}
	byConn, err = s.ToolCache().ListByConnector(ctx, tenantA.ID, connA.ID)
	if err != nil {
		t.Fatalf("ListByConnector after MarkStale: %v", err)
	}
	if len(byConn) != 1 || !byConn[0].Stale {
		t.Errorf("MarkStale did not persist: %+v", byConn)
	}

	// Search excludes stale rows by default, and includes them when asked.
	hits, err = s.ToolCache().Search(ctx, tenantA.ID, "tosyn", 10, false)
	if err != nil {
		t.Fatalf("Search (includeStale=false): %v", err)
	}
	if containsCachedTool(hits, connA.ID, "photosynthesis-lookup") {
		t.Errorf("Search(includeStale=false) = %+v, want the now-stale tool excluded", hits)
	}
	hits, err = s.ToolCache().Search(ctx, tenantA.ID, "tosyn", 10, true)
	if err != nil {
		t.Fatalf("Search (includeStale=true): %v", err)
	}
	if !containsCachedTool(hits, connA.ID, "photosynthesis-lookup") {
		t.Errorf("Search(includeStale=true) = %+v, want the stale tool included", hits)
	}

	// DeleteExpired removes only the already-expired row (connA2's
	// "weather" tool), not the still-valid ones.
	deleted, err := s.ToolCache().DeleteExpired(ctx)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if deleted != 1 {
		t.Errorf("DeleteExpired() = %d, want 1", deleted)
	}
	byTenant, err = s.ToolCache().ListByTenant(ctx, tenantA.ID)
	if err != nil {
		t.Fatalf("ListByTenant after DeleteExpired: %v", err)
	}
	if containsCachedTool(byTenant, connA2.ID, "weather") {
		t.Error("DeleteExpired left the expired weather tool in place")
	}

	// DeleteByConnector removes the rest for that connector.
	if err := s.ToolCache().DeleteByConnector(ctx, connA.ID); err != nil {
		t.Fatalf("DeleteByConnector: %v", err)
	}
	byConn, err = s.ToolCache().ListByConnector(ctx, tenantA.ID, connA.ID)
	if err != nil {
		t.Fatalf("ListByConnector after DeleteByConnector: %v", err)
	}
	if len(byConn) != 0 {
		t.Errorf("ListByConnector after DeleteByConnector = %+v, want empty", byConn)
	}

	// Soft-deleting a connector must hide its cached tools from both
	// ListByTenant and ListByConnector even though the rows themselves
	// are still there -- the second layer of the delete-time cache
	// invalidation in internal/api/handlers.Connectors.Delete, for when
	// its first layer (an explicit DeleteByConnector/Invalidate call) is
	// missed or races a crash. connB (tenant B) already has one row
	// ("other-tenant-tool") untouched by everything above; add a second
	// tenant-B connector and tool, soft-delete connB, and confirm only
	// the second connector's row is left.
	connB2 := &store.Connector{TenantID: tenantB.ID, Name: "connB2", Slug: unique("connb2"), Endpoint: "https://z2", TimeoutMS: 1000}
	if err := s.Connectors().Create(ctx, connB2); err != nil {
		t.Fatalf("create connector (tenant B, second): %v", err)
	}
	if err := s.ToolCache().Upsert(ctx, &store.CachedTool{
		TenantID: tenantB.ID, ConnectorID: connB2.ID,
		ToolNamespace: "", ToolName: "still-here",
		CachedAt: now, ExpiresAt: now.Add(30 * time.Minute),
	}); err != nil {
		t.Fatalf("Upsert (tenant B, second connector): %v", err)
	}
	if err := s.Connectors().SoftDelete(ctx, tenantB.ID, connB.ID); err != nil {
		t.Fatalf("SoftDelete connB: %v", err)
	}

	byTenant, err = s.ToolCache().ListByTenant(ctx, tenantB.ID)
	if err != nil {
		t.Fatalf("ListByTenant after soft-deleting connB: %v", err)
	}
	if len(byTenant) != 1 || byTenant[0].ToolName != "still-here" {
		t.Errorf("ListByTenant(tenantB) after soft-deleting connB = %+v, want only connB2's row", byTenant)
	}

	byConn, err = s.ToolCache().ListByConnector(ctx, tenantB.ID, connB.ID)
	if err != nil {
		t.Fatalf("ListByConnector after soft-deleting connB: %v", err)
	}
	if len(byConn) != 0 {
		t.Errorf("ListByConnector(soft-deleted connector) = %+v, want empty", byConn)
	}
}

func containsCachedTool(list []*store.CachedTool, connectorID, toolName string) bool {
	for _, ct := range list {
		if ct.ConnectorID == connectorID && ct.ToolName == toolName {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

func testModels(t *testing.T, s store.Store) {
	ctx := context.Background()

	tenantA := mustCreateTenant(t, ctx, s, "model-a")
	tenantB := mustCreateTenant(t, ctx, s, "model-b")

	name := unique("sonnet")
	m := &store.Model{
		TenantID:    tenantA.ID,
		Name:        name,
		Description: "Fast default",
		Enabled:     true,
		Targets: []store.ModelTarget{
			{Vendor: "anthropic", Model: "claude-sonnet-4-5", Credential: "anthropic-prod"},
			{Vendor: "bedrock", Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0", Region: "us-east-1"},
		},
		Price:    &store.ModelPrice{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
		Metadata: map[string]any{"team": "platform"},
	}
	if err := s.Models().Create(ctx, m); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if m.ID == "" {
		t.Fatal("create model: ID not populated")
	}

	byID, err := s.Models().Get(ctx, tenantA.ID, m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if byID.Name != name || byID.TenantID != tenantA.ID || !byID.Enabled {
		t.Errorf("Get = %+v, want name %q tenant %q enabled", byID, name, tenantA.ID)
	}
	if len(byID.Targets) != 2 || byID.Targets[0].Vendor != "anthropic" || byID.Targets[0].Credential != "anthropic-prod" ||
		byID.Targets[1].Vendor != "bedrock" || byID.Targets[1].Region != "us-east-1" {
		t.Errorf("Get.Targets = %+v, want the two targets in order", byID.Targets)
	}
	if byID.Price == nil || byID.Price.Input != 3 || byID.Price.Output != 15 || byID.Price.CacheRead != 0.3 || byID.Price.CacheWrite != 3.75 {
		t.Errorf("Get.Price = %+v, want {3 15 0.3 3.75}", byID.Price)
	}
	if byID.Metadata["team"] != "platform" {
		t.Errorf("Get.Metadata = %+v", byID.Metadata)
	}

	byName, err := s.Models().GetByName(ctx, tenantA.ID, name)
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if byName.ID != m.ID {
		t.Errorf("GetByName.ID = %q, want %q", byName.ID, m.ID)
	}

	// Tenant isolation: tenant B sees neither the id nor the name.
	if _, err := s.Models().Get(ctx, tenantB.ID, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(tenantB, tenantA's id) error = %v, want ErrNotFound", err)
	}
	if _, err := s.Models().GetByName(ctx, tenantB.ID, name); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByName(tenantB, tenantA's name) error = %v, want ErrNotFound", err)
	}
	if _, err := s.Models().GetByName(ctx, tenantA.ID, unique("no-such")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByName(missing) error = %v, want ErrNotFound", err)
	}

	// Duplicate name within the tenant -> ErrConflict; the same name in
	// another tenant is fine.
	dup := &store.Model{TenantID: tenantA.ID, Name: name, Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "x"}}}
	if err := s.Models().Create(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(duplicate name) error = %v, want ErrConflict", err)
	}
	other := &store.Model{TenantID: tenantB.ID, Name: name, Enabled: true, Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "y"}}}
	if err := s.Models().Create(ctx, other); err != nil {
		t.Errorf("Create(same name, different tenant) error = %v, want nil", err)
	}

	// Platform rows (TenantID "") are visible to every tenant; a tenant row
	// of the same name overrides them for that tenant only.
	platName := unique("haiku")
	plat := &store.Model{Name: platName, Enabled: true, Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "claude-haiku-4-5"}}}
	if err := s.Models().Create(ctx, plat); err != nil {
		t.Fatalf("create platform model: %v", err)
	}
	if plat.ID == "" {
		t.Fatal("create platform model: ID not populated")
	}
	platDup := &store.Model{Name: platName, Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "z"}}}
	if err := s.Models().Create(ctx, platDup); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(duplicate platform name) error = %v, want ErrConflict", err)
	}
	for _, tid := range []string{tenantA.ID, tenantB.ID, ""} {
		got, err := s.Models().GetByName(ctx, tid, platName)
		if err != nil {
			t.Fatalf("GetByName(%q, platform name): %v", tid, err)
		}
		if got.ID != plat.ID || got.TenantID != "" {
			t.Errorf("GetByName(%q, platform name) = id %q tenant %q, want the platform row %q", tid, got.ID, got.TenantID, plat.ID)
		}
		if got, err := s.Models().Get(ctx, tid, plat.ID); err != nil || got.ID != plat.ID {
			t.Errorf("Get(%q, platform id) = %v, %v; want the platform row", tid, got, err)
		}
	}
	override := &store.Model{TenantID: tenantA.ID, Name: platName, Enabled: true, Targets: []store.ModelTarget{{Vendor: "openai_compat", Model: "gpt-4o-mini", BaseURL: "https://llm.internal/v1", AllowCallerKey: true, Label: "groq"}}}
	if err := s.Models().Create(ctx, override); err != nil {
		t.Fatalf("create tenant override of platform name: %v", err)
	}
	if got, err := s.Models().GetByName(ctx, tenantA.ID, platName); err != nil || len(got.Targets) != 1 || !got.Targets[0].AllowCallerKey || got.Targets[0].Label != "groq" {
		t.Errorf("GetByName(override) = %+v, %v; want allow_caller_key and label to round-trip", got, err)
	}
	if byID.Targets[0].AllowCallerKey || byID.Targets[1].AllowCallerKey {
		t.Errorf("allow_caller_key must default to false: %+v", byID.Targets)
	}
	if got, err := s.Models().GetByName(ctx, tenantA.ID, platName); err != nil || got.ID != override.ID {
		t.Errorf("GetByName(tenantA, overridden name) = %v, %v; want tenant row %q", got, err, override.ID)
	}
	if got, err := s.Models().GetByName(ctx, tenantB.ID, platName); err != nil || got.ID != plat.ID {
		t.Errorf("GetByName(tenantB, overridden name) = %v, %v; want platform row %q", got, err, plat.ID)
	}

	// List: tenant rows + platform rows, tenant wins on a shared name,
	// ordered by name; "" lists platform rows only.
	list, err := s.Models().List(ctx, tenantA.ID, store.ListOptions{})
	if err != nil {
		t.Fatalf("List(tenantA): %v", err)
	}
	if !containsModel(list, m.ID) || !containsModel(list, override.ID) || containsModel(list, plat.ID) || containsModel(list, other.ID) {
		t.Errorf("List(tenantA) = %v; want own rows, the override (not the platform row it shadows), nothing of tenantB's", modelIDs(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Name > list[i].Name {
			t.Errorf("List not ordered by name: %v", modelIDs(list))
			break
		}
	}
	listB, err := s.Models().List(ctx, tenantB.ID, store.ListOptions{})
	if err != nil {
		t.Fatalf("List(tenantB): %v", err)
	}
	if !containsModel(listB, other.ID) || !containsModel(listB, plat.ID) || containsModel(listB, m.ID) {
		t.Errorf("List(tenantB) = %v; want tenantB's row + the platform row", modelIDs(listB))
	}
	platList, err := s.Models().List(ctx, "", store.ListOptions{})
	if err != nil {
		t.Fatalf("List(platform): %v", err)
	}
	if !containsModel(platList, plat.ID) || containsModel(platList, m.ID) || containsModel(platList, override.ID) {
		t.Errorf("List(platform) = %v; want platform rows only", modelIDs(platList))
	}

	// EnabledOnly drops disabled rows.
	m.Enabled = false
	if err := s.Models().Update(ctx, m); err != nil {
		t.Fatalf("Update (disable): %v", err)
	}
	enabled, err := s.Models().List(ctx, tenantA.ID, store.ListOptions{EnabledOnly: true})
	if err != nil {
		t.Fatalf("List(EnabledOnly): %v", err)
	}
	if containsModel(enabled, m.ID) || !containsModel(enabled, override.ID) {
		t.Errorf("List(EnabledOnly) = %v; disabled row must be dropped", modelIDs(enabled))
	}

	// Update persists every mutable column (name, targets, price cleared).
	m.Enabled = true
	m.Description = "renamed"
	m.Targets = m.Targets[:1]
	m.Price = nil
	m.Metadata = map[string]any{"team": "sec"}
	newName := unique("sonnet-v2")
	m.Name = newName
	if err := s.Models().Update(ctx, m); err != nil {
		t.Fatalf("Update: %v", err)
	}
	byID, err = s.Models().Get(ctx, tenantA.ID, m.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if byID.Name != newName || byID.Description != "renamed" || len(byID.Targets) != 1 || byID.Price != nil || byID.Metadata["team"] != "sec" || !byID.Enabled {
		t.Errorf("Update did not persist: %+v (price %+v)", byID, byID.Price)
	}
	if !byID.UpdatedAt.After(byID.CreatedAt) && !byID.UpdatedAt.Equal(byID.CreatedAt) {
		t.Errorf("UpdatedAt %v should be >= CreatedAt %v", byID.UpdatedAt, byID.CreatedAt)
	}
	if _, err := s.Models().GetByName(ctx, tenantA.ID, name); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByName(old name) after rename error = %v, want ErrNotFound", err)
	}

	// A rename onto another live row's name is a conflict.
	m.Name = override.Name
	if err := s.Models().Update(ctx, m); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Update(colliding name) error = %v, want ErrConflict", err)
	}
	m.Name = newName

	// Update is scoped: tenant B cannot rewrite tenant A's row, and a
	// tenant cannot rewrite a platform row.
	stolen := *m
	stolen.TenantID = tenantB.ID
	if err := s.Models().Update(ctx, &stolen); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Update(tenantB, tenantA's row) error = %v, want ErrNotFound", err)
	}
	platAsTenant := *plat
	platAsTenant.TenantID = tenantA.ID
	if err := s.Models().Update(ctx, &platAsTenant); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Update(tenantA, platform row) error = %v, want ErrNotFound", err)
	}
	// ... while the platform owner ("") can.
	plat.Description = "platform default"
	if err := s.Models().Update(ctx, plat); err != nil {
		t.Errorf("Update(platform row) error = %v", err)
	}

	// SoftDelete hides the row from Get/GetByName/List and frees the name.
	if err := s.Models().SoftDelete(ctx, tenantB.ID, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SoftDelete(tenantB, tenantA's row) error = %v, want ErrNotFound", err)
	}
	if err := s.Models().SoftDelete(ctx, tenantA.ID, plat.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SoftDelete(tenantA, platform row) error = %v, want ErrNotFound", err)
	}
	if err := s.Models().SoftDelete(ctx, tenantA.ID, m.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, err := s.Models().Get(ctx, tenantA.ID, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get after soft delete error = %v, want ErrNotFound", err)
	}
	if _, err := s.Models().GetByName(ctx, tenantA.ID, newName); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByName after soft delete error = %v, want ErrNotFound", err)
	}
	list, err = s.Models().List(ctx, tenantA.ID, store.ListOptions{})
	if err != nil {
		t.Fatalf("List after soft delete: %v", err)
	}
	if containsModel(list, m.ID) {
		t.Error("List still returns the soft-deleted row")
	}
	if err := s.Models().SoftDelete(ctx, tenantA.ID, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second SoftDelete error = %v, want ErrNotFound", err)
	}
	if err := s.Models().Update(ctx, m); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Update(soft-deleted) error = %v, want ErrNotFound", err)
	}
	reuse := &store.Model{TenantID: tenantA.ID, Name: newName, Enabled: true, Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "again"}}}
	if err := s.Models().Create(ctx, reuse); err != nil {
		t.Errorf("Create(name of a soft-deleted row) error = %v, want nil", err)
	}

	// Platform rows can be soft-deleted by the platform owner.
	if err := s.Models().SoftDelete(ctx, "", plat.ID); err != nil {
		t.Errorf("SoftDelete(platform row) error = %v", err)
	}
	if _, err := s.Models().GetByName(ctx, tenantB.ID, platName); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByName(tenantB, deleted platform name) error = %v, want ErrNotFound", err)
	}
}

// testModelLimits covers Model.Limits: round-tripped on Create, replaced
// and cleared by Update, visible through every read, on tenant and
// platform rows alike.
func testModelLimits(t *testing.T, s store.Store) {
	ctx := context.Background()
	tenant := mustCreateTenant(t, ctx, s, "model-lim")
	targets := []store.ModelTarget{{Vendor: "anthropic", Model: "claude-sonnet-4-5"}}

	daily, rpm := 2.5, 30
	name := unique("budgeted")
	m := &store.Model{TenantID: tenant.ID, Name: name, Enabled: true, Targets: targets, Limits: &store.Limits{DailyUSD: &daily, RPM: &rpm}}
	if err := s.Models().Create(ctx, m); err != nil {
		t.Fatalf("create model with limits: %v", err)
	}
	plain := &store.Model{TenantID: tenant.ID, Name: unique("plain"), Enabled: true, Targets: targets}
	if err := s.Models().Create(ctx, plain); err != nil {
		t.Fatalf("create model without limits: %v", err)
	}

	got, err := s.Models().GetByName(ctx, tenant.ID, name)
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if l := got.Limits; l == nil || l.DailyUSD == nil || *l.DailyUSD != daily || l.RPM == nil || *l.RPM != rpm ||
		l.MonthlyUSD != nil || l.MaxTokens != nil {
		t.Fatalf("GetByName limits = %+v, want daily_usd=%v rpm=%d only", got.Limits, daily, rpm)
	}
	if got, err := s.Models().Get(ctx, tenant.ID, plain.ID); err != nil || got.Limits != nil {
		t.Fatalf("Get(no limits) = %+v, %v; want nil Limits", got, err)
	}

	// Update replaces the limits wholesale.
	monthly, maxTok := 40.0, 2048
	got.Limits = &store.Limits{MonthlyUSD: &monthly, MaxTokens: &maxTok}
	if err := s.Models().Update(ctx, got); err != nil {
		t.Fatalf("Update(limits): %v", err)
	}
	list, err := s.Models().List(ctx, tenant.ID, store.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var listed *store.Model
	for _, x := range list {
		if x.ID == m.ID {
			listed = x
		}
	}
	if listed == nil {
		t.Fatalf("List lacks the model: %v", modelIDs(list))
	}
	if l := listed.Limits; l == nil || l.DailyUSD != nil || l.RPM != nil ||
		l.MonthlyUSD == nil || *l.MonthlyUSD != monthly || l.MaxTokens == nil || *l.MaxTokens != maxTok {
		t.Fatalf("List limits after Update = %+v, want a full replacement (monthly_usd, max_tokens only)", l)
	}

	// Zero is a real limit, not "unset".
	zero := 0.0
	listed.Limits = &store.Limits{DailyUSD: &zero}
	if err := s.Models().Update(ctx, listed); err != nil {
		t.Fatalf("Update(zero): %v", err)
	}
	if got, err := s.Models().Get(ctx, tenant.ID, m.ID); err != nil || got.Limits == nil || got.Limits.DailyUSD == nil || *got.Limits.DailyUSD != 0 {
		t.Fatalf("Get after zero = %+v, %v; want daily_usd 0", got, err)
	}

	// Clearing.
	got, _ = s.Models().Get(ctx, tenant.ID, m.ID)
	got.Limits = nil
	if err := s.Models().Update(ctx, got); err != nil {
		t.Fatalf("Update(clear): %v", err)
	}
	if got, err := s.Models().Get(ctx, tenant.ID, m.ID); err != nil || got.Limits != nil {
		t.Fatalf("Get after clear = %+v, %v; want nil Limits", got, err)
	}

	// A platform row carries limits too, and every tenant reads them.
	plat := &store.Model{Name: unique("plat-budgeted"), Enabled: true, Targets: targets, Limits: &store.Limits{RPM: &rpm}}
	if err := s.Models().Create(ctx, plat); err != nil {
		t.Fatalf("create platform model with limits: %v", err)
	}
	if got, err := s.Models().GetByName(ctx, tenant.ID, plat.Name); err != nil || got.Limits == nil || got.Limits.RPM == nil || *got.Limits.RPM != rpm {
		t.Fatalf("GetByName(platform) = %+v, %v; want rpm=%d", got, err, rpm)
	}
}

func containsModel(list []*store.Model, id string) bool {
	for _, m := range list {
		if m.ID == id {
			return true
		}
	}
	return false
}

// modelIDs renders a list as "id(name)" tokens for failure messages.
func modelIDs(list []*store.Model) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID+"("+m.Name+")")
	}
	return out
}

// ---------------------------------------------------------------------------
// Model catalog
// ---------------------------------------------------------------------------

func testModelCatalog(t *testing.T, s store.Store) {
	ctx := context.Background()
	cat := s.ModelCatalog()

	slug := unique("acme")
	p := &store.ModelCatalogProvider{
		Slug: slug, DisplayName: "Acme AI", Vendor: "openai_compat",
		BaseURL: "https://api.acme.example/v1", DocsURL: "https://docs.acme.example",
		Enabled: true,
	}
	if err := cat.CreateProvider(ctx, p); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if p.ID == "" {
		t.Fatal("CreateProvider: ID not populated")
	}

	gotP, err := cat.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if gotP.Slug != slug || gotP.BaseURL != p.BaseURL || len(gotP.Models) != 0 {
		t.Errorf("GetProvider = %+v, want a fresh provider with no models", gotP)
	}

	// Duplicate slug -> ErrConflict.
	dupP := &store.ModelCatalogProvider{Slug: slug, DisplayName: "dup", BaseURL: "https://x.example/v1", Enabled: true}
	if err := cat.CreateProvider(ctx, dupP); !errors.Is(err, store.ErrConflict) {
		t.Errorf("CreateProvider(duplicate slug) error = %v, want ErrConflict", err)
	}

	m1Name := unique("acme-large")
	m1 := &store.ModelCatalogModel{
		ProviderID: p.ID, ModelID: "acme/large-v1", DisplayName: "Acme Large", SuggestedName: m1Name,
		Price:        &store.ModelPrice{Input: 1, Output: 2},
		Capabilities: map[string]any{"tools": true, "vision": nil},
		Notes:        "multi-turn tool use is not supported yet",
		Enabled:      true,
	}
	if err := cat.CreateModel(ctx, m1); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if m1.ID == "" {
		t.Fatal("CreateModel: ID not populated")
	}
	m2Name := unique("acme-small")
	m2 := &store.ModelCatalogModel{ProviderID: p.ID, ModelID: "acme/small-v1", SuggestedName: m2Name, Enabled: true}
	if err := cat.CreateModel(ctx, m2); err != nil {
		t.Fatalf("CreateModel(m2): %v", err)
	}

	// Duplicate suggested_name (even under a different provider) and
	// duplicate (provider_id, model_id) both conflict.
	if err := cat.CreateModel(ctx, &store.ModelCatalogModel{ProviderID: p.ID, ModelID: "acme/other", SuggestedName: m1Name}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("CreateModel(duplicate suggested_name) error = %v, want ErrConflict", err)
	}
	if err := cat.CreateModel(ctx, &store.ModelCatalogModel{ProviderID: p.ID, ModelID: "acme/large-v1", SuggestedName: unique("other")}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("CreateModel(duplicate provider+model_id) error = %v, want ErrConflict", err)
	}

	gotM1, err := cat.GetModel(ctx, m1.ID)
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if gotM1.Price == nil || gotM1.Price.Input != 1 || gotM1.Capabilities["tools"] != true || gotM1.Notes != "multi-turn tool use is not supported yet" {
		t.Errorf("GetModel = %+v, want price {1 2 ...}, capabilities.tools=true and notes round-tripped", gotM1)
	}

	gotP2, err := cat.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProvider (with models): %v", err)
	}
	if len(gotP2.Models) != 2 {
		t.Fatalf("GetProvider.Models = %d, want 2", len(gotP2.Models))
	}

	list, err := cat.ListProviders(ctx)
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if !containsProviderWithModels(list, p.ID, 2) {
		t.Error("ListProviders missing the created provider with its 2 models")
	}

	// UpdateModel: rewrite fields; provider_id is immutable.
	gotM1.DisplayName = "Acme Large v1.1"
	gotM1.Notes = "now supports tools"
	gotM1.Enabled = false
	if err := cat.UpdateModel(ctx, gotM1); err != nil {
		t.Fatalf("UpdateModel: %v", err)
	}
	reGotM1, err := cat.GetModel(ctx, m1.ID)
	if err != nil {
		t.Fatalf("GetModel after update: %v", err)
	}
	if reGotM1.DisplayName != "Acme Large v1.1" || reGotM1.Enabled || reGotM1.ProviderID != p.ID || reGotM1.Notes != "now supports tools" {
		t.Errorf("UpdateModel did not persist: %+v", reGotM1)
	}

	// CountTenantUsage / Connect.
	tenantA := mustCreateTenant(t, ctx, s, "catalog-a")
	tenantB := mustCreateTenant(t, ctx, s, "catalog-b")

	if n, err := cat.CountTenantUsage(ctx, m2.ID); err != nil || n != 0 {
		t.Errorf("CountTenantUsage(unused) = %d, %v, want 0, nil", n, err)
	}

	connectModel := func(cm *store.ModelCatalogModel) store.CatalogConnectModel {
		return store.CatalogConnectModel{
			CatalogModelID: cm.ID, Name: cm.SuggestedName, Description: cm.DisplayName,
			ModelID: cm.ModelID, BaseURL: p.BaseURL, Label: p.Slug, Price: cm.Price, Capabilities: cm.Capabilities,
		}
	}

	newCredName := unique("acme-api-key")
	res, err := cat.Connect(ctx, tenantA.ID,
		store.CatalogConnectCredential{New: &store.CatalogConnectNewCredential{
			Name: newCredName, Type: "api_key", Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyID: "k1", FieldNames: []string{"api_key"},
		}},
		[]store.CatalogConnectModel{connectModel(m2)},
	)
	if err != nil {
		t.Fatalf("Connect (new credential): %v", err)
	}
	if !res.CredentialCreated || res.CredentialName != newCredName {
		t.Errorf("Connect result.Credential = %+v, want created=true name=%q", res, newCredName)
	}
	if len(res.Models) != 1 || res.Models[0].Status != "created" || res.Models[0].Name != m2Name {
		t.Errorf("Connect result.Models = %+v, want one created row named %q", res.Models, m2Name)
	}
	if _, err := s.Credentials().Get(ctx, tenantA.ID, newCredName); err != nil {
		t.Errorf("Connect did not create the credential: %v", err)
	}
	tm, err := s.Models().Get(ctx, tenantA.ID, res.Models[0].ModelID)
	if err != nil {
		t.Fatalf("Get the connected tenant model: %v", err)
	}
	if tm.CatalogModelID != m2.ID || len(tm.Targets) != 1 || tm.Targets[0].Vendor != "openai_compat" ||
		tm.Targets[0].Model != m2.ModelID || tm.Targets[0].BaseURL != p.BaseURL || tm.Targets[0].Credential != newCredName || tm.Targets[0].Label != p.Slug {
		t.Errorf("connected tenant model = %+v, want a single openai_compat target from the catalog model", tm)
	}

	// Re-requesting the exact same catalog model is a no-op "exists", not
	// a conflict -- and reuses an EXISTING credential this time.
	res2, err := cat.Connect(ctx, tenantA.ID, store.CatalogConnectCredential{Existing: newCredName}, []store.CatalogConnectModel{connectModel(m2)})
	if err != nil {
		t.Fatalf("Connect (re-request, existing credential): %v", err)
	}
	if res2.CredentialCreated || res2.CredentialName != newCredName {
		t.Errorf("Connect (reuse) result.Credential = %+v, want created=false name=%q", res2, newCredName)
	}
	if len(res2.Models) != 1 || res2.Models[0].Status != "exists" || res2.Models[0].ModelID != tm.ID {
		t.Errorf("Connect (re-request) result.Models = %+v, want one \"exists\" row for %q", res2.Models, tm.ID)
	}

	// Existing credential that does not exist -> ErrNotFound, and no model
	// is registered.
	if _, err := cat.Connect(ctx, tenantA.ID, store.CatalogConnectCredential{Existing: unique("no-such-cred")}, []store.CatalogConnectModel{connectModel(m1)}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Connect(missing existing credential) error = %v, want ErrNotFound", err)
	}
	if _, err := s.Models().GetByName(ctx, tenantA.ID, m1Name); !errors.Is(err, store.ErrNotFound) {
		t.Error("Connect(missing existing credential) should not have registered any model")
	}

	// A NEW credential whose name collides with an existing one ->
	// CredentialNameConflictError (wraps ErrConflict), and no model row
	// from this call either.
	_, err = cat.Connect(ctx, tenantA.ID,
		store.CatalogConnectCredential{New: &store.CatalogConnectNewCredential{Name: newCredName, Type: "api_key", Ciphertext: []byte("c"), Nonce: []byte("n"), KeyID: "k1"}},
		[]store.CatalogConnectModel{connectModel(m1)})
	var credConflict *store.CredentialNameConflictError
	if !errors.As(err, &credConflict) || !errors.Is(err, store.ErrConflict) {
		t.Errorf("Connect(new credential name clash) error = %v, want *CredentialNameConflictError wrapping ErrConflict", err)
	}
	if _, err := s.Models().GetByName(ctx, tenantA.ID, m1Name); !errors.Is(err, store.ErrNotFound) {
		t.Error("Connect(credential name clash) should not have registered any model")
	}

	// A hand-created tenant model with the SAME name as a catalog
	// connect's target, but not connected from that catalog model ->
	// ModelNameConflictError, tenant-scoped (tenant B is unaffected).
	handMade := &store.Model{TenantID: tenantA.ID, Name: m1Name, Enabled: true, Targets: []store.ModelTarget{{Vendor: "anthropic", Model: "x"}}}
	if err := s.Models().Create(ctx, handMade); err != nil {
		t.Fatalf("create hand-made model: %v", err)
	}
	_, err = cat.Connect(ctx, tenantA.ID, store.CatalogConnectCredential{Existing: newCredName}, []store.CatalogConnectModel{connectModel(m1)})
	var modelConflict *store.ModelNameConflictError
	if !errors.As(err, &modelConflict) || !errors.Is(err, store.ErrConflict) || modelConflict.Name != m1Name {
		t.Errorf("Connect(name clash with hand-made model) error = %v, want *ModelNameConflictError{Name: %q}", err, m1Name)
	}
	credForB := unique("acme-api-key-b")
	resB, err := cat.Connect(ctx, tenantB.ID,
		store.CatalogConnectCredential{New: &store.CatalogConnectNewCredential{Name: credForB, Type: "api_key", Ciphertext: []byte("c"), Nonce: []byte("n"), KeyID: "k1"}},
		[]store.CatalogConnectModel{connectModel(m1)})
	if err != nil || len(resB.Models) != 1 || resB.Models[0].Status != "created" {
		t.Errorf("Connect(tenant B, same name as tenant A's hand-made model) = %+v, %v, want a created row (tenant-scoped conflict check)", resB, err)
	}

	if n, err := cat.CountTenantUsage(ctx, m2.ID); err != nil || n != 1 {
		t.Errorf("CountTenantUsage(m2, connected once) = %d, %v, want 1, nil", n, err)
	}

	// ApplyPrices: a platform-wide price refresh. tm (tenant A, connected
	// from m2 whose catalog price was nil) gets a manual override first, to
	// prove it is left alone; resB's model (tenant B, connected from m1
	// with m1's price {1,2} untouched since Connect) still matches m1's
	// OLD catalog price, so it DOES get the refresh.
	tm.Price = &store.ModelPrice{Input: 99, Output: 99}
	if err := s.Models().Update(ctx, tm); err != nil {
		t.Fatalf("manually override tenant A's connected model price: %v", err)
	}
	newM2Price := &store.ModelPrice{Input: 5, Output: 10}
	newM1Price := &store.ModelPrice{Input: 3, Output: 4}
	applyRes, err := cat.ApplyPrices(ctx, []store.PriceUpdate{
		{CatalogModelID: m2.ID, Price: newM2Price},
		{CatalogModelID: m1.ID, Price: newM1Price},
	}, true)
	if err != nil {
		t.Fatalf("ApplyPrices: %v", err)
	}
	if applyRes.CatalogUpdated != 2 {
		t.Errorf("ApplyPrices.CatalogUpdated = %d, want 2", applyRes.CatalogUpdated)
	}
	if applyRes.TenantUpdated != 1 {
		t.Errorf("ApplyPrices.TenantUpdated = %d, want 1 (only resB's model still matched m1's old price)", applyRes.TenantUpdated)
	}
	if len(applyRes.TenantIDs) != 1 || applyRes.TenantIDs[0] != tenantB.ID {
		t.Errorf("ApplyPrices.TenantIDs = %v, want [%q] (only tenant B's model was touched)", applyRes.TenantIDs, tenantB.ID)
	}
	if gotM2, err := cat.GetModel(ctx, m2.ID); err != nil || gotM2.Price == nil || *gotM2.Price != *newM2Price {
		t.Errorf("GetModel(m2) after ApplyPrices = %+v, %v, want price %+v", gotM2, err, newM2Price)
	}
	if gotM1, err := cat.GetModel(ctx, m1.ID); err != nil || gotM1.Price == nil || *gotM1.Price != *newM1Price {
		t.Errorf("GetModel(m1) after ApplyPrices = %+v, %v, want price %+v", gotM1, err, newM1Price)
	}
	if tmAfterApply, err := s.Models().Get(ctx, tenantA.ID, tm.ID); err != nil || tmAfterApply.Price == nil || tmAfterApply.Price.Input != 99 {
		t.Errorf("tenant A's overridden model after ApplyPrices = %+v, %v, want the override (99,99) preserved", tmAfterApply, err)
	}
	if tmBAfterApply, err := s.Models().Get(ctx, tenantB.ID, resB.Models[0].ModelID); err != nil || tmBAfterApply.Price == nil || *tmBAfterApply.Price != *newM1Price {
		t.Errorf("tenant B's connected model after ApplyPrices = %+v, %v, want refreshed price %+v", tmBAfterApply, err, newM1Price)
	}

	// A batch with one unknown catalog model id aborts the WHOLE call --
	// m2's price from the successful call above is untouched.
	_, err = cat.ApplyPrices(ctx, []store.PriceUpdate{
		{CatalogModelID: m2.ID, Price: &store.ModelPrice{Input: 50, Output: 60}},
		{CatalogModelID: unique("no-such-catalog-model"), Price: &store.ModelPrice{Input: 1, Output: 1}},
	}, false)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ApplyPrices(one unknown id) error = %v, want ErrNotFound", err)
	}
	if gotM2, err := cat.GetModel(ctx, m2.ID); err != nil || gotM2.Price == nil || *gotM2.Price != *newM2Price {
		t.Errorf("GetModel(m2) after aborted ApplyPrices = %+v, %v, want unchanged price %+v (no partial write)", gotM2, err, newM2Price)
	}

	// DeleteModel: usage>0 does not block the store-level delete itself
	// (the caller enforces the 409-unless-force confirmation); afterward
	// the connected tenant model loses its CatalogModelID (ON DELETE SET
	// NULL) but is otherwise untouched.
	if err := cat.DeleteModel(ctx, m2.ID); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if _, err := cat.GetModel(ctx, m2.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetModel after delete error = %v, want ErrNotFound", err)
	}
	tmAfter, err := s.Models().Get(ctx, tenantA.ID, tm.ID)
	if err != nil {
		t.Fatalf("Get tenant model after catalog model delete: %v", err)
	}
	if tmAfter.CatalogModelID != "" {
		t.Errorf("tenant model CatalogModelID after catalog model delete = %q, want \"\" (ON DELETE SET NULL)", tmAfter.CatalogModelID)
	}
	if err := cat.DeleteModel(ctx, m2.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteModel(already deleted) error = %v, want ErrNotFound", err)
	}

	// DeleteProvider cascades to its remaining models.
	if err := cat.DeleteProvider(ctx, p.ID); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	if _, err := cat.GetProvider(ctx, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetProvider after delete error = %v, want ErrNotFound", err)
	}
	if _, err := cat.GetModel(ctx, m1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetModel(m1) after provider delete error = %v, want ErrNotFound (ON DELETE CASCADE)", err)
	}
	if err := cat.DeleteProvider(ctx, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteProvider(already deleted) error = %v, want ErrNotFound", err)
	}
}

func containsProviderWithModels(list []*store.ModelCatalogProvider, id string, nModels int) bool {
	for _, p := range list {
		if p.ID == id {
			return len(p.Models) == nModels
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

// skillMD renders a minimal, valid SKILL.md: the two required frontmatter
// fields (name, description) and a one-line body.
func skillMD(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\nBody for " + name + ".\n"
}

func testSkills(t *testing.T, s store.Store) {
	ctx := context.Background()

	tenantA := mustCreateTenant(t, ctx, s, "skill-a")
	tenantB := mustCreateTenant(t, ctx, s, "skill-b")

	name := unique("recon")
	files := []store.SkillFile{
		{Path: "SKILL.md", Content: skillMD(name, "Reconnaissance helper."), SHA256: "ignored-on-write", Size: 999},
		{Path: "reference.md", Content: "extra background material"},
	}
	sk := &store.Skill{
		TenantID:    tenantA.ID,
		Name:        name,
		Kind:        "skill",
		Description: "Reconnaissance helper.",
		Frontmatter: map[string]any{"name": name, "description": "Reconnaissance helper."},
		Enabled:     true,
		Metadata:    map[string]any{"team": "platform"},
	}
	if err := s.Skills().Create(ctx, sk, files, "tester"); err != nil {
		t.Fatalf("create skill: %v", err)
	}
	if sk.ID == "" {
		t.Fatal("create skill: ID not populated")
	}
	if sk.LatestVersion != 1 {
		t.Errorf("create skill: LatestVersion = %d, want 1", sk.LatestVersion)
	}

	byID, err := s.Skills().Get(ctx, tenantA.ID, sk.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if byID.Name != name || byID.Kind != "skill" || !byID.Enabled || byID.LatestVersion != 1 {
		t.Errorf("Get = %+v, want name %q kind skill enabled latest_version 1", byID, name)
	}
	if byID.Metadata["team"] != "platform" {
		t.Errorf("Get.Metadata = %+v", byID.Metadata)
	}

	byName, err := s.Skills().GetByName(ctx, tenantA.ID, name)
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if byName.ID != sk.ID {
		t.Errorf("GetByName.ID = %q, want %q", byName.ID, sk.ID)
	}

	// Tenant isolation.
	if _, err := s.Skills().Get(ctx, tenantB.ID, sk.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(tenantB, tenantA's id) error = %v, want ErrNotFound", err)
	}
	if _, err := s.Skills().GetByName(ctx, tenantB.ID, name); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByName(tenantB, tenantA's name) error = %v, want ErrNotFound", err)
	}

	// Duplicate live name within the tenant -> ErrConflict; same name in
	// another tenant is fine.
	dup := &store.Skill{TenantID: tenantA.ID, Name: name, Kind: "skill", Enabled: true}
	if err := s.Skills().Create(ctx, dup, files, "tester"); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(duplicate name) error = %v, want ErrConflict", err)
	}
	other := &store.Skill{TenantID: tenantB.ID, Name: name, Kind: "skill", Enabled: true}
	if err := s.Skills().Create(ctx, other, files, "tester"); err != nil {
		t.Errorf("Create(same name, different tenant) error = %v, want nil", err)
	}

	// Platform rows (TenantID "") are visible to every tenant; a tenant
	// row of the same name overrides them for that tenant only.
	platName := unique("triage")
	plat := &store.Skill{Name: platName, Kind: "command", Enabled: true, Arguments: []store.CommandArgument{{Name: "target", Required: true}}}
	platFiles := []store.SkillFile{{Path: "SKILL.md", Content: skillMD(platName, "Platform triage command.")}}
	if err := s.Skills().Create(ctx, plat, platFiles, "seed"); err != nil {
		t.Fatalf("create platform skill: %v", err)
	}
	platDup := &store.Skill{Name: platName, Kind: "command"}
	if err := s.Skills().Create(ctx, platDup, platFiles, "seed"); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(duplicate platform name) error = %v, want ErrConflict", err)
	}
	for _, tid := range []string{tenantA.ID, tenantB.ID, ""} {
		got, err := s.Skills().GetByName(ctx, tid, platName)
		if err != nil {
			t.Fatalf("GetByName(%q, platform name): %v", tid, err)
		}
		if got.ID != plat.ID || got.TenantID != "" {
			t.Errorf("GetByName(%q, platform name) = id %q tenant %q, want the platform row %q", tid, got.ID, got.TenantID, plat.ID)
		}
	}
	override := &store.Skill{TenantID: tenantA.ID, Name: platName, Kind: "command", Enabled: true}
	if err := s.Skills().Create(ctx, override, platFiles, "tester"); err != nil {
		t.Fatalf("create tenant override of platform name: %v", err)
	}
	if got, err := s.Skills().GetByName(ctx, tenantA.ID, platName); err != nil || got.ID != override.ID {
		t.Errorf("GetByName(tenantA, overridden name) = %v, %v; want tenant row %q", got, err, override.ID)
	}
	if got, err := s.Skills().GetByName(ctx, tenantB.ID, platName); err != nil || got.ID != plat.ID {
		t.Errorf("GetByName(tenantB, overridden name) = %v, %v; want platform row %q", got, err, plat.ID)
	}

	// List: tenant rows + platform rows, tenant wins on a shared name.
	list, total, err := s.Skills().List(ctx, tenantA.ID, store.SkillListOptions{})
	if err != nil {
		t.Fatalf("List(tenantA): %v", err)
	}
	if total != len(list) {
		t.Errorf("List(tenantA) total = %d, want len(items) = %d", total, len(list))
	}
	if !containsSkill(list, sk.ID) || !containsSkill(list, override.ID) || containsSkill(list, plat.ID) || containsSkill(list, other.ID) {
		t.Errorf("List(tenantA) = %v; want own rows, the override (not the platform row it shadows), nothing of tenantB's", skillIDs(list))
	}
	platOnly, _, err := s.Skills().List(ctx, "", store.SkillListOptions{})
	if err != nil {
		t.Fatalf("List(platform): %v", err)
	}
	if !containsSkill(platOnly, plat.ID) || containsSkill(platOnly, sk.ID) {
		t.Errorf("List(platform) = %v; want platform rows only", skillIDs(platOnly))
	}

	// opts.Kind filters.
	commandsOnly, _, err := s.Skills().List(ctx, tenantA.ID, store.SkillListOptions{Kind: "command"})
	if err != nil {
		t.Fatalf("List(kind=command): %v", err)
	}
	if containsSkill(commandsOnly, sk.ID) || !containsSkill(commandsOnly, override.ID) {
		t.Errorf("List(kind=command) = %v; want the command row only", skillIDs(commandsOnly))
	}
	skillsOnly, _, err := s.Skills().List(ctx, tenantA.ID, store.SkillListOptions{Kind: "skill"})
	if err != nil {
		t.Fatalf("List(kind=skill): %v", err)
	}
	if !containsSkill(skillsOnly, sk.ID) || containsSkill(skillsOnly, override.ID) {
		t.Errorf("List(kind=skill) = %v; want the skill row only", skillIDs(skillsOnly))
	}

	// Update rewrites description/enabled/metadata/arguments only.
	byID.Description = "renamed description"
	byID.Enabled = false
	byID.Metadata = map[string]any{"team": "sec"}
	byID.Arguments = []store.CommandArgument{{Name: "extra"}}
	if err := s.Skills().Update(ctx, byID); err != nil {
		t.Fatalf("Update: %v", err)
	}
	updated, err := s.Skills().Get(ctx, tenantA.ID, sk.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if updated.Description != "renamed description" || updated.Enabled || updated.Metadata["team"] != "sec" ||
		len(updated.Arguments) != 1 || updated.Arguments[0].Name != "extra" {
		t.Errorf("Update did not persist: %+v", updated)
	}
	if updated.Name != name || updated.Kind != "skill" {
		t.Errorf("Update must not change name/kind: %+v", updated)
	}

	// Update is scoped: tenant B cannot rewrite tenant A's row, and a
	// tenant cannot rewrite a platform row.
	stolen := *updated
	stolen.TenantID = tenantB.ID
	if err := s.Skills().Update(ctx, &stolen); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Update(tenantB, tenantA's row) error = %v, want ErrNotFound", err)
	}
	platAsTenant := *plat
	platAsTenant.TenantID = tenantA.ID
	if err := s.Skills().Update(ctx, &platAsTenant); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Update(tenantA, platform row) error = %v, want ErrNotFound", err)
	}

	// AddVersion increments LatestVersion and refreshes description/
	// frontmatter from the new SKILL.md.
	v2Files := []store.SkillFile{{Path: "SKILL.md", Content: skillMD(name, "Updated recon description.")}}
	v2, err := s.Skills().AddVersion(ctx, tenantA.ID, sk.ID, v2Files, "tester2")
	if err != nil {
		t.Fatalf("AddVersion: %v", err)
	}
	if v2.Version != 2 {
		t.Errorf("AddVersion() Version = %d, want 2", v2.Version)
	}
	if len(v2.Files) != 1 || v2.Files[0].Path != "SKILL.md" {
		t.Errorf("AddVersion() Files = %+v, want the one SKILL.md file", v2.Files)
	}
	if v2.CreatedBy != "tester2" {
		t.Errorf("AddVersion() CreatedBy = %q, want tester2", v2.CreatedBy)
	}
	afterVersion, err := s.Skills().Get(ctx, tenantA.ID, sk.ID)
	if err != nil {
		t.Fatalf("Get after AddVersion: %v", err)
	}
	if afterVersion.LatestVersion != 2 {
		t.Errorf("Get after AddVersion: LatestVersion = %d, want 2", afterVersion.LatestVersion)
	}
	if afterVersion.Description != "Updated recon description." {
		t.Errorf("AddVersion did not refresh Description: %q", afterVersion.Description)
	}
	if afterVersion.Frontmatter["description"] != "Updated recon description." {
		t.Errorf("AddVersion did not refresh Frontmatter: %+v", afterVersion.Frontmatter)
	}

	// AddVersion is scoped like Update.
	if _, err := s.Skills().AddVersion(ctx, tenantB.ID, sk.ID, v2Files, "tester"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("AddVersion(tenantB, tenantA's row) error = %v, want ErrNotFound", err)
	}

	// GetVersion returns the full file bodies for a specific version;
	// ListVersions returns every version, newest first, without bodies.
	v1, err := s.Skills().GetVersion(ctx, sk.ID, 1)
	if err != nil {
		t.Fatalf("GetVersion(1): %v", err)
	}
	if len(v1.Files) != 2 {
		t.Errorf("GetVersion(1).Files = %+v, want the original 2 files", v1.Files)
	}
	if _, err := s.Skills().GetVersion(ctx, sk.ID, 99); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetVersion(missing) error = %v, want ErrNotFound", err)
	}
	versions, err := s.Skills().ListVersions(ctx, sk.ID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 || versions[0].Version != 2 || versions[1].Version != 1 {
		t.Fatalf("ListVersions = %+v, want [2, 1]", versions)
	}
	if versions[0].Files != nil {
		t.Errorf("ListVersions()[0].Files = %+v, want nil (no file bodies)", versions[0].Files)
	}

	// SoftDelete hides the row from Get/GetByName/List and frees the
	// name for re-use.
	if err := s.Skills().SoftDelete(ctx, tenantB.ID, sk.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SoftDelete(tenantB, tenantA's row) error = %v, want ErrNotFound", err)
	}
	if err := s.Skills().SoftDelete(ctx, tenantA.ID, sk.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, err := s.Skills().Get(ctx, tenantA.ID, sk.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get after soft delete error = %v, want ErrNotFound", err)
	}
	list, _, err = s.Skills().List(ctx, tenantA.ID, store.SkillListOptions{})
	if err != nil {
		t.Fatalf("List after soft delete: %v", err)
	}
	if containsSkill(list, sk.ID) {
		t.Error("List after soft delete still contains the skill")
	}
	reuse := &store.Skill{TenantID: tenantA.ID, Name: name, Kind: "skill", Enabled: true}
	if err := s.Skills().Create(ctx, reuse, files, "tester"); err != nil {
		t.Errorf("Create(name of a soft-deleted row) error = %v, want nil", err)
	}
}

func containsSkill(list []store.Skill, id string) bool {
	for _, sk := range list {
		if sk.ID == id {
			return true
		}
	}
	return false
}

func skillIDs(list []store.Skill) []string {
	out := make([]string, 0, len(list))
	for _, sk := range list {
		out = append(out, sk.ID+"("+sk.Name+")")
	}
	return out
}

// testProfileSkills covers AgentProfileStore.SetSkills/GetSkills (replace
// semantics) and AgentProfile.Instructions' round trip.
func testProfileSkills(t *testing.T, s store.Store) {
	ctx := context.Background()
	tenant := mustCreateTenant(t, ctx, s, "profskill")

	p := &store.AgentProfile{TenantID: tenant.ID, Name: "Agent", Slug: unique("agent"), Instructions: "Be careful."}
	if err := s.AgentProfiles().Create(ctx, p); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if p.Instructions != "Be careful." {
		t.Errorf("create profile: Instructions = %q, want it echoed back", p.Instructions)
	}
	got, err := s.AgentProfiles().Get(ctx, tenant.ID, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Instructions != "Be careful." {
		t.Errorf("Get.Instructions = %q, want \"Be careful.\"", got.Instructions)
	}

	got.Instructions = "Updated instructions."
	if err := s.AgentProfiles().Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err = s.AgentProfiles().Get(ctx, tenant.ID, p.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.Instructions != "Updated instructions." {
		t.Errorf("Get after update: Instructions = %q, want \"Updated instructions.\"", got.Instructions)
	}

	skA := &store.Skill{TenantID: tenant.ID, Name: unique("skill-a"), Kind: "skill", Enabled: true}
	if err := s.Skills().Create(ctx, skA, []store.SkillFile{{Path: "SKILL.md", Content: skillMD(skA.Name, "A.")}}, "tester"); err != nil {
		t.Fatalf("create skill A: %v", err)
	}
	skB := &store.Skill{TenantID: tenant.ID, Name: unique("skill-b"), Kind: "command", Enabled: true}
	if err := s.Skills().Create(ctx, skB, []store.SkillFile{{Path: "SKILL.md", Content: skillMD(skB.Name, "B.")}}, "tester"); err != nil {
		t.Fatalf("create skill B: %v", err)
	}

	empty, err := s.AgentProfiles().GetSkills(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetSkills (none attached): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("GetSkills (none attached) = %+v, want empty", empty)
	}

	pinned := 1
	initial := []store.ProfileSkill{
		{AgentProfileID: p.ID, SkillID: skA.ID, Version: &pinned},
		{AgentProfileID: p.ID, SkillID: skB.ID},
	}
	if err := s.AgentProfiles().SetSkills(ctx, p.ID, initial); err != nil {
		t.Fatalf("SetSkills: %v", err)
	}
	attached, err := s.AgentProfiles().GetSkills(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetSkills: %v", err)
	}
	if len(attached) != 2 {
		t.Fatalf("GetSkills() = %d items, want 2: %+v", len(attached), attached)
	}
	for _, item := range attached {
		switch item.SkillID {
		case skA.ID:
			if item.Version == nil || *item.Version != 1 {
				t.Errorf("GetSkills() skill A version = %v, want pinned 1", item.Version)
			}
		case skB.ID:
			if item.Version != nil {
				t.Errorf("GetSkills() skill B version = %v, want nil (latest)", item.Version)
			}
		default:
			t.Errorf("GetSkills() unexpected skill_id %q", item.SkillID)
		}
	}

	// Replace semantics: a second SetSkills call fully replaces the set.
	replacement := []store.ProfileSkill{{AgentProfileID: p.ID, SkillID: skB.ID}}
	if err := s.AgentProfiles().SetSkills(ctx, p.ID, replacement); err != nil {
		t.Fatalf("SetSkills (replace): %v", err)
	}
	attached, err = s.AgentProfiles().GetSkills(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetSkills after replace: %v", err)
	}
	if len(attached) != 1 || attached[0].SkillID != skB.ID {
		t.Errorf("GetSkills after replace = %+v, want exactly skill B", attached)
	}

	if err := s.AgentProfiles().SetSkills(ctx, uuid.NewString(), initial); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetSkills(missing profile) error = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Users, sessions, auth audit
// ---------------------------------------------------------------------------

func newUser(tenantID, username, role string) *store.User {
	return &store.User{
		TenantID:     tenantID,
		Username:     username,
		DisplayName:  "User " + username,
		PasswordHash: "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		Role:         role,
	}
}

func testUsers(t *testing.T, s store.Store) {
	ctx := context.Background()
	users := s.Users()

	tenantA := mustCreateTenant(t, ctx, s, "user-a")
	tenantB := mustCreateTenant(t, ctx, s, "user-b")

	alice := newUser(tenantA.ID, unique("alice"), store.UserRoleAdmin)
	alice.MustChangePassword = true
	if err := users.Create(ctx, alice); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if alice.ID == "" || alice.CreatedAt.IsZero() || alice.UpdatedAt.IsZero() || alice.PasswordChangedAt.IsZero() {
		t.Fatalf("Create did not populate server fields: %+v", alice)
	}

	got, err := users.Get(ctx, tenantA.ID, alice.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Username != alice.Username || got.Role != store.UserRoleAdmin || !got.MustChangePassword ||
		got.Disabled || got.PasswordHash != alice.PasswordHash || got.DisplayName != alice.DisplayName {
		t.Errorf("Get = %+v, want the created user", got)
	}
	if got.LastLoginAt != nil || got.LockedUntil != nil || got.DeletedAt != nil || got.FailedLogins != 0 {
		t.Errorf("Get: fresh user has login state set: %+v", got)
	}

	byName, err := users.GetByUsername(ctx, tenantA.ID, alice.Username)
	if err != nil || byName.ID != alice.ID {
		t.Fatalf("GetByUsername = %v, %v; want alice", byName, err)
	}

	// Tenant isolation: same id / username under another tenant is invisible.
	if _, err := users.Get(ctx, tenantB.ID, alice.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(other tenant) error = %v, want ErrNotFound", err)
	}
	if _, err := users.GetByUsername(ctx, tenantB.ID, alice.Username); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByUsername(other tenant) error = %v, want ErrNotFound", err)
	}
	if list, err := users.List(ctx, tenantB.ID, store.UserListOptions{}); err != nil || len(list) != 0 {
		t.Errorf("List(other tenant) = %d users, err %v; want none", len(list), err)
	}
	if err := users.Update(ctx, &store.User{TenantID: tenantB.ID, ID: alice.ID, Role: store.UserRoleViewer}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Update(other tenant) error = %v, want ErrNotFound", err)
	}
	if err := users.SoftDelete(ctx, tenantB.ID, alice.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SoftDelete(other tenant) error = %v, want ErrNotFound", err)
	}
	if err := users.SetPassword(ctx, tenantB.ID, alice.ID, "x", false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetPassword(other tenant) error = %v, want ErrNotFound", err)
	}

	// The same username is fine in another tenant, a conflict in the same one.
	if err := users.Create(ctx, newUser(tenantB.ID, alice.Username, store.UserRoleViewer)); err != nil {
		t.Errorf("Create(same username, other tenant): %v", err)
	}
	if err := users.Create(ctx, newUser(tenantA.ID, alice.Username, store.UserRoleViewer)); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(duplicate live username) error = %v, want ErrConflict", err)
	}

	// List: ordered by username, role filter.
	bob := newUser(tenantA.ID, alice.Username+"-b", store.UserRoleViewer)
	if err := users.Create(ctx, bob); err != nil {
		t.Fatalf("Create bob: %v", err)
	}
	list, err := users.List(ctx, tenantA.ID, store.UserListOptions{})
	if err != nil || len(list) != 2 || list[0].ID != alice.ID || list[1].ID != bob.ID {
		t.Fatalf("List = %v, %v; want [alice bob] in username order", list, err)
	}
	if list, err := users.List(ctx, tenantA.ID, store.UserListOptions{Role: store.UserRoleViewer}); err != nil || len(list) != 1 || list[0].ID != bob.ID {
		t.Errorf("List(role=viewer) = %v, %v; want [bob]", list, err)
	}

	// Update touches display name / role / disabled only.
	bob.DisplayName, bob.Role, bob.Disabled = "Bobby", store.UserRoleAdmin, true
	if err := users.Update(ctx, bob); err != nil {
		t.Fatalf("Update: %v", err)
	}
	gotBob, _ := users.Get(ctx, tenantA.ID, bob.ID)
	if gotBob.DisplayName != "Bobby" || gotBob.Role != store.UserRoleAdmin || !gotBob.Disabled || gotBob.Username != bob.Username {
		t.Errorf("after Update: %+v", gotBob)
	}
	if err := users.Update(ctx, &store.User{TenantID: tenantA.ID, ID: uuid.NewString(), Role: store.UserRoleViewer}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Update(missing) error = %v, want ErrNotFound", err)
	}

	// CountActiveAdmins: alice (enabled admin) counts; bob (disabled admin) does not.
	if n, err := users.CountActiveAdmins(ctx, tenantA.ID); err != nil || n != 1 {
		t.Errorf("CountActiveAdmins = %d, %v; want 1", n, err)
	}
	bob.Disabled = false
	if err := users.Update(ctx, bob); err != nil {
		t.Fatalf("Update enable: %v", err)
	}
	if n, _ := users.CountActiveAdmins(ctx, tenantA.ID); n != 2 {
		t.Errorf("CountActiveAdmins after enabling bob = %d, want 2", n)
	}
	if n, _ := users.CountActiveAdmins(ctx, tenantB.ID); n != 0 {
		t.Errorf("CountActiveAdmins(tenant B, viewer only) = %d, want 0", n)
	}

	// Login bookkeeping: failures lock at the threshold and reset the counter.
	at := time.Now().UTC().Truncate(time.Microsecond)
	for i := 1; i <= 2; i++ {
		if err := users.RecordLoginFailure(ctx, tenantA.ID, alice.ID, 3, 15*time.Minute, at); err != nil {
			t.Fatalf("RecordLoginFailure: %v", err)
		}
		g, _ := users.Get(ctx, tenantA.ID, alice.ID)
		if g.FailedLogins != i || g.LockedUntil != nil {
			t.Fatalf("after %d failures: failed_logins=%d locked_until=%v", i, g.FailedLogins, g.LockedUntil)
		}
	}
	if err := users.RecordLoginFailure(ctx, tenantA.ID, alice.ID, 3, 15*time.Minute, at); err != nil {
		t.Fatalf("RecordLoginFailure (locking): %v", err)
	}
	g, _ := users.Get(ctx, tenantA.ID, alice.ID)
	if g.LockedUntil == nil || !g.LockedUntil.Equal(at.Add(15*time.Minute)) || g.FailedLogins != 0 {
		t.Fatalf("after threshold: failed_logins=%d locked_until=%v, want 0 and %v", g.FailedLogins, g.LockedUntil, at.Add(15*time.Minute))
	}
	if err := users.RecordLoginSuccess(ctx, tenantA.ID, alice.ID, at); err != nil {
		t.Fatalf("RecordLoginSuccess: %v", err)
	}
	g, _ = users.Get(ctx, tenantA.ID, alice.ID)
	if g.LockedUntil != nil || g.FailedLogins != 0 || g.LastLoginAt == nil || !g.LastLoginAt.Equal(at) {
		t.Errorf("after success: %+v", g)
	}
	if err := users.RecordLoginFailure(ctx, tenantA.ID, uuid.NewString(), 3, time.Minute, at); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RecordLoginFailure(missing) error = %v, want ErrNotFound", err)
	}

	// SetPassword replaces the hash, sets must-change, and clears the lock.
	if err := users.RecordLoginFailure(ctx, tenantA.ID, alice.ID, 1, time.Hour, at); err != nil {
		t.Fatal(err)
	}
	if err := users.SetPassword(ctx, tenantA.ID, alice.ID, "new-hash", false); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	g, _ = users.Get(ctx, tenantA.ID, alice.ID)
	if g.PasswordHash != "new-hash" || g.MustChangePassword || g.LockedUntil != nil || g.FailedLogins != 0 {
		t.Errorf("after SetPassword: %+v", g)
	}
	if err := users.SetPassword(ctx, tenantA.ID, alice.ID, "temp-hash", true); err != nil {
		t.Fatal(err)
	}
	if g, _ = users.Get(ctx, tenantA.ID, alice.ID); !g.MustChangePassword {
		t.Error("SetPassword(mustChange=true) did not set MustChangePassword")
	}

	// Soft delete hides the user everywhere and frees the username.
	if err := users.SoftDelete(ctx, tenantA.ID, alice.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, err := users.Get(ctx, tenantA.ID, alice.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(deleted) error = %v, want ErrNotFound", err)
	}
	if _, err := users.GetByUsername(ctx, tenantA.ID, alice.Username); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByUsername(deleted) error = %v, want ErrNotFound", err)
	}
	if err := users.SoftDelete(ctx, tenantA.ID, alice.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SoftDelete(twice) error = %v, want ErrNotFound", err)
	}
	if n, _ := users.CountActiveAdmins(ctx, tenantA.ID); n != 1 {
		t.Errorf("CountActiveAdmins after deleting alice = %d, want 1 (bob)", n)
	}
	reborn := newUser(tenantA.ID, alice.Username, store.UserRoleViewer)
	if err := users.Create(ctx, reborn); err != nil {
		t.Fatalf("Create(re-create after soft delete): %v", err)
	}
	if reborn.ID == alice.ID {
		t.Error("re-created user reused the soft-deleted user's id")
	}
	if g, err := users.GetByUsername(ctx, tenantA.ID, alice.Username); err != nil || g.ID != reborn.ID {
		t.Errorf("GetByUsername after re-create = %v, %v; want the new user", g, err)
	}
}

func testUserSessions(t *testing.T, s store.Store) {
	ctx := context.Background()
	sessions := s.UserSessions()

	tenant := mustCreateTenant(t, ctx, s, "sess")
	u1 := newUser(tenant.ID, unique("sess-u1"), store.UserRoleAdmin)
	u2 := newUser(tenant.ID, unique("sess-u2"), store.UserRoleViewer)
	for _, u := range []*store.User{u1, u2} {
		if err := s.Users().Create(ctx, u); err != nil {
			t.Fatalf("create user: %v", err)
		}
	}

	expires := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)
	mk := func(u *store.User) *store.UserSession {
		ss := &store.UserSession{
			ID: unique("sha"), UserID: u.ID, TenantID: tenant.ID, CSRFToken: unique("csrf"),
			ExpiresAt: expires, UserAgent: "ua", IP: "203.0.113.9",
		}
		if err := sessions.Create(ctx, ss); err != nil {
			t.Fatalf("Create session: %v", err)
		}
		return ss
	}
	s1, s2, other := mk(u1), mk(u1), mk(u2)
	if s1.CreatedAt.IsZero() || s1.LastSeenAt.IsZero() {
		t.Errorf("Create did not populate timestamps: %+v", s1)
	}

	got, err := sessions.Get(ctx, s1.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UserID != u1.ID || got.TenantID != tenant.ID || got.CSRFToken != s1.CSRFToken || !got.ExpiresAt.Equal(expires) ||
		got.RevokedAt != nil || got.UserAgent != "ua" || got.IP != "203.0.113.9" {
		t.Errorf("Get = %+v, want the created session", got)
	}
	if _, err := sessions.Get(ctx, unique("nope")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
	if err := sessions.Create(ctx, &store.UserSession{ID: s1.ID, UserID: u1.ID, TenantID: tenant.ID, CSRFToken: "x", ExpiresAt: expires}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(duplicate id) error = %v, want ErrConflict", err)
	}

	seen := time.Now().Add(time.Minute).UTC().Truncate(time.Microsecond)
	if err := sessions.Touch(ctx, s1.ID, seen); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if got, _ = sessions.Get(ctx, s1.ID); !got.LastSeenAt.Equal(seen) {
		t.Errorf("after Touch LastSeenAt = %v, want %v", got.LastSeenAt, seen)
	}
	if err := sessions.Touch(ctx, unique("nope"), seen); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Touch(missing) error = %v, want ErrNotFound", err)
	}

	// Revoke is idempotent and keeps the row readable (with RevokedAt set).
	if err := sessions.Revoke(ctx, s1.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	first, _ := sessions.Get(ctx, s1.ID)
	if first.RevokedAt == nil {
		t.Fatal("Revoke did not set RevokedAt")
	}
	if err := sessions.Revoke(ctx, s1.ID); err != nil {
		t.Errorf("Revoke(twice): %v", err)
	}
	if again, _ := sessions.Get(ctx, s1.ID); !again.RevokedAt.Equal(*first.RevokedAt) {
		t.Errorf("second Revoke moved RevokedAt from %v to %v", first.RevokedAt, again.RevokedAt)
	}
	if err := sessions.Revoke(ctx, unique("nope")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Revoke(missing) error = %v, want ErrNotFound", err)
	}

	// RevokeAllForUser: only that user's still-live sessions, minus the exception.
	s3 := mk(u1)
	n, err := sessions.RevokeAllForUser(ctx, u1.ID, s3.ID)
	if err != nil {
		t.Fatalf("RevokeAllForUser: %v", err)
	}
	if n != 1 { // s2 only: s1 was already revoked, s3 is excepted
		t.Errorf("RevokeAllForUser revoked %d sessions, want 1", n)
	}
	for id, wantRevoked := range map[string]bool{s1.ID: true, s2.ID: true, s3.ID: false, other.ID: false} {
		g, _ := sessions.Get(ctx, id)
		if (g.RevokedAt != nil) != wantRevoked {
			t.Errorf("session %s revoked = %v, want %v", id, g.RevokedAt != nil, wantRevoked)
		}
	}
	if n, _ := sessions.RevokeAllForUser(ctx, u1.ID, ""); n != 1 {
		t.Errorf("RevokeAllForUser(no exception) revoked %d, want 1 (s3)", n)
	}
	if g, _ := sessions.Get(ctx, other.ID); g.RevokedAt != nil {
		t.Error("RevokeAllForUser touched another user's session")
	}

	// Expiry is data, not a read filter: a session past ExpiresAt is still returned.
	old := &store.UserSession{ID: unique("sha"), UserID: u2.ID, TenantID: tenant.ID, CSRFToken: "c", ExpiresAt: time.Now().Add(-time.Hour)}
	if err := sessions.Create(ctx, old); err != nil {
		t.Fatalf("Create(expired): %v", err)
	}
	if g, err := sessions.Get(ctx, old.ID); err != nil || !g.ExpiresAt.Before(time.Now()) {
		t.Errorf("Get(expired) = %+v, %v; want the row with a past ExpiresAt", g, err)
	}
}

func testAuthAudit(t *testing.T, s store.Store) {
	ctx := context.Background()
	audit := s.AuthAudit()

	tenantA := mustCreateTenant(t, ctx, s, "audit-a")
	tenantB := mustCreateTenant(t, ctx, s, "audit-b")
	target := newUser(tenantA.ID, unique("audit-target"), store.UserRoleViewer)
	if err := s.Users().Create(ctx, target); err != nil {
		t.Fatalf("create user: %v", err)
	}

	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	add := func(tenantID, action, targetID string, offset time.Duration) {
		t.Helper()
		e := &store.AuthAuditEntry{
			TenantID: tenantID, ActorKind: "user", ActorID: "actor-1", Action: action,
			TargetUserID: targetID, IP: "198.51.100.7", Detail: "d-" + action, At: base.Add(offset),
		}
		if err := audit.Append(ctx, e); err != nil {
			t.Fatalf("Append(%s): %v", action, err)
		}
		if e.ID == 0 {
			t.Fatalf("Append(%s): ID not populated", action)
		}
	}
	add(tenantA.ID, "login_ok", "", 1*time.Minute)
	add(tenantA.ID, "user_create", target.ID, 2*time.Minute)
	add(tenantA.ID, "login_fail", "", 3*time.Minute)
	add(tenantB.ID, "login_ok", "", 4*time.Minute)
	add("", "login_fail", "", 5*time.Minute) // unknown tenant: no tenant id

	got, total, err := audit.List(ctx, tenantA.ID, store.AuthAuditListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 3 || len(got) != 3 {
		t.Fatalf("List(tenant A) = %d entries, total %d; want 3/3 (tenant B and tenant-less rows must not leak)", len(got), total)
	}
	if got[0].Action != "login_fail" || got[1].Action != "user_create" || got[2].Action != "login_ok" {
		t.Errorf("List order = %s,%s,%s; want newest first login_fail,user_create,login_ok", got[0].Action, got[1].Action, got[2].Action)
	}
	if got[1].TargetUserID != target.ID || got[0].TargetUserID != "" || got[1].ActorKind != "user" || got[1].IP != "198.51.100.7" || got[1].Detail != "d-user_create" {
		t.Errorf("List row fields wrong: %+v / %+v", got[1], got[0])
	}

	page, total, err := audit.List(ctx, tenantA.ID, store.AuthAuditListOptions{Limit: 1, Offset: 1})
	if err != nil || total != 3 || len(page) != 1 || page[0].Action != "user_create" {
		t.Errorf("List(limit 1, offset 1) = %v, total %d, err %v; want [user_create]/3", page, total, err)
	}
	if filtered, total, _ := audit.List(ctx, tenantA.ID, store.AuthAuditListOptions{Action: "login_ok"}); total != 1 || len(filtered) != 1 {
		t.Errorf("List(action=login_ok) = %d/%d, want 1/1", len(filtered), total)
	}
	if filtered, total, _ := audit.List(ctx, tenantA.ID, store.AuthAuditListOptions{TargetUserID: target.ID}); total != 1 || len(filtered) != 1 || filtered[0].Action != "user_create" {
		t.Errorf("List(target user) = %v/%d, want [user_create]", filtered, total)
	}
	if none, total, err := audit.List(ctx, tenantA.ID, store.AuthAuditListOptions{Offset: 99}); err != nil || total != 3 || len(none) != 0 {
		t.Errorf("List(offset past end) = %d entries, total %d, err %v; want 0/3", len(none), total, err)
	}
}

func testPingMigrate(t *testing.T, s store.Store) {
	ctx := context.Background()

	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	// Migrate must have already been applied by newStore; calling it
	// again must be a safe no-op.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (idempotent re-run): %v", err)
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping after re-Migrate: %v", err)
	}
}
