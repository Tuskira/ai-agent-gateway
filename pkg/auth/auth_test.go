package auth

import (
	"context"
	"testing"
)

func TestRoleAuthorizer_Allow(t *testing.T) {
	authz := NewRoleAuthorizer()
	ctx := context.Background()

	tests := []struct {
		name       string
		roles      []string
		permission string
		want       bool
	}{
		{"admin allows anything", []string{"admin"}, "connector.create", true},
		{"admin allows deeply nested permission", []string{"admin"}, "mcp.tools.call.retry", true},
		{"agent allows mcp prefix", []string{"agent"}, "mcp.tools.call", true},
		{"agent allows bare mcp", []string{"agent"}, "mcp", true},
		{"agent allows llm prefix", []string{"agent"}, "llm.messages.create", true},
		{"agent allows any two-segment read", []string{"agent"}, "connector.read", true},
		{"agent allows profile read", []string{"agent"}, "profile.read", true},
		{"agent denies write", []string{"agent"}, "connector.write", false},
		{"agent denies create", []string{"agent"}, "connector.create", false},
		{"agent denies three-segment read", []string{"agent"}, "connector.tools.read", false},
		{"unknown role denies everything", []string{"nobody"}, "connector.read", false},
		{"no roles denies everything", []string{}, "connector.read", false},
		{"multiple roles union", []string{"nobody", "agent"}, "llm.messages.create", true},
		{"admin does not allow platform.admin", []string{"admin"}, "platform.admin", false},
		{"platform-admin allows platform.admin", []string{"platform-admin"}, "platform.admin", true},
		{"agent's *.read does not allow platform.admin", []string{"agent"}, "platform.admin", false},
		{"interceptor allows ingest.write", []string{"interceptor"}, "ingest.write", true},
		{"interceptor denies everything else", []string{"interceptor"}, "ingest.read", false},
		{"interceptor denies mcp", []string{"interceptor"}, "mcp.tools.call", false},
		{"agent's *.read does not allow ingest.write (read grant never matches a write permission)", []string{"agent"}, "ingest.write", false},
		{"admin allows ingest.write", []string{"admin"}, "ingest.write", true},
		{"viewer allows any read", []string{"viewer"}, "connector.read", true},
		{"viewer allows analytics read", []string{"viewer"}, "analytics.read", true},
		{"viewer denies write", []string{"viewer"}, "connector.create", false},
		{"viewer denies mcp", []string{"viewer"}, "mcp.tools.call", false},
		{"viewer denies llm", []string{"viewer"}, "llm.messages.create", false},
		{"viewer denies users.manage", []string{"viewer"}, PermUsersManage, false},
		{"viewer denies admin.manage", []string{"viewer"}, "admin.manage", false},
		{"viewer denies platform.admin", []string{"viewer"}, PermPlatformAdmin, false},
		{"admin allows users.manage", []string{"admin"}, PermUsersManage, true},
		{"agent denies users.manage", []string{"agent"}, PermUsersManage, false},
		{"interceptor denies users.manage", []string{"interceptor"}, PermUsersManage, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Principal{Subject: "u1", TenantID: "t1", Roles: tt.roles}
			if got := authz.Allow(ctx, p, tt.permission); got != tt.want {
				t.Errorf("Allow(roles=%v, perm=%q) = %v, want %v", tt.roles, tt.permission, got, tt.want)
			}
		})
	}
}

func TestRoleAuthorizer_Allow_NilPrincipal(t *testing.T) {
	authz := NewRoleAuthorizer()
	if authz.Allow(context.Background(), nil, "connector.read") {
		t.Error("Allow(nil principal) = true, want false")
	}
}

func TestRoleAuthorizer_CustomRole(t *testing.T) {
	authz := &RoleAuthorizer{Rules: map[string][]string{
		"billing": {"invoices.*"},
	}}
	p := &Principal{Roles: []string{"billing"}}

	if !authz.Allow(context.Background(), p, "invoices.export") {
		t.Error("expected billing role to allow invoices.export")
	}
	if authz.Allow(context.Background(), p, "connector.read") {
		t.Error("expected billing role to deny connector.read")
	}
}

