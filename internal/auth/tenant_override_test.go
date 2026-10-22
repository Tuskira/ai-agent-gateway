package auth

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/dptest"
	applog "github.com/Tuskira/tusk-ai-secured-gateway/internal/log"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

const (
	homeTenant  = "11111111-1111-1111-1111-111111111111"
	otherTenant = "22222222-2222-2222-2222-222222222222"
	ghostTenant = "33333333-3333-3333-3333-333333333333"
)

// overrideFixture wires TenantOverride over the real RoleAuthorizer and
// the in-memory tenant store with two tenants, and captures the Principal
// and request logger the wrapped handler sees.
type overrideFixture struct {
	handler http.Handler
	seen    *pkgauth.Principal
	called  bool
	logs    bytes.Buffer
}

func newOverrideFixture(t *testing.T) *overrideFixture {
	t.Helper()
	st := dptest.New()
	for _, tn := range []*store.Tenant{
		{ID: homeTenant, Name: "Home", Slug: "home"},
		{ID: otherTenant, Name: "Other", Slug: "other"},
	} {
		if err := st.Tenants().Create(context.Background(), tn); err != nil {
			t.Fatal(err)
		}
	}
	f := &overrideFixture{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.called = true
		f.seen, _ = pkgauth.PrincipalFrom(r.Context())
		applog.From(r.Context()).Info("handled")
		w.WriteHeader(http.StatusNoContent)
	})
	f.handler = TenantOverride(pkgauth.NewRoleAuthorizer(), st.Tenants())(next)
	return f
}

// serve runs one request for p, with the acting header set when acting
// is non-empty, under a request logger that writes to f.logs.
func (f *overrideFixture) serve(p *pkgauth.Principal, acting string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/connectors", nil)
	if acting != "" {
		r.Header.Set(ActingTenantHeader, acting)
	}
	ctx := r.Context()
	if p != nil {
		ctx = pkgauth.WithPrincipal(ctx, p)
	}
	logger := slog.New(slog.NewTextHandler(&f.logs, nil))
	ctx = applog.With(ctx, logger.With("tenant_id", p.TenantID))
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r.WithContext(ctx))
	return w
}

func platformUser() *pkgauth.Principal {
	return &pkgauth.Principal{
		Subject: "u-platform", TenantID: homeTenant,
		Roles:      pkgauth.RolesForKey(pkgauth.RolePlatformAdmin),
		AuthMethod: pkgauth.AuthMethodSession, Username: "root@example.com",
		SessionID: "sess-1", CSRFToken: "csrf-1",
	}
}

func adminUser() *pkgauth.Principal {
	return &pkgauth.Principal{Subject: "u-admin", TenantID: homeTenant, Roles: []string{"admin"}, AuthMethod: pkgauth.AuthMethodSession}
}

func TestTenantOverride_AbsentHeaderPassesThrough(t *testing.T) {
	for name, p := range map[string]*pkgauth.Principal{
		"admin user":    adminUser(),
		"platform user": platformUser(),
		"admin key":     {Subject: "k1", TenantID: homeTenant, Roles: []string{"admin"}, AuthMethod: "apikey"},
	} {
		f := newOverrideFixture(t)
		w := f.serve(p, "")
		if w.Code != http.StatusNoContent || !f.called {
			t.Fatalf("%s: status = %d, called = %v; want 204 and the handler called", name, w.Code, f.called)
		}
		if f.seen != p {
			t.Errorf("%s: principal was replaced although no header was sent", name)
		}
		if f.seen.HomeTenantID != "" || f.seen.TenantID != homeTenant {
			t.Errorf("%s: principal = %+v, want untouched", name, f.seen)
		}
	}
}

func TestTenantOverride_WithoutPlatformAdminIs403(t *testing.T) {
	cases := map[string]*pkgauth.Principal{
		"admin user":  adminUser(),
		"viewer user": {Subject: "u-view", TenantID: homeTenant, Roles: []string{"viewer"}, AuthMethod: pkgauth.AuthMethodSession},
		"admin key":   {Subject: "k1", TenantID: homeTenant, Roles: []string{"admin"}, AuthMethod: "apikey"},
		"agent key":   {Subject: "k2", TenantID: homeTenant, Roles: []string{"agent"}, AuthMethod: "apikey"},
	}
	for name, p := range cases {
		f := newOverrideFixture(t)
		// Even the principal's own tenant is refused: the header is
		// inert without platform.admin.
		for _, acting := range []string{otherTenant, homeTenant, "not-a-uuid"} {
			w := f.serve(p, acting)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s with %q: status = %d, want 403", name, acting, w.Code)
			}
			body := decodeErrorBody(t, w)
			if body.Error.Type != "permission_error" || !strings.Contains(body.Error.Message, "platform.admin") {
				t.Errorf("%s: body = %+v", name, body)
			}
			if f.called {
				t.Fatalf("%s: handler ran despite the 403", name)
			}
		}
	}
}

func TestTenantOverride_NoPrincipalIs403(t *testing.T) {
	f := newOverrideFixture(t)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/connectors", nil)
	r.Header.Set(ActingTenantHeader, otherTenant)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || f.called {
		t.Fatalf("status = %d, called = %v; want 403 and the handler not called", w.Code, f.called)
	}
}

func TestTenantOverride_MalformedTenantIs400(t *testing.T) {
	for _, acting := range []string{"not-a-uuid", "home", "1111", " " + otherTenant} {
		f := newOverrideFixture(t)
		w := f.serve(platformUser(), acting)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%q: status = %d, want 400", acting, w.Code)
		}
		body := decodeErrorBody(t, w)
		if body.Error.Type != "validation_error" || !strings.Contains(body.Error.Message, ActingTenantHeader) {
			t.Errorf("%q: body = %+v", acting, body)
		}
		if f.called {
			t.Fatalf("%q: handler ran despite the 400", acting)
		}
	}
}

