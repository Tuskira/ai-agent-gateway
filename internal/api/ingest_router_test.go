package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// routerFakeIngestSink is a minimal sink.IngestSink: it accepts everything
// it's handed, with no duplicate detection -- these tests are about
// routing/permission (does POST /api/v1/ingest reach the handler and does
// the handler run at all), not about the sink's own dedup logic (covered
// by pkg/sink/clickhouse's own tests).
type routerFakeIngestSink struct{}

func (routerFakeIngestSink) WriteIngestBatch(_ context.Context, _ string, access []*sink.AccessLog, llm []*sink.LLMCall) (sink.IngestResult, error) {
	return sink.IngestResult{AcceptedAccess: len(access), AcceptedLLM: len(llm)}, nil
}

// TestIngest_RolePermissions_FullStack exercises the actual route wiring
// (Permission: "ingest.write" on POST /api/v1/ingest) through the real
// pkgauth.RoleAuthorizer: the built-in "interceptor" role is granted, the
// built-in "agent" role (its "*.read" grant does not satisfy a write
// permission) and a custom read-only role are both refused with 403, and
// admin's blanket "*" still covers it -- the auth cases "wrong role gets
// 403, interceptor role gets 200", plus admin passing on purpose.
func TestIngest_RolePermissions_FullStack(t *testing.T) {
	authorizer := pkgauth.NewRoleAuthorizer()
	authorizer.Rules["reader-only"] = []string{"*.read"}

	principals := map[string]*pkgauth.Principal{
		"admin-a":       {Subject: "admin-a", TenantID: "tenant-a", KeyID: "key-admin", Roles: []string{"admin"}, AuthMethod: "test"},
		"agent-a":       {Subject: "agent-a", TenantID: "tenant-a", KeyID: "key-agent", Roles: []string{"agent"}, AuthMethod: "test"},
		"interceptor-a": {Subject: "interceptor-a", TenantID: "tenant-a", KeyID: "key-interceptor", Roles: []string{"interceptor"}, AuthMethod: "test"},
		"reader-a":      {Subject: "reader-a", TenantID: "tenant-a", KeyID: "key-reader", Roles: []string{"reader-only"}, AuthMethod: "test"},
	}

	h := NewRouter(Deps{
		ServiceVersion: "test",
		Authenticator:  &scriptedAuthenticator{principals: principals},
		Authorizer:     authorizer,
		Store:          nopStore{},
		IngestConfig:   config.Ingest{Enabled: true, MaxBodyBytes: 1 << 20, MaxRecords: 1000, RatePerMinute: 0},
		IngestSink:     routerFakeIngestSink{},
	})

	do := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(`{"schema_version":1}`))
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := do("interceptor-a"); w.Code != http.StatusOK {
		t.Errorf("interceptor role POST /ingest = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if w := do("admin-a"); w.Code != http.StatusOK {
		t.Errorf("admin role POST /ingest = %d, want 200 (admin's blanket \"*\" covers ingest.write), body=%s", w.Code, w.Body.String())
	}
	if w := do("agent-a"); w.Code != http.StatusForbidden {
		t.Errorf("agent role POST /ingest = %d, want 403 (agent's *.read must not satisfy ingest.write)", w.Code)
	}
	if w := do("reader-a"); w.Code != http.StatusForbidden {
		t.Errorf("reader-only role POST /ingest = %d, want 403", w.Code)
	}
	if w := do(""); w.Code != http.StatusUnauthorized {
		t.Errorf("no credential POST /ingest = %d, want 401", w.Code)
	}
}

// TestIngest_Disabled_NotFoundRegardlessOfRole proves the disabled-route
// 404 (Ingest.Ingest checking IngestConfig.Enabled) is reachable and
// correctly shaped for an authenticated, correctly-permissioned caller --
// the scenario the "indistinguishable from an unknown route" requirement
// is actually scoped to (see handlers.Ingest's doc comment on the
// auth-vs-enabled-check ordering).
func TestIngest_Disabled_NotFoundRegardlessOfRole(t *testing.T) {
	principals := map[string]*pkgauth.Principal{
		"interceptor-a": {Subject: "interceptor-a", TenantID: "tenant-a", KeyID: "key-interceptor", Roles: []string{"interceptor"}, AuthMethod: "test"},
	}
	h := NewRouter(Deps{
		ServiceVersion: "test",
		Authenticator:  &scriptedAuthenticator{principals: principals},
		Authorizer:     pkgauth.NewRoleAuthorizer(),
		Store:          nopStore{},
		IngestConfig:   config.Ingest{Enabled: false},
		IngestSink:     routerFakeIngestSink{},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(`{"schema_version":1}`))
	req.Header.Set("Authorization", "Bearer interceptor-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}

	// Compare against an actually-unknown route's 404 to prove the bodies
	// are byte-for-byte identical.
	unknownReq := httptest.NewRequest(http.MethodGet, "/api/v1/this-route-does-not-exist", nil)
	unknownW := httptest.NewRecorder()
	h.ServeHTTP(unknownW, unknownReq)

	if w.Body.String() != unknownW.Body.String() {
		t.Errorf("disabled ingest body = %q, unknown-route body = %q, want identical", w.Body.String(), unknownW.Body.String())
	}
}