// TestRoleAuthorizer_CustomRole_BareStarDoesNotGrantPlatformAdmin pins the
// platform namespace carve-out (see matchPermission): a custom role
// granted only the blanket "*" pattern -- which the trailing-wildcard
// rule would otherwise let match anything, including platform.admin --
// must NOT be treated as platform-admin. Only platform-admin (via its
// "platform.*" grant, see NewRoleAuthorizer) or a role explicitly
// given "platform.*"/"platform.admin" should pass.
func TestRoleAuthorizer_CustomRole_BareStarDoesNotGrantPlatformAdmin(t *testing.T) {
	authz := &RoleAuthorizer{Rules: map[string][]string{
		"superstar": {"*"},
	}}
	p := &Principal{Roles: []string{"superstar"}}

	if authz.Allow(context.Background(), p, "platform.admin") {
		t.Error("a role holding only the bare \"*\" pattern must not be granted platform.admin")
	}
	// Sanity: it still allows everything else, so this isn't just a
	// broken authorizer.
	if !authz.Allow(context.Background(), p, "connector.create") {
		t.Error("expected the \"*\" pattern to still allow an ordinary permission")
	}

	explicit := &RoleAuthorizer{Rules: map[string][]string{
		"platform-admin": {"platform.*"},
	}}
	pe := &Principal{Roles: []string{"platform-admin"}}
	if !explicit.Allow(context.Background(), pe, "platform.admin") {
		t.Error("a role explicitly granted platform.* must be granted platform.admin")
	}
}

func TestWithPrincipal_PrincipalFrom(t *testing.T) {
	ctx := context.Background()

	if _, ok := PrincipalFrom(ctx); ok {
		t.Fatal("PrincipalFrom(empty ctx) ok = true, want false")
	}

	want := &Principal{Subject: "u1", TenantID: "t1"}
	ctx = WithPrincipal(ctx, want)

	got, ok := PrincipalFrom(ctx)
	if !ok {
		t.Fatal("PrincipalFrom(ctx) ok = false, want true")
	}
	if got != want {
		t.Errorf("PrincipalFrom(ctx) = %+v, want %+v", got, want)
	}
}

func TestMatchPermission(t *testing.T) {
	tests := []struct {
		pattern, permission string
		want                bool
	}{
		{"*", "anything.at.all", true},
		{"*", "", true},
		{"mcp.*", "mcp", true},
		{"mcp.*", "mcp.tools", true},
		{"mcp.*", "mcp.tools.call", true},
		{"mcp.*", "llm.tools.call", false},
		{"*.read", "connector.read", true},
		{"*.read", "connector.write", false},
		{"*.read", "connector.tools.read", false},
		{"connector.create", "connector.create", true},
		{"connector.create", "connector.read", false},
		{"*", "platform.admin", false},
		{"*.read", "platform.read", false},
		{"platform.*", "platform.admin", true},
		{"platform.*", "platform", true},
		{"platform.admin", "platform.admin", true},
		{"platform.admin", "platform.other", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"__"+tt.permission, func(t *testing.T) {
			if got := matchPermission(tt.pattern, tt.permission); got != tt.want {
				t.Errorf("matchPermission(%q, %q) = %v, want %v", tt.pattern, tt.permission, got, tt.want)
			}
		})
	}
}

func TestRoleAuthorizer_SessionAdminLacksPlatformAdmin(t *testing.T) {
	authz := NewRoleAuthorizer()
	ctx := context.Background()

	user := &Principal{Subject: "u1", TenantID: "t1", Roles: []string{"admin"}, AuthMethod: AuthMethodSession}
	if authz.Allow(ctx, user, PermPlatformAdmin) {
		t.Error("session admin was granted platform.admin")
	}
	for _, perm := range []string{"connector.create", "admin.manage", PermUsersManage, "mcp.tools.call", "analytics.read"} {
		if !authz.Allow(ctx, user, perm) {
			t.Errorf("session admin denied %q, want allowed (admin minus platform.admin)", perm)
		}
	}

	// A plain admin API key is a tenant admin: no platform.admin either.
	key := &Principal{Subject: "k1", TenantID: "t1", Roles: []string{"admin"}, AuthMethod: "apikey"}
	if authz.Allow(ctx, key, PermPlatformAdmin) {
		t.Error("tenant admin API key was granted platform.admin")
	}
	// A platform-admin key (roles expanded by RolesForKey) holds both.
	pkey := &Principal{Subject: "k2", TenantID: "t1", Roles: RolesForKey(RolePlatformAdmin), AuthMethod: "apikey"}
	if !authz.Allow(ctx, pkey, PermPlatformAdmin) || !authz.Allow(ctx, pkey, "connector.create") {
		t.Error("platform-admin key must hold platform.admin and tenant admin permissions")
	}
}

