package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// RunUserIdentities runs only the user-identity (SSO link) conformance
// checks, for stores that are not run against the full suite (the
// in-memory store in internal/dataplane/dptest).
func RunUserIdentities(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()
	t.Run("UserIdentities", func(t *testing.T) { testUserIdentities(t, newStore(t)) })
}

func testUserIdentities(t *testing.T, s store.Store) {
	ctx := context.Background()
	ids := s.UserIdentities()

	tenantA := mustCreateTenant(t, ctx, s, "ident-a")
	tenantB := mustCreateTenant(t, ctx, s, "ident-b")
	alice := newUser(tenantA.ID, unique("alice"), store.UserRoleViewer)
	bob := newUser(tenantA.ID, unique("bob"), store.UserRoleViewer)
	carol := newUser(tenantB.ID, unique("carol"), store.UserRoleViewer)
	for _, u := range []*store.User{alice, bob, carol} {
		if err := s.Users().Create(ctx, u); err != nil {
			t.Fatalf("create user: %v", err)
		}
	}

	const issuer = "https://idp.example.com/realms/one"
	link := &store.UserIdentity{
		TenantID: tenantA.ID, Provider: "corp", Issuer: issuer, Subject: unique("sub"),
		UserID: alice.ID, Email: "alice@example.com",
	}
	if err := ids.Create(ctx, link); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if link.ID == "" || link.CreatedAt.IsZero() || link.LastLoginAt.IsZero() {
		t.Fatalf("Create did not populate server fields: %+v", link)
	}

	got, err := ids.GetBySubject(ctx, tenantA.ID, "corp", issuer, link.Subject)
	if err != nil {
		t.Fatalf("GetBySubject: %v", err)
	}
	if got.ID != link.ID || got.UserID != alice.ID || got.Email != "alice@example.com" || got.Issuer != issuer || got.Provider != "corp" {
		t.Errorf("GetBySubject = %+v, want the created link", got)
	}
	if got, err := ids.GetByUser(ctx, tenantA.ID, "corp", issuer, alice.ID); err != nil || got.ID != link.ID {
		t.Errorf("GetByUser = %+v, %v; want the created link", got, err)
	}

	// Every key part matters, and the tenant scopes every read.
	for name, fn := range map[string]func() error{
		"other tenant":   func() error { _, err := ids.GetBySubject(ctx, tenantB.ID, "corp", issuer, link.Subject); return err },
		"other provider": func() error { _, err := ids.GetBySubject(ctx, tenantA.ID, "other", issuer, link.Subject); return err },
		"other issuer": func() error {
			_, err := ids.GetBySubject(ctx, tenantA.ID, "corp", issuer+"x", link.Subject)
			return err
		},
		"other subject": func() error { _, err := ids.GetBySubject(ctx, tenantA.ID, "corp", issuer, unique("sub")); return err },
		"user, other tenant": func() error {
			_, err := ids.GetByUser(ctx, tenantB.ID, "corp", issuer, alice.ID)
			return err
		},
		"user without link": func() error { _, err := ids.GetByUser(ctx, tenantA.ID, "corp", issuer, bob.ID); return err },
	} {
		if err := fn(); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: error = %v, want ErrNotFound", name, err)
		}
	}

	// Unique per (tenant, provider, issuer, subject) and per (tenant,
	// provider, issuer, user).
	if err := ids.Create(ctx, &store.UserIdentity{TenantID: tenantA.ID, Provider: "corp", Issuer: issuer, Subject: link.Subject, UserID: bob.ID}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(same subject, other user) error = %v, want ErrConflict", err)
	}
	if err := ids.Create(ctx, &store.UserIdentity{TenantID: tenantA.ID, Provider: "corp", Issuer: issuer, Subject: unique("sub"), UserID: alice.ID}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(same user, other subject) error = %v, want ErrConflict", err)
	}
	// The same subject under another provider, issuer or tenant is a
	// different identity.
	second := &store.UserIdentity{TenantID: tenantA.ID, Provider: "corp", Issuer: issuer + "/two", Subject: link.Subject, UserID: alice.ID}
	if err := ids.Create(ctx, second); err != nil {
		t.Errorf("Create(other issuer): %v", err)
	}
	third := &store.UserIdentity{TenantID: tenantA.ID, Provider: "backup", Issuer: issuer, Subject: link.Subject, UserID: alice.ID}
	if err := ids.Create(ctx, third); err != nil {
		t.Errorf("Create(other provider): %v", err)
	}
	inB := &store.UserIdentity{TenantID: tenantB.ID, Provider: "corp", Issuer: issuer, Subject: link.Subject, UserID: carol.ID}
	if err := ids.Create(ctx, inB); err != nil {
		t.Errorf("Create(other tenant): %v", err)
	}

	list, err := ids.ListByUser(ctx, tenantA.ID, alice.ID)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(list) != 3 || list[0].Provider != "backup" || list[1].Provider != "corp" || list[1].Issuer != issuer || list[2].Issuer != issuer+"/two" {
		t.Errorf("ListByUser = %+v, want backup, corp/%s, corp/%s/two in that order", list, issuer, issuer)
	}
	if list, err := ids.ListByUser(ctx, tenantB.ID, alice.ID); err != nil || len(list) != 0 {
		t.Errorf("ListByUser(other tenant) = %+v, %v; want empty", list, err)
	}

	at := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	if err := ids.RecordLogin(ctx, tenantA.ID, link.ID, "alice.new@example.com", at); err != nil {
		t.Fatalf("RecordLogin: %v", err)
	}
	if got, _ := ids.GetBySubject(ctx, tenantA.ID, "corp", issuer, link.Subject); !got.LastLoginAt.Equal(at) || got.Email != "alice.new@example.com" {
		t.Errorf("after RecordLogin = %+v, want last_login_at %v and the new email", got, at)
	}
	if err := ids.RecordLogin(ctx, tenantB.ID, link.ID, "x@example.com", at); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RecordLogin(other tenant) error = %v, want ErrNotFound", err)
	}

	if err := ids.Delete(ctx, tenantB.ID, link.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Delete(other tenant) error = %v, want ErrNotFound", err)
	}
	if err := ids.Delete(ctx, tenantA.ID, link.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := ids.GetBySubject(ctx, tenantA.ID, "corp", issuer, link.Subject); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetBySubject after Delete error = %v, want ErrNotFound", err)
	}
	if err := ids.Delete(ctx, tenantA.ID, link.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Delete(twice) error = %v, want ErrNotFound", err)
	}
	// Once the link is gone the subject can be linked again (to another user).
	if err := ids.Create(ctx, &store.UserIdentity{TenantID: tenantA.ID, Provider: "corp", Issuer: issuer, Subject: link.Subject, UserID: bob.ID}); err != nil {
		t.Errorf("Create(after delete): %v", err)
	}
}
