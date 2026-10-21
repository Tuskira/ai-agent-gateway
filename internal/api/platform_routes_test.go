package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/auth/session"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
)

const (
	testTenantID = "0b6f3f0e-5a5e-4d8e-9c35-0d4c3b1f7a21"
	testKeyID    = "1c7a4a1f-6b6f-4e9f-8d46-1e5d4c2a8b32"
)

type platformCall struct{ method, path, body string }

// platformRoutes is every route gated on platform.admin that touches
// tenants or their keys.
var platformRoutes = []platformCall{
	{http.MethodPost, "/api/v1/tenants", `{"id":"` + testTenantID + `","slug":"x","name":"x"}`},
	{http.MethodGet, "/api/v1/tenants", ""},
	{http.MethodGet, "/api/v1/tenants?slug=x", ""},
	{http.MethodPost, "/api/v1/tenants/" + testTenantID + "/api-keys", `{"name":"k","role":"agent"}`},
	{http.MethodGet, "/api/v1/tenants/" + testTenantID + "/api-keys", ""},
	{http.MethodPatch, "/api/v1/tenants/" + testTenantID + "/api-keys/" + testKeyID, `{"limits":{"rpm":5}}`},
	{http.MethodDelete, "/api/v1/tenants/" + testTenantID + "/api-keys/" + testKeyID, ""},
	{http.MethodPost, "/api/v1/tenants/" + testTenantID + "/api-keys/" + testKeyID + "/rotate", ""},
}

func platformRouter(t *testing.T, logger *slog.Logger) http.Handler {
	t.Helper()
	authz := pkgauth.NewRoleAuthorizer()
	// A custom role with another platform.* permission but not
	// platform.admin: must not reach these routes.
	authz.Rules["ops"] = []string{"platform.catalog.manage", "connector.*"}
	return NewRouter(Deps{
		ServiceVersion: "test",
		Logger:         logger,
		Authorizer:     authz,
		Store:          nopStore{},
		Authenticator: &scriptedAuthenticator{principals: map[string]*pkgauth.Principal{
			"admin":       {Subject: "admin-key", TenantID: testTenantID, Roles: []string{"admin"}, AuthMethod: "apikey"},
			"agent":       {Subject: "agent-key", TenantID: "t", Roles: []string{"agent"}, AuthMethod: "apikey"},
			"viewer":      {Subject: "viewer-key", TenantID: "t", Roles: []string{"viewer"}, AuthMethod: "apikey"},
			"interceptor": {Subject: "icp-key", TenantID: "t", Roles: []string{"interceptor"}, AuthMethod: "apikey"},
			"ops":         {Subject: "ops-key", TenantID: "t", Roles: []string{"ops"}, AuthMethod: "apikey"},
			"console":     {Subject: "user-1", TenantID: "t", Roles: []string{"admin", "platform-admin"}, AuthMethod: pkgauth.AuthMethodSession, CSRFToken: "csrf"},
			"platform":    {Subject: "platform-key", TenantID: "home", Roles: pkgauth.RolesForKey(pkgauth.RolePlatformAdmin), AuthMethod: "apikey"},
		}},
	})
}

func doPlatform(h http.Handler, c platformCall, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(session.CSRFHeader, "csrf")
	req.Header.Set("X-Request-Id", "req-123")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// Every platform route refuses everyone but a platform-admin key: tenant
// admins (even for their own tenant), agents, viewers, interceptors, a
// custom role holding a different platform.* permission, and a console
// session (even one whose roles include platform-admin). The denial must
// come from the permission layer, not from CSRF or validation.
func TestPlatformRoutes_RefuseEveryoneButPlatformAdmin(t *testing.T) {
	h := platformRouter(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	for _, c := range platformRoutes {
		for _, who := range []string{"admin", "agent", "viewer", "interceptor", "ops", "console"} {
			w := doPlatform(h, c, who)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "permission_error") {
				t.Errorf("%s %s %s %s = %d %s, want 403 permission_error", who, c.method, c.path, c.body, w.Code, w.Body.String())
			}
		}
		if w := doPlatform(h, c, "platform"); w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
			t.Errorf("platform %s %s = %d, want the permission to pass", c.method, c.path, w.Code)
		}
	}
}

