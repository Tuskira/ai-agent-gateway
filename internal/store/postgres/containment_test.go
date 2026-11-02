package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// TestContainment: a key moves along the containment ladder (only one rung
// set at a time, the note kept, a revoked key refused).
func TestContainment(t *testing.T) {
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping Postgres containment test")
	}
	ctx := context.Background()
	st := newIsolatedStore(t, rawURL)
	tn := &store.Tenant{Slug: "t-" + uuid.NewString()[:8], Name: "t"}
	if err := st.Tenants().Create(ctx, tn); err != nil {
		t.Fatal(err)
	}
	k := &store.APIKey{TenantID: tn.ID, Name: "k", Role: "agent", KeyHash: "hash-" + uuid.NewString(), KeyPrefix: "gk_x"}
	if err := st.APIKeys().Create(ctx, k); err != nil {
		t.Fatal(err)
	}
	c, ok := st.APIKeys().(store.APIKeyContainer)
	if !ok {
		t.Fatal("postgres api key store cannot contain keys")
	}
	get := func() *store.APIKey {
		t.Helper()
		got, err := st.APIKeys().GetByID(ctx, tn.ID, k.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := get(); store.ContainmentOf(got) != store.ContainmentNormal || got.ContainmentNote != "" {
		t.Fatalf("new key = %+v", got)
	}
	if err := c.SetContainment(ctx, tn.ID, k.ID, store.ContainmentRestricted, "injection"); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.RestrictedAt == nil || got.SuspendedAt != nil || got.ContainmentNote != "injection" {
		t.Fatalf("restricted = %+v", got)
	}
	if err := c.SetContainment(ctx, tn.ID, k.ID, store.ContainmentKilled, "exfil"); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.SuspendedAt == nil || got.RestrictedAt != nil || store.ContainmentOf(got) != store.ContainmentKilled {
		t.Fatalf("killed = %+v", got)
	}
	byHash, err := st.APIKeys().GetByHash(ctx, k.KeyHash)
	if err != nil || byHash.SuspendedAt == nil {
		t.Fatalf("GetByHash = %+v, %v", byHash, err)
	}
	if err := c.SetContainment(ctx, tn.ID, k.ID, store.ContainmentNormal, "cleared"); err != nil {
		t.Fatal(err)
	}
	if got := get(); store.ContainmentOf(got) != store.ContainmentNormal || got.ContainmentNote != "cleared" {
		t.Fatalf("normal = %+v", got)
	}
	if err := c.SetContainment(ctx, tn.ID, uuid.NewString(), store.ContainmentKilled, "x"); err != store.ErrNotFound {
		t.Fatalf("unknown key: %v", err)
	}
	if err := st.APIKeys().Revoke(ctx, tn.ID, k.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.SetContainment(ctx, tn.ID, k.ID, store.ContainmentKilled, "x"); err != store.ErrNotFound {
		t.Fatalf("revoked key: %v", err)
	}
}
