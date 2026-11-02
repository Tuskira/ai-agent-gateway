package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
)

// resolverServer is a real HTTP server speaking the credential-service
// contract, standing in for an outside service.
type resolverServer struct {
	*httptest.Server
	token string

	mu       sync.Mutex
	value    string
	ttl      int64
	status   int
	requests []map[string]string
	resolves atomic.Int32
	options  atomic.Int32
	lastAuth string
	lastQS   string
}

func newResolverServer(t *testing.T, token, value string) *resolverServer {
	t.Helper()
	rs := &resolverServer{token: token, value: value, status: http.StatusOK}
	rs.Server = httptest.NewServer(http.HandlerFunc(rs.serve))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *resolverServer) serve(w http.ResponseWriter, r *http.Request) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.lastAuth = r.Header.Get("Authorization")
	if rs.lastAuth != "Bearer "+rs.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/credentials/resolve":
		rs.resolves.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		rs.requests = append(rs.requests, body)
		if rs.status != http.StatusOK {
			w.WriteHeader(rs.status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"value": rs.value, "ttl_seconds": rs.ttl})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/credentials/options":
		rs.options.Add(1)
		rs.lastQS = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"options": []map[string]string{{"id": "splunk-prod"}}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (rs *resolverServer) set(value string, ttl int64, status int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.value, rs.ttl, rs.status = value, ttl, status
}

func (rs *resolverServer) lastRequest() map[string]string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.requests) == 0 {
		return nil
	}
	return rs.requests[len(rs.requests)-1]
}

// newExternalServiceFixture builds the provider over a real secrets.Service
// holding tenant-a's auth credential, and a resolver server.
func newExternalServiceFixture(t *testing.T) (*ExternalServiceProvider, *resolverServer, *Service) {
	t.Helper()
	svc, _ := newTestService(t)
	if _, err := svc.Create(context.Background(), "tenant-a", "vault-auth", "generic", map[string]string{"token": "gw-token"}, "u"); err != nil {
		t.Fatalf("create auth credential: %v", err)
	}
	rs := newResolverServer(t, "gw-token", "secret-v1")
	return NewExternalServiceProvider(svc.Fetcher(), 2*time.Second, 5*time.Minute), rs, svc
}

func sourceFor(rs *resolverServer, tenant string) *headers.Source {
	return &headers.Source{
		ID: "src-1", TenantID: tenant, Name: "vault-a", Provider: ExternalServiceProviderID, Enabled: true,
		Config: map[string]any{"url": rs.URL, "auth_credential": "vault-auth"},
	}
}

func resolveCtx(tenant string, src *headers.Source) context.Context {
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "key-1", TenantID: tenant})
	return headers.WithResolveInfo(ctx, headers.ResolveInfo{ConnectorSlug: "splunk", HeaderName: "Authorization", Source: src})
}

func TestExternalService_ResolveSendsContractAndCaches(t *testing.T) {
	p, rs, _ := newExternalServiceFixture(t)
	cfg := map[string]any{"credential_id": "splunk-prod", "field": "token", "prefix": "Splunk "}
	if err := p.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	ctx := resolveCtx("tenant-a", sourceFor(rs, "tenant-a"))

	v, err := p.Resolve(ctx, cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v != "Splunk secret-v1" {
		t.Errorf("Resolve = %q, want %q", v, "Splunk secret-v1")
	}
	want := map[string]string{
		"tenant_id": "tenant-a", "connector_slug": "splunk", "header_name": "Authorization",
		"credential_id": "splunk-prod", "field": "token",
	}
	got := rs.lastRequest()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("request %s = %q, want %q", k, got[k], w)
		}
	}

	// Cached: a changed value at the service is not seen until invalidated.
	rs.set("secret-v2", 0, http.StatusOK)
	if v, _ := p.Resolve(ctx, cfg); v != "Splunk secret-v1" {
		t.Errorf("second Resolve = %q, want the cached value", v)
	}
	if n := rs.resolves.Load(); n != 1 {
		t.Errorf("resolve calls = %d, want 1", n)
	}

	p.Invalidate(ctx, "key-1", "splunk")
	if v, _ := p.Resolve(ctx, cfg); v != "Splunk secret-v2" {
		t.Errorf("Resolve after Invalidate = %q, want the new value", v)
	}
	if n := rs.resolves.Load(); n != 2 {
		t.Errorf("resolve calls = %d, want 2", n)
	}
}