// An unknown tenant is a 404 on the per-tenant routes (nopStore has none).
func TestPlatformRoutes_UnknownTenant404(t *testing.T) {
	h := platformRouter(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	for _, c := range platformRoutes[3:] {
		if w := doPlatform(h, c, "platform"); w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", c.method, c.path, w.Code)
		}
	}
}

func TestTenantsList_EmptySlug400(t *testing.T) {
	h := platformRouter(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	for _, q := range []string{"?slug=", "?slug=x&slug=", "?slug=&slug=x"} {
		if w := doPlatform(h, platformCall{http.MethodGet, "/api/v1/tenants" + q, ""}, "platform"); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", q, w.Code)
		}
	}
}

func TestPlatformAudit_RequestIDIsCapped(t *testing.T) {
	var buf bytes.Buffer
	h := platformRouter(t, slog.New(slog.NewJSONHandler(&buf, nil)))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tenants", nil)
	req.Header.Set("Authorization", "Bearer platform")
	req.Header.Set("X-Request-Id", strings.Repeat("é", 500))
	h.ServeHTTP(httptest.NewRecorder(), req)
	for _, l := range parseLogs(t, &buf) {
		if id, _ := l["request_id"].(string); len([]rune(id)) != 128 {
			t.Errorf("request_id has %d runes, want 128", len([]rune(id)))
		}
	}
}

type logLine map[string]any

func parseLogs(t *testing.T, buf *bytes.Buffer) []logLine {
	t.Helper()
	var out []logLine
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m logLine
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

// Each platform request writes an intent line and a result line, for
// successes and for refused or failed attempts alike.
func TestPlatformRoutes_AuditLines(t *testing.T) {
	var buf bytes.Buffer
	h := platformRouter(t, slog.New(slog.NewJSONHandler(&buf, nil)))

	// Success: tenant creation with a chosen id.
	doPlatform(h, platformRoutes[0], "platform")
	// Denied: a tenant admin on the key-create route.
	doPlatform(h, platformRoutes[3], "admin")
	// Not found: unknown tenant.
	doPlatform(h, platformRoutes[5], "platform")

	var reqs, results []logLine
	for _, l := range parseLogs(t, &buf) {
		switch l["msg"] {
		case "platform request":
			reqs = append(reqs, l)
		case "platform result":
			results = append(results, l)
		}
	}
	if len(reqs) != 3 || len(results) != 3 {
		t.Fatalf("got %d request and %d result lines, want 3 each: %s", len(reqs), len(results), buf.String())
	}

	ok := results[0]
	if ok["outcome"] != "ok" || ok["status"] != float64(201) || ok["actor"] != "platform-key" || ok["actor_tenant_id"] != "home" ||
		ok["route"] != "/api/v1/tenants" || ok["created_tenant_id"] != testTenantID || ok["created_tenant_slug"] != "x" ||
		ok["id_supplied"] != true || ok["request_id"] != "req-123" || ok["client_ip"] == "" || ok["client_ip"] == nil {
		t.Errorf("tenant create result = %v", ok)
	}
	denied := results[1]
	if denied["outcome"] != "denied" || denied["status"] != float64(403) || denied["actor"] != "admin-key" || denied["tenant_id"] != testTenantID {
		t.Errorf("denied result = %v", denied)
	}
	missing := results[2]
	if missing["outcome"] != "not_found" || missing["status"] != float64(404) || missing["key_id"] != testKeyID || missing["tenant_id"] != testTenantID {
		t.Errorf("not-found result = %v", missing)
	}
	for _, l := range reqs {
		if l["actor"] == nil || l["route"] == nil || l["method"] == nil {
			t.Errorf("intent line missing fields: %v", l)
		}
	}
	if strings.Contains(buf.String(), "gk_") || strings.Contains(buf.String(), `"key":`) {
		t.Errorf("log may contain a key: %s", buf.String())
	}
}
