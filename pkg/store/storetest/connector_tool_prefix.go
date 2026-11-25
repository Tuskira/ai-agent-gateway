package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// testConnectorToolPrefix covers the optional Connector.ToolPrefix: it
// round-trips, it is looked up case-insensitively, and a prefix shares one
// namespace per tenant with the live connectors' slugs and prefixes.
func testConnectorToolPrefix(t *testing.T, s store.Store) {
	ctx := context.Background()
	cs := s.Connectors()
	tenantA := mustCreateTenant(t, ctx, s, "pfx-a")
	tenantB := mustCreateTenant(t, ctx, s, "pfx-b")

	mk := func(tenant, slug, prefix string) *store.Connector {
		return &store.Connector{TenantID: tenant, Name: slug, Slug: slug, ToolPrefix: prefix, Endpoint: "https://mcp.example.com/" + slug, TimeoutMS: 1000}
	}

	prefix := "Vendor.Intel_" + unique("p")
	a := mk(tenantA.ID, unique("a"), prefix)
	if err := cs.Create(ctx, a); err != nil {
		t.Fatalf("Create with prefix: %v", err)
	}
	plain := mk(tenantA.ID, unique("plain"), "")
	if err := cs.Create(ctx, plain); err != nil {
		t.Fatalf("Create without prefix: %v", err)
	}

	// Round trip and lookup, case-insensitive; no prefix reads back "".
	got, err := cs.Get(ctx, tenantA.ID, a.ID)
	if err != nil || got.ToolPrefix != prefix {
		t.Fatalf("Get = %+v, %v; want ToolPrefix %q", got, err, prefix)
	}
	if got, err = cs.Get(ctx, tenantA.ID, plain.ID); err != nil || got.ToolPrefix != "" {
		t.Errorf("Get(plain) = %+v, %v; want empty ToolPrefix", got, err)
	}
	for _, q := range []string{prefix, "vendor.intel_" + prefix[len("Vendor.Intel_"):], "VENDOR.INTEL_" + prefix[len("Vendor.Intel_"):]} {
		if got, err = cs.GetByToolPrefix(ctx, tenantA.ID, q); err != nil || got.ID != a.ID {
			t.Errorf("GetByToolPrefix(%q) = %+v, %v; want connector %s", q, got, err, a.ID)
		}
	}
	if _, err = cs.GetByToolPrefix(ctx, tenantB.ID, prefix); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByToolPrefix(other tenant) error = %v, want ErrNotFound", err)
	}
	if _, err = cs.GetByToolPrefix(ctx, tenantA.ID, plain.Slug); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByToolPrefix(a slug) error = %v, want ErrNotFound", err)
	}
	list, err := cs.List(ctx, tenantA.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, c := range list {
		if c.ID == a.ID && c.ToolPrefix != prefix {
			t.Errorf("List row ToolPrefix = %q, want %q", c.ToolPrefix, prefix)
		}
	}

	// Prefix against prefix: same text, any case, is a conflict; another
	// tenant may reuse it.
	if err := cs.Create(ctx, mk(tenantA.ID, unique("d"), prefix)); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(duplicate prefix) error = %v, want ErrConflict", err)
	}
	if err := cs.Create(ctx, mk(tenantA.ID, unique("d"), "VENDOR.INTEL_"+prefix[len("Vendor.Intel_"):])); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(prefix differing only in case) error = %v, want ErrConflict", err)
	}
	if err := cs.Create(ctx, mk(tenantB.ID, unique("b"), prefix)); err != nil {
		t.Errorf("Create(same prefix, other tenant) error = %v, want nil", err)
	}

	// Prefix against another connector's slug, also differing in case; and
	// a slug against another connector's prefix.
	if err := cs.Create(ctx, mk(tenantA.ID, unique("e"), plain.Slug)); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(prefix = another slug) error = %v, want ErrConflict", err)
	}
	upper := []byte(plain.Slug)
	for i, ch := range upper {
		if ch >= 'a' && ch <= 'z' {
			upper[i] = ch - 32
		}
	}
	if err := cs.Create(ctx, mk(tenantA.ID, unique("e"), string(upper))); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(prefix = another slug in other case) error = %v, want ErrConflict", err)
	}
	if err := cs.Create(ctx, mk(tenantA.ID, "vendor.intel_"+prefix[len("Vendor.Intel_"):], "")); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Create(slug = another prefix) error = %v, want ErrConflict", err)
	}
	// A connector's own prefix may equal its own slug.
	self := mk(tenantA.ID, unique("self"), "")
	if err := cs.Create(ctx, self); err != nil {
		t.Fatal(err)
	}
	self.ToolPrefix = self.Slug
	if err := cs.Update(ctx, self); err != nil {
		t.Errorf("Update(prefix = own slug) error = %v, want nil", err)
	}

	// Update: set, change, clear. A colliding Update does not persist.
	plain.ToolPrefix = prefix
	if err := cs.Update(ctx, plain); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Update(colliding prefix) error = %v, want ErrConflict", err)
	}
	plain.ToolPrefix = "other_" + unique("p")
	if err := cs.Update(ctx, plain); err != nil {
		t.Fatalf("Update(set prefix): %v", err)
	}
	if got, _ = cs.GetByToolPrefix(ctx, tenantA.ID, plain.ToolPrefix); got == nil || got.ID != plain.ID {
		t.Errorf("GetByToolPrefix after Update = %+v, want %s", got, plain.ID)
	}
	old := plain.ToolPrefix
	plain.ToolPrefix = ""
	if err := cs.Update(ctx, plain); err != nil {
		t.Fatalf("Update(clear prefix): %v", err)
	}
	if got, err = cs.Get(ctx, tenantA.ID, plain.ID); err != nil || got.ToolPrefix != "" {
		t.Errorf("Get after clear = %+v, %v; want empty ToolPrefix", got, err)
	}
	if _, err = cs.GetByToolPrefix(ctx, tenantA.ID, old); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByToolPrefix(cleared) error = %v, want ErrNotFound", err)
	}
	// Two connectors without a prefix do not collide with each other.
	if err := cs.Create(ctx, mk(tenantA.ID, unique("n1"), "")); err != nil {
		t.Errorf("Create(no prefix) #1: %v", err)
	}
	if err := cs.Create(ctx, mk(tenantA.ID, unique("n2"), "")); err != nil {
		t.Errorf("Create(no prefix) #2: %v", err)
	}

	// SoftDelete frees the prefix and hides it from the lookup.
	if err := cs.SoftDelete(ctx, tenantA.ID, a.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, err = cs.GetByToolPrefix(ctx, tenantA.ID, prefix); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByToolPrefix after delete error = %v, want ErrNotFound", err)
	}
	if err := cs.Create(ctx, mk(tenantA.ID, unique("reuse"), prefix)); err != nil {
		t.Errorf("Create with a soft-deleted connector's prefix: %v", err)
	}

	// Concurrent creates with the same prefix: exactly one wins, the other
	// is a conflict. The same for a prefix racing a slug of the same text.
	race := func(name string, a, b *store.Connector) {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, c := range []*store.Connector{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = cs.Create(ctx, c)
			}()
		}
		wg.Wait()
		ok, conflict := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, store.ErrConflict):
				conflict++
			default:
				t.Errorf("%s: unexpected error %v", name, err)
			}
		}
		if ok != 1 || conflict != 1 {
			t.Errorf("%s: %d created, %d conflicts; want 1 and 1", name, ok, conflict)
		}
	}
	rp := "Race." + unique("r")
	race("same prefix", mk(tenantA.ID, unique("r1"), rp), mk(tenantA.ID, unique("r2"), rp))
	rs := unique("rslug")
	race("prefix vs slug", mk(tenantA.ID, rs, ""), mk(tenantA.ID, unique("r3"), rs))
}