func TestExternalService_TTLIsMinOfServiceAndConfig(t *testing.T) {
	p, rs, _ := newExternalServiceFixture(t)
	now := time.Now()
	p.now = func() time.Time { return now }
	cfg := map[string]any{"credential_id": "c", "field": "f"}
	ctx := resolveCtx("tenant-a", sourceFor(rs, "tenant-a"))

	rs.set("short", 30, http.StatusOK) // shorter than cache_ttl (5m)
	if _, err := p.Resolve(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now = now.Add(29 * time.Second)
	_, _ = p.Resolve(ctx, cfg)
	if n := rs.resolves.Load(); n != 1 {
		t.Fatalf("resolve calls before ttl_seconds = %d, want 1", n)
	}
	now = now.Add(2 * time.Second)
	_, _ = p.Resolve(ctx, cfg)
	if n := rs.resolves.Load(); n != 2 {
		t.Fatalf("resolve calls after ttl_seconds = %d, want 2", n)
	}

	// A ttl_seconds longer than cache_ttl is capped at cache_ttl.
	p.InvalidateTenant("tenant-a")
	rs.set("long", 3600, http.StatusOK)
	_, _ = p.Resolve(ctx, cfg)
	now = now.Add(5*time.Minute + time.Second)
	_, _ = p.Resolve(ctx, cfg)
	if n := rs.resolves.Load(); n != 4 {
		t.Fatalf("resolve calls after cache_ttl = %d, want 4", n)
	}
}

func TestExternalService_InvalidateIsPerTenant(t *testing.T) {
	p, rs, svc := newExternalServiceFixture(t)
	if _, err := svc.Create(context.Background(), "tenant-b", "vault-auth", "generic", map[string]string{"token": "gw-token"}, "u"); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"credential_id": "c", "field": "f"}
	ctxA := resolveCtx("tenant-a", sourceFor(rs, "tenant-a"))
	ctxB := resolveCtx("tenant-b", sourceFor(rs, "tenant-b"))
	_, _ = p.Resolve(ctxA, cfg)
	_, _ = p.Resolve(ctxB, cfg)
	if n := rs.resolves.Load(); n != 2 {
		t.Fatalf("resolve calls = %d, want 2 (one per tenant)", n)
	}
	p.Invalidate(ctxA, "key-1", "splunk")
	_, _ = p.Resolve(ctxB, cfg)
	if n := rs.resolves.Load(); n != 2 {
		t.Errorf("tenant-b refetched after tenant-a's invalidate")
	}
	_, _ = p.Resolve(ctxA, cfg)
	if n := rs.resolves.Load(); n != 3 {
		t.Errorf("tenant-a not refetched after its invalidate")
	}
}

func TestExternalService_ResolveErrors(t *testing.T) {
	p, rs, _ := newExternalServiceFixture(t)
	cfg := map[string]any{"credential_id": "c", "field": "f"}

	t.Run("no source", func(t *testing.T) {
		if _, err := p.Resolve(resolveCtx("tenant-a", nil), cfg); err == nil || !strings.Contains(err.Error(), "source") {
			t.Errorf("err = %v, want a missing-source error", err)
		}
	})
	t.Run("no principal", func(t *testing.T) {
		ctx := headers.WithResolveInfo(context.Background(), headers.ResolveInfo{Source: sourceFor(rs, "tenant-a")})
		if _, err := p.Resolve(ctx, cfg); err == nil {
			t.Error("Resolve without a principal succeeded")
		}
	})
	t.Run("source of another tenant", func(t *testing.T) {
		if _, err := p.Resolve(resolveCtx("tenant-a", sourceFor(rs, "tenant-b")), cfg); err == nil {
			t.Error("Resolve with another tenant's source succeeded")
		}
	})
	t.Run("auth credential missing in caller's tenant", func(t *testing.T) {
		if _, err := p.Resolve(resolveCtx("tenant-c", sourceFor(rs, "tenant-c")), cfg); err == nil {
			t.Error("Resolve with no auth credential succeeded")
		}
		if n := rs.resolves.Load(); n != 0 {
			t.Errorf("service called %d times without a token", n)
		}
	})
	t.Run("non-2xx", func(t *testing.T) {
		rs.set("x", 0, http.StatusForbidden)
		_, err := p.Resolve(resolveCtx("tenant-a", sourceFor(rs, "tenant-a")), cfg)
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Errorf("err = %v, want HTTP 403", err)
		}
	})
	t.Run("empty value", func(t *testing.T) {
		rs.set("", 0, http.StatusOK)
		if _, err := p.Resolve(resolveCtx("tenant-a", sourceFor(rs, "tenant-a")), cfg); err == nil {
			t.Error("Resolve accepted an empty value")
		}
	})
}

