package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

type fakeTenants struct {
	store.TenantStore
	list []*store.Tenant
	err  error
}

func (f fakeTenants) List(context.Context) ([]*store.Tenant, error) { return f.list, f.err }

type fakeUsers struct {
	store.UserStore
	admins map[string]int
	err    error
}

func (f fakeUsers) CountActiveAdmins(_ context.Context, id string) (int, error) {
	return f.admins[id], f.err
}

type fakeCounter struct {
	t fakeTenants
	u fakeUsers
}

func (f fakeCounter) Tenants() store.TenantStore { return f.t }
func (f fakeCounter) Users() store.UserStore     { return f.u }

func newCounter() fakeCounter {
	return fakeCounter{
		t: fakeTenants{list: []*store.Tenant{{ID: "1", Slug: "acme"}, {ID: "2", Slug: "globex"}}},
		u: fakeUsers{admins: map[string]int{"1": 0, "2": 2}},
	}
}

func TestWarnAdminlessTenants(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	got := warnAdminlessTenants(context.Background(), newCounter(), false, logger)
	if len(got) != 1 || got[0] != "acme" {
		t.Fatalf("warned = %v, want [acme]", got)
	}
	want := "console API-key login is disabled and tenant acme has no active admin user; create one with 'gateway create-user'"
	if !strings.Contains(buf.String(), want) || !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("log = %q, want WARN containing %q", buf.String(), want)
	}
	if strings.Contains(buf.String(), "globex") {
		t.Errorf("tenant with an admin was warned about: %s", buf.String())
	}
}

func TestWarnAdminlessTenants_APIKeyLoginOnIsSilent(t *testing.T) {
	var buf bytes.Buffer
	got := warnAdminlessTenants(context.Background(), newCounter(), true, slog.New(slog.NewTextHandler(&buf, nil)))
	if got != nil || buf.Len() != 0 {
		t.Errorf("warned = %v, log = %q, want nothing", got, buf.String())
	}
}

func TestWarnAdminlessTenants_StoreErrorDoesNotWarnAboutTenants(t *testing.T) {
	c := newCounter()
	c.u.err = errors.New("db down")
	var buf bytes.Buffer
	got := warnAdminlessTenants(context.Background(), c, false, slog.New(slog.NewTextHandler(&buf, nil)))
	if len(got) != 0 {
		t.Errorf("warned = %v, want none on a read error", got)
	}
	c = newCounter()
	c.t.err = errors.New("db down")
	if got := warnAdminlessTenants(context.Background(), c, false, slog.New(slog.NewTextHandler(&buf, nil))); len(got) != 0 {
		t.Errorf("warned = %v, want none when tenants cannot be listed", got)
	}
}