func TestTenantOverride_UnknownTenantIs404(t *testing.T) {
	f := newOverrideFixture(t)
	w := f.serve(platformUser(), ghostTenant)
	if w.Code != http.StatusNotFound || f.called {
		t.Fatalf("status = %d, called = %v; want 404 and the handler not called", w.Code, f.called)
	}
	body := decodeErrorBody(t, w)
	if body.Error.Type != "not_found" {
		t.Errorf("body = %+v", body)
	}
}

func TestTenantOverride_NilTenantStoreIs404(t *testing.T) {
	called := false
	h := TenantOverride(pkgauth.NewRoleAuthorizer(), nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(ActingTenantHeader, otherTenant)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r.WithContext(pkgauth.WithPrincipal(r.Context(), platformUser())))
	if w.Code != http.StatusNotFound || called {
		t.Fatalf("status = %d, called = %v; want 404 and the handler not called", w.Code, called)
	}
}

func TestTenantOverride_SuccessActsInOtherTenantAndKeepsHome(t *testing.T) {
	f := newOverrideFixture(t)
	p := platformUser()
	w := f.serve(p, otherTenant)
	if w.Code != http.StatusNoContent || !f.called {
		t.Fatalf("status = %d, called = %v; want 204 and the handler called", w.Code, f.called)
	}
	got := f.seen
	if got == nil || got == p {
		t.Fatalf("handler saw %v, want a fresh copy of the principal", got)
	}
	if got.TenantID != otherTenant {
		t.Errorf("TenantID = %q, want the acting tenant %q", got.TenantID, otherTenant)
	}
	if got.HomeTenantID != homeTenant || got.Home() != homeTenant {
		t.Errorf("HomeTenantID = %q, Home() = %q, want %q", got.HomeTenantID, got.Home(), homeTenant)
	}
	// Everything else is carried over unchanged.
	if got.Subject != p.Subject || got.AuthMethod != p.AuthMethod || got.Username != p.Username ||
		got.SessionID != p.SessionID || got.CSRFToken != p.CSRFToken || len(got.Roles) != 2 {
		t.Errorf("principal copy = %+v, want the original's fields", got)
	}
	// The original in the caller's context is untouched.
	if p.TenantID != homeTenant || p.HomeTenantID != "" {
		t.Errorf("original principal mutated: %+v", p)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "acting_tenant_id="+otherTenant) || !strings.Contains(logs, "tenant_id="+homeTenant) {
		t.Errorf("request log lacks acting_tenant_id and the home tenant_id: %q", logs)
	}
}

func TestTenantOverride_HeaderIsCanonicalised(t *testing.T) {
	// Every spelling uuid.Parse accepts names the same tenant; the handler
	// and the request log see the canonical lowercase form.
	for _, acting := range []string{
		strings.ToUpper(otherTenant),
		"{" + otherTenant + "}",
		"urn:uuid:" + strings.ToUpper(otherTenant),
	} {
		f := newOverrideFixture(t)
		w := f.serve(platformUser(), acting)
		if w.Code != http.StatusNoContent || !f.called {
			t.Fatalf("%q: status = %d, called = %v; want 204 and the handler called", acting, w.Code, f.called)
		}
		if f.seen.TenantID != otherTenant || f.seen.HomeTenantID != homeTenant {
			t.Errorf("%q: TenantID = %q, HomeTenantID = %q; want %q and %q", acting, f.seen.TenantID, f.seen.HomeTenantID, otherTenant, homeTenant)
		}
		if !strings.Contains(f.logs.String(), "acting_tenant_id="+otherTenant) {
			t.Errorf("%q: request log lacks the canonical acting_tenant_id: %q", acting, f.logs.String())
		}
	}
	// The caller's own tenant in uppercase is still the no-op.
	f := newOverrideFixture(t)
	p := platformUser()
	if w := f.serve(p, strings.ToUpper(homeTenant)); w.Code != http.StatusNoContent || f.seen != p {
		t.Errorf("uppercase own tenant: status = %d, principal = %+v; want 204 and the original", w.Code, f.seen)
	}
}

func TestTenantOverride_OwnTenantIsNoop(t *testing.T) {
	f := newOverrideFixture(t)
	p := platformUser()
	w := f.serve(p, homeTenant)
	if w.Code != http.StatusNoContent || !f.called {
		t.Fatalf("status = %d, called = %v; want 204 and the handler called", w.Code, f.called)
	}
	if f.seen != p || f.seen.HomeTenantID != "" {
		t.Errorf("principal = %+v, want the original untouched", f.seen)
	}
	if strings.Contains(f.logs.String(), "acting_tenant_id") {
		t.Errorf("acting_tenant_id logged for the principal's own tenant: %q", f.logs.String())
	}
}

func TestTenantOverride_PlatformKeyIsHonoured(t *testing.T) {
	f := newOverrideFixture(t)
	key := &pkgauth.Principal{Subject: "k-platform", TenantID: homeTenant, Roles: pkgauth.RolesForKey(pkgauth.RolePlatformAdmin), AuthMethod: "apikey", KeyID: "k-platform"}
	w := f.serve(key, otherTenant)
	if w.Code != http.StatusNoContent || f.seen == nil || f.seen.TenantID != otherTenant || f.seen.HomeTenantID != homeTenant {
		t.Fatalf("status = %d, principal = %+v; want 204 acting in %s from %s", w.Code, f.seen, otherTenant, homeTenant)
	}
}
