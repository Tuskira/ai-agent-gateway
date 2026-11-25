package postgres_test

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// TestInvalidationNotifier checks, against a real Postgres
// (GATEWAY_TEST_DATABASE_URL), that every write to a row a gateway process
// caches publishes a store.Invalidation a listener on a SEPARATE connection
// pool (another replica) receives, and that touching last_used_at does not.
// The channel is database-wide, so events are filtered by this test's own
// tenant.
func TestInvalidationNotifier(t *testing.T) {
	rawURL := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set; skipping Postgres invalidation test")
	}
	ctx := context.Background()
	writer := newIsolatedStore(t, rawURL)

	otherDB, err := sql.Open("pgx", rawURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { otherDB.Close() })
	listener := postgres.New(otherDB)

	tn := &store.Tenant{Slug: "t-" + uuid.NewString()[:8], Name: "t"}
	if err := writer.Tenants().Create(ctx, tn); err != nil {
		t.Fatal(err)
	}
	other := &store.Tenant{Slug: "o-" + uuid.NewString()[:8], Name: "o"}
	if err := writer.Tenants().Create(ctx, other); err != nil {
		t.Fatal(err)
	}

	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ready := make(chan struct{}, 4)
	var mu sync.Mutex
	var got, platform []store.Invalidation
	done := make(chan error, 1)
	go func() {
		done <- listener.ListenInvalidations(lctx, func() { ready <- struct{}{} }, func(ev store.Invalidation) {
			mu.Lock()
			defer mu.Unlock()
			switch ev.Tenant {
			case tn.ID:
				got = append(got, ev)
			case "":
				// A platform row: any writer on the database may publish
				// these, so they are checked loosely below.
				platform = append(platform, ev)
			}
		})
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("listener never became ready")
	}

	one := func(ev store.Invalidation) func() []store.Invalidation {
		return func() []store.Invalidation { return []store.Invalidation{ev} }
	}
	// expect runs write and waits for exactly the wanted events (order free).
	expect := func(what string, write func(), wantFn func() []store.Invalidation) {
		t.Helper()
		mu.Lock()
		got = nil
		mu.Unlock()
		write()
		want := wantFn()
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			have := append([]store.Invalidation(nil), got...)
			mu.Unlock()
			if len(have) >= len(want) {
				// Settle: no extra event may follow.
				time.Sleep(150 * time.Millisecond)
				mu.Lock()
				have = append([]store.Invalidation(nil), got...)
				mu.Unlock()
				if len(have) != len(want) {
					t.Fatalf("%s: events = %+v, want %+v", what, have, want)
				}
				for _, w := range want {
					found := false
					for _, h := range have {
						found = found || h == w
					}
					if !found {
						t.Fatalf("%s: events = %+v, want %+v", what, have, want)
					}
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: events = %+v, want %+v", what, have, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	expectNone := func(what string, write func()) {
		t.Helper()
		mu.Lock()
		got = nil
		mu.Unlock()
		write()
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		if len(got) != 0 {
			t.Fatalf("%s: events = %+v, want none", what, got)
		}
	}

	// Profile and its grants.
	prof := &store.AgentProfile{TenantID: tn.ID, Name: "p", Slug: "p-" + uuid.NewString()[:8]}
	expect("profile create", func() {
		if err := writer.AgentProfiles().Create(ctx, prof); err != nil {
			t.Fatal(err)
		}
	}, func() []store.Invalidation {
		return []store.Invalidation{{Kind: "profile", Tenant: tn.ID, ID: prof.ID}}
	})

	conn := &store.Connector{TenantID: tn.ID, Name: "c", Slug: "c-" + uuid.NewString()[:8], Endpoint: "https://x.example.com", TimeoutMS: 1000}
	if err := writer.Connectors().Create(ctx, conn); err != nil {
		t.Fatal(err)
	}
	profEv := func() []store.Invalidation {
		return []store.Invalidation{{Kind: "profile", Tenant: tn.ID, ID: prof.ID}}
	}
	expect("profile tools (several rows, one event)", func() {
		if err := writer.AgentProfiles().SetTools(ctx, tn.ID, prof.ID, []store.ProfileTool{
			{AgentProfileID: prof.ID, ConnectorID: conn.ID, ToolName: "a"},
			{AgentProfileID: prof.ID, ConnectorID: conn.ID, ToolName: "b"},
		}); err != nil {
			t.Fatal(err)
		}
	}, profEv)
	prof.Description = "changed"
	expect("profile update", func() {
		if err := writer.AgentProfiles().Update(ctx, prof); err != nil {
			t.Fatal(err)
		}
	}, profEv)

	// Skills: tenant-wide for the tenant, empty id; a platform skill has no
	// tenant.
	sk := &store.Skill{TenantID: tn.ID, Name: "s-" + uuid.NewString()[:8], Kind: "skill", Enabled: true}
	expect("skill create", func() {
		if err := writer.Skills().Create(ctx, sk, []store.SkillFile{{Path: "SKILL.md", Content: "x", Size: 1}}, "t"); err != nil {
			t.Fatal(err)
		}
	}, one(store.Invalidation{Kind: "profile", Tenant: tn.ID, ID: ""}))
	expect("profile skills", func() {
		if err := writer.AgentProfiles().SetSkills(ctx, prof.ID, []store.ProfileSkill{{AgentProfileID: prof.ID, SkillID: sk.ID}}); err != nil {
			t.Fatal(err)
		}
	}, profEv)
	platformSkill := &store.Skill{Name: "plat-" + uuid.NewString()[:8], Kind: "skill", Enabled: true}
	mu.Lock()
	platform = nil
	mu.Unlock()
	if err := writer.Skills().Create(ctx, platformSkill, []store.SkillFile{{Path: "SKILL.md", Content: "x", Size: 1}}, "t"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		n := len(platform)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("platform skill create: no tenant-less profile event")
		}
	}

	// Credentials and sources.
	cred := &store.Credential{TenantID: tn.ID, Name: "c-" + uuid.NewString()[:8], Type: "generic", Ciphertext: []byte("x"), Nonce: []byte("n"), KeyID: "k1", FieldNames: []string{"f"}}
	expect("credential create", func() {
		if err := writer.Credentials().Create(ctx, cred); err != nil {
			t.Fatal(err)
		}
	}, func() []store.Invalidation {
		return []store.Invalidation{{Kind: "credential", Tenant: tn.ID, ID: cred.ID}}
	})
	expect("credential rotate", func() {
		if err := writer.Credentials().Rotate(ctx, tn.ID, cred.Name, []byte("y"), []byte("m"), "k1", []string{"f"}, store.UnlockedOnly); err != nil {
			t.Fatal(err)
		}
	}, func() []store.Invalidation {
		return []store.Invalidation{{Kind: "credential", Tenant: tn.ID, ID: cred.ID}}
	})

	src := &store.CredentialSource{TenantID: tn.ID, Name: "s-" + uuid.NewString()[:8], Provider: "external_service", Enabled: true, Config: map[string]any{"url": "https://x.example.com"}}
	expect("source create", func() {
		if err := writer.CredentialSources().Create(ctx, src); err != nil {
			t.Fatal(err)
		}
	}, func() []store.Invalidation { return []store.Invalidation{{Kind: "source", Tenant: tn.ID, ID: src.ID}} })
	src.Enabled = false
	expect("source update", func() {
		if err := writer.CredentialSources().Update(ctx, src, store.UnlockedOnly); err != nil {
			t.Fatal(err)
		}
	}, func() []store.Invalidation { return []store.Invalidation{{Kind: "source", Tenant: tn.ID, ID: src.ID}} })

	// Keys: a change announces the hash; using the key does not.
	key := &store.APIKey{TenantID: tn.ID, Name: "k", Role: "admin", KeyHash: "hash-" + uuid.NewString(), KeyPrefix: "gk_x"}
	if err := writer.APIKeys().Create(ctx, key); err != nil {
		t.Fatal(err)
	}
	expectNone("touch last_used_at", func() {
		if err := writer.APIKeys().TouchLastUsed(ctx, key.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	expect("key revoke", func() {
		if err := writer.APIKeys().Revoke(ctx, tn.ID, key.ID); err != nil {
			t.Fatal(err)
		}
	}, one(store.Invalidation{Kind: "key", Tenant: tn.ID, ID: key.KeyHash}))

	// Another tenant's writes are not this tenant's events (the listener
	// filter drops them), and a failed write publishes nothing.
	expectNone("write that fails", func() {
		_ = writer.Credentials().Rotate(ctx, tn.ID, "no-such-credential", []byte("y"), []byte("m"), "k1", nil, store.UnlockedOnly)
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not stop")
	}
}