func TestRoleAuthorizer_PlatformNamespace(t *testing.T) {
	authz := NewRoleAuthorizer()
	ctx := context.Background()
	admin := &Principal{Roles: []string{"admin"}, AuthMethod: "apikey"}
	platOnly := &Principal{Roles: []string{RolePlatformAdmin}, AuthMethod: "apikey"}
	for _, perm := range []string{"platform.admin", PermCatalogManage, "platform.anything.else"} {
		if authz.Allow(ctx, admin, perm) {
			t.Errorf("admin ('*') matched %q; * must never cover platform.*", perm)
		}
		if !authz.Allow(ctx, platOnly, perm) {
			t.Errorf("platform-admin denied %q", perm)
		}
	}
	// platform-admin alone is not a tenant admin.
	if authz.Allow(ctx, platOnly, "connector.create") {
		t.Error("platform-admin role alone must not grant tenant permissions")
	}
	if got := RolesForKey("admin"); len(got) != 1 || got[0] != "admin" {
		t.Errorf("RolesForKey(admin) = %v", got)
	}
	if got := RolesForKey(RolePlatformAdmin); len(got) != 2 {
		t.Errorf("RolesForKey(platform-admin) = %v", got)
	}
	// A non-platform pattern never matches the namespace.
	for _, pat := range []string{"*", "*.read", "*.admin"} {
		if matchPermission(pat, "platform.admin") {
			t.Errorf("pattern %q matched platform.admin", pat)
		}
	}
}

func TestRoleAuthorizer_CatalogManage(t *testing.T) {
	ctx := context.Background()
	sessionAdmin := &Principal{Subject: "u1", TenantID: "t1", Roles: []string{"admin"}, AuthMethod: AuthMethodSession}
	sessionViewer := &Principal{Subject: "u2", TenantID: "t1", Roles: []string{"viewer"}, AuthMethod: AuthMethodSession}
	apikeyAdmin := &Principal{Subject: "k1", TenantID: "t1", Roles: []string{"admin"}, AuthMethod: "apikey"}
	apikeyAgent := &Principal{Subject: "k2", TenantID: "t1", Roles: []string{"agent"}, AuthMethod: "apikey"}
	apikeyPlatform := &Principal{Subject: "k3", TenantID: "t1", Roles: RolesForKey(RolePlatformAdmin), AuthMethod: "apikey"}

	t.Run("nil SingleTenant fails closed for a session admin", func(t *testing.T) {
		authz := NewRoleAuthorizer() // SingleTenant left nil
		if authz.Allow(ctx, sessionAdmin, PermCatalogManage) {
			t.Error("session admin granted platform.catalog.manage with SingleTenant unset")
		}
		if authz.Allow(ctx, apikeyAdmin, PermCatalogManage) {
			t.Error("tenant-admin API key granted platform.catalog.manage")
		}
		if !authz.Allow(ctx, apikeyPlatform, PermCatalogManage) {
			t.Error("platform-admin API key denied platform.catalog.manage")
		}
	})

	t.Run("single-tenant grants a session admin, multi-tenant does not", func(t *testing.T) {
		single := true
		authz := NewRoleAuthorizer()
		authz.SingleTenant = func(context.Context) bool { return single }

		if !authz.Allow(ctx, sessionAdmin, PermCatalogManage) {
			t.Error("session admin denied platform.catalog.manage while single-tenant")
		}
		single = false
		if authz.Allow(ctx, sessionAdmin, PermCatalogManage) {
			t.Error("session admin granted platform.catalog.manage while multi-tenant")
		}
	})

	t.Run("API-key admin and viewer/agent are unaffected by SingleTenant", func(t *testing.T) {
		for _, single := range []bool{true, false} {
			authz := NewRoleAuthorizer()
			authz.SingleTenant = func(context.Context) bool { return single }

			if authz.Allow(ctx, apikeyAdmin, PermCatalogManage) {
				t.Errorf("single=%v: tenant-admin API key granted platform.catalog.manage", single)
			}
			if !authz.Allow(ctx, apikeyPlatform, PermCatalogManage) {
				t.Errorf("single=%v: platform-admin API key denied platform.catalog.manage", single)
			}
			if authz.Allow(ctx, apikeyAgent, PermCatalogManage) {
				t.Errorf("single=%v: API-key agent granted platform.catalog.manage", single)
			}
			if authz.Allow(ctx, sessionViewer, PermCatalogManage) {
				t.Errorf("single=%v: session viewer granted platform.catalog.manage", single)
			}
		}
	})
}