func TestExternalService_Validate(t *testing.T) {
	p := NewExternalServiceProvider(nil, time.Second, time.Minute)
	for _, tc := range []struct {
		cfg     map[string]any
		wantErr bool
	}{
		{map[string]any{"credential_id": "a", "field": "b"}, false},
		{map[string]any{"credential_id": "a", "field": "b", "prefix": "Bearer "}, false},
		{map[string]any{"field": "b"}, true},
		{map[string]any{"credential_id": "a"}, true},
		{map[string]any{"credential_id": "a", "field": "b", "prefix": "x\r\nEvil: 1"}, true},
	} {
		if err := p.Validate(tc.cfg); (err != nil) != tc.wantErr {
			t.Errorf("Validate(%v) = %v, wantErr %v", tc.cfg, err, tc.wantErr)
		}
	}
}

func TestExternalService_ValidateSourceURL(t *testing.T) {
	p := NewExternalServiceProvider(nil, time.Second, time.Minute)
	src := func(u string) map[string]any { return map[string]any{"url": u, "auth_credential": "a"} }

	ok := []string{
		"https://creds.example.com",
		"https://creds.example.com/base/",
		"http://127.0.0.1:8200", // loopback, allowed by this test binary's egress policy
		"http://localhost:8200",
	}
	for _, u := range ok {
		if err := p.ValidateSource(src(u)); err != nil {
			t.Errorf("ValidateSource(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{
		"",
		"http://creds.example.com", // plain http to a public host
		"http://8.8.8.8",
		"ftp://creds.example.com",
		"https://user:pw@creds.example.com",
		"https://creds.example.com/?x=1",
		"https:///nohost",
	}
	for _, u := range bad {
		if err := p.ValidateSource(src(u)); err == nil {
			t.Errorf("ValidateSource(%q) = nil, want an error", u)
		}
	}
	if err := p.ValidateSource(map[string]any{"url": "https://creds.example.com"}); err == nil {
		t.Error("ValidateSource without auth_credential = nil, want an error")
	}

	// Under the default egress policy loopback is blocked, so neither
	// plain http nor https may target it.
	strict, err := netguard.NewPolicy(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	prev := netguard.Current()
	netguard.SetPolicy(strict)
	defer netguard.SetPolicy(prev)
	for _, u := range []string{"http://127.0.0.1:8200", "http://10.0.0.5", "https://127.0.0.1"} {
		if err := p.ValidateSource(src(u)); err == nil {
			t.Errorf("ValidateSource(%q) under the default policy = nil, want an error", u)
		}
	}
}

func TestExternalService_TestSource(t *testing.T) {
	p, rs, _ := newExternalServiceFixture(t)
	src := *sourceFor(rs, "tenant-a")
	if err := p.TestSource(context.Background(), src); err != nil {
		t.Fatalf("TestSource: %v", err)
	}
	if rs.lastQS != "field=credential_id&tenant_id=tenant-a" {
		t.Errorf("options query = %q", rs.lastQS)
	}
	if rs.resolves.Load() != 0 {
		t.Error("TestSource resolved a secret")
	}

	rs.mu.Lock()
	rs.token = "other-token" // the service no longer accepts the gateway's token
	rs.mu.Unlock()
	if err := p.TestSource(context.Background(), src); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("TestSource with a rejected token = %v, want HTTP 401", err)
	}
}
